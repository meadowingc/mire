package reaper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/meadowingc/mire/sqlite"
)

func TestCloseFlushesAcceptedPostsBeforeDatabaseCloses(t *testing.T) {
	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	server := serveTestFeed(t)
	defer server.Close()
	db.WriteFeed(server.URL)
	r := NewWithPolling(db, false)
	feed, err := r.Fetch(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if posts := db.GetPostsForFeed(server.URL); len(posts) != len(feed.Items) {
		t.Fatalf("close lost accepted posts: got %d, want %d", len(posts), len(feed.Items))
	}
	if _, err := r.Fetch(server.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch after close was accepted: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("repeated close failed: %v", err)
	}
}

func TestCloseCancelsInflightNetworkRequests(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	db.WriteFeed(server.URL)
	r := NewWithPolling(db, false)
	fetched := make(chan error, 1)
	go func() {
		_, err := r.Fetch(server.URL)
		fetched <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-fetched; !errors.Is(err, context.Canceled) {
		t.Fatalf("inflight request was not canceled: %v", err)
	}
	if deadline := db.GetFeedPollDeadlines()[server.URL]; !deadline.IsZero() {
		t.Fatal("shutdown changed a failed request's polling deadline")
	}
}
