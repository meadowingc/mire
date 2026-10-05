package reaper

import (
	"context"
	"path/filepath"
	"testing"

	"codeberg.org/meadowingc/mire/sqlite"
)

func createNewTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db := sqlite.New(filepath.Join(t.TempDir(), "reaper.db"))
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHasFeed(t *testing.T) {
	db := createNewTestDB(t)
	r := NewWithPolling(db, false)
	defer r.Close(context.Background())
	first, second := serveTestFeed(t), serveTestFeed(t)
	defer first.Close()
	defer second.Close()
	db.WriteFeed(first.URL)
	db.WriteFeed(second.URL)

	r.Fetch(first.URL)
	r.Fetch(second.URL)

	if r.HasFeed("banana") == true {
		t.Fatal("reaper should not have a banana")
	}
	if r.HasFeed(first.URL) == false {
		t.Fatal("reaper should have the first feed")
	}
	if r.HasFeed(second.URL) == false {
		t.Fatal("reaper should have the second feed")
	}
}

func TestNewPostsGetAddedToDatabase(t *testing.T) {
	db := createNewTestDB(t)
	server := serveTestFeed(t)
	defer server.Close()
	db.WriteFeed(server.URL)
	r := NewWithPolling(db, false)
	if _, err := r.Fetch(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(db.GetPostsForFeed(server.URL)) != 2 {
		t.Fatal("expected both posts in db")
	}
}
