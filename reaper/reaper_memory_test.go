package reaper

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serveTestFeed(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>http://example.com</link>
    <description>A test feed</description>
    <item><title>Post One</title><link>http://example.com/1</link><pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate></item>
    <item><title>Post Two</title><link>http://example.com/2</link><pubDate>Tue, 03 Jan 2006 15:04:05 GMT</pubDate></item>
  </channel>
</rss>`)
	}))
}

func TestFetchDoesNotRetainItemsInMemory(t *testing.T) {
	srv := serveTestFeed(t)
	defer srv.Close()

	db := createNewTestDB()
	db.WriteFeed(srv.URL)
	r := New(db)

	// The caller gets the fully parsed feed for immediate use...
	feed, err := r.Fetch(srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if len(feed.Items) != 2 {
		t.Fatalf("expected 2 items in returned feed, got %d", len(feed.Items))
	}

	// ...while the reaper itself tracks only the last-fetch time
	if !r.HasFeed(srv.URL) {
		t.Fatal("expected feed to be tracked by reaper")
	}

	// feed metadata must be persisted in the database
	meta := db.GetFeedMetadata(srv.URL)
	if meta == nil {
		t.Fatal("expected feed metadata in db")
	}
	if meta.Title != "Test Feed" {
		t.Errorf("expected metadata title %q, got %q", "Test Feed", meta.Title)
	}
	if meta.Description != "A test feed" {
		t.Errorf("expected metadata description %q, got %q", "A test feed", meta.Description)
	}
	if titles := db.GetFeedTitles([]string{srv.URL}); titles[srv.URL] != "Test Feed" {
		t.Errorf("expected GetFeedTitles to return %q, got %v", "Test Feed", titles)
	}

	// items should land in the database once the batch saver flushes
	time.Sleep(6 * time.Second)
	if posts := db.GetPostsForFeed(srv.URL); len(posts) != 2 {
		t.Fatalf("expected 2 posts in db, got %d", len(posts))
	}

	// re-fetching the same feed must not duplicate posts in the db
	if _, err := r.Fetch(srv.URL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	if posts := db.GetPostsForFeed(srv.URL); len(posts) != 2 {
		t.Fatalf("expected db dedupe to keep 2 posts, got %d", len(posts))
	}
}
