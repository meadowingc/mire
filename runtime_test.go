package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeConfigurationRequiresExistingManagedDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	t.Setenv("MIRE_DATABASE", missing)
	t.Setenv("MIRE_REQUIRE_DATABASE", "true")
	if _, err := loadRuntimeConfig(); err == nil {
		t.Fatal("missing managed database was accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("configuration validation created a database")
	}
}

func TestRuntimeConfigurationRejectsInvalidValues(t *testing.T) {
	for name, value := range map[string]string{
		"MIRE_LISTEN_ADDR": "not-an-address", "MIRE_DATABASE": "",
		"MIRE_FILES_DIR": "", "MIRE_BACKGROUND": "maybe", "MIRE_REQUIRE_DATABASE": "maybe",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := loadRuntimeConfig(); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestRuntimeDatabaseURIQuotesFilename(t *testing.T) {
	config := defaultRuntimeConfig()
	config.database = filepath.Join(t.TempDir(), "db ?#&.sqlite")
	config.requireDatabase = true
	dsn, err := url.Parse(config.databaseDSN())
	if err != nil {
		t.Fatal(err)
	}
	if dsn.Path != config.database || dsn.Query().Get("mode") != "rw" || len(dsn.Query()["_pragma"]) != 4 {
		t.Fatalf("database filename/options were not preserved: %s", dsn)
	}
}

func startRuntime(t *testing.T, config runtimeConfig) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config.address = listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, config) }()
	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runtime shutdown failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not finish shutdown")
		}
	}
	t.Cleanup(stop)
	origin := "http://" + config.address
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(origin + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Mire-Revision") == buildRevision {
				return origin, stop
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime did not become healthy")
	return "", stop
}

func TestRuntimeRestartPreservesAccountsAndSessions(t *testing.T) {
	config := defaultRuntimeConfig()
	config.database = filepath.Join(t.TempDir(), "mire.db")
	config.background = false
	origin, stop := startRuntime(t, config)
	client := &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(origin+"/register", url.Values{"username": {"restart-reader"}, "password": {"test-password"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatal("registration failed")
	}
	cookie := resp.Cookies()[0]
	stop()
	config.requireDatabase = true
	origin, _ = startRuntime(t, config)
	request, _ := http.NewRequest(http.MethodGet, origin+"/", nil)
	request.AddCookie(cookie)
	resp, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(resp.Header.Get("Location"), "/restart-reader") {
		t.Fatal("session/account did not survive a graceful restart")
	}
}
