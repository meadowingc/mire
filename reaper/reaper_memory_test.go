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

	// ...but the reaper itself must only keep metadata
	tracked := r.GetFeed(srv.URL)
	if tracked == nil {
		t.Fatal("expected feed to be tracked by reaper")
	}
	if len(tracked.Items) != 0 {
		t.Fatalf("expected no items retained in reaper memory, got %d", len(tracked.Items))
	}
	if tracked.Title != "Test Feed" {
		t.Errorf("expected metadata title %q, got %q", "Test Feed", tracked.Title)
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
