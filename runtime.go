package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var buildRevision = "development"

type runtimeConfig struct {
	address         string
	database        string
	files           string
	background      bool
	requireDatabase bool
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{address: ":5544", database: "mire.db", files: "files", background: true}
}

func loadRuntimeConfig() (runtimeConfig, error) {
	config := defaultRuntimeConfig()
	for name, target := range map[string]*string{
		"MIRE_LISTEN_ADDR": &config.address,
		"MIRE_DATABASE":    &config.database,
		"MIRE_FILES_DIR":   &config.files,
	} {
		if value, present := os.LookupEnv(name); present {
			if value == "" {
				return config, fmt.Errorf("%s must not be empty", name)
			}
			*target = value
		}
	}
	for name, target := range map[string]*bool{
		"MIRE_BACKGROUND":       &config.background,
		"MIRE_REQUIRE_DATABASE": &config.requireDatabase,
	} {
		if value, present := os.LookupEnv(name); present {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return config, fmt.Errorf("%s must be a boolean", name)
			}
			*target = parsed
		}
	}
	if _, _, err := net.SplitHostPort(config.address); err != nil {
		return config, fmt.Errorf("invalid MIRE_LISTEN_ADDR: %w", err)
	}
	if info, err := os.Stat(config.files); err != nil {
		return config, fmt.Errorf("open Mire files directory: %w", err)
	} else if !info.IsDir() {
		return config, errors.New("MIRE_FILES_DIR must be a directory")
	}
	if config.requireDatabase {
		if info, err := os.Stat(config.database); err != nil {
			return config, fmt.Errorf("required Mire database: %w", err)
		} else if !info.Mode().IsRegular() {
			return config, errors.New("MIRE_DATABASE must be a regular file")
		}
	}
	return config, nil
}

func (config runtimeConfig) databaseDSN() string {
	path, err := filepath.Abs(config.database)
	if err != nil {
		log.Fatalf("resolve database path: %v", err)
	}
	database := &url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	if config.requireDatabase {
		query.Set("mode", "rw")
	}
	for _, pragma := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)", "cache_size(-20000)"} {
		query.Add("_pragma", pragma)
	}
	database.RawQuery = query.Encode()
	return database.String()
}

func runServer(ctx context.Context, config runtimeConfig) error {
	listener, err := net.Listen("tcp", config.address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	s := newSite(config)
	server := &http.Server{
		Handler: buildRouter(s), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
	}
	statsContext, stopStats := context.WithCancel(context.Background())
	defer stopStats()
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		if config.background {
			statsCalculatorProcess(statsContext, s)
		}
	}()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	log.Printf("main: listening on %s (revision %s)", listener.Addr(), buildRevision)
	var serveError error
	select {
	case <-ctx.Done():
	case serveError = <-served:
		if errors.Is(serveError, http.ErrServerClosed) {
			serveError = nil
		}
	}
	log.Println("main: draining requests and background work...")
	stopStats()
	shutdown, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
		return fmt.Errorf("HTTP shutdown did not drain: %w", err)
	}
	if err := s.reaper.Close(shutdown); err != nil {
		return fmt.Errorf("feed worker shutdown: %w", err)
	}
	workersDone := make(chan struct{})
	go func() {
		s.workers.Wait()
		<-statsDone
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-shutdown.Done():
		return fmt.Errorf("background shutdown did not drain: %w", shutdown.Err())
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("database shutdown: %w", err)
	}
	log.Println("main: server gracefully stopped")
	return serveError
}
