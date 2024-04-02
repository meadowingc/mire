package reaper

import (
	"os"
	"testing"
	"time"

	"codeberg.org/meadowingc/mire/sqlite"
)

func createNewTestDB() *sqlite.DB {
	// remove old db if it exists
	os.Remove("reaper_go_test.db")

	db := sqlite.New("reaper_go_test.db")
	return db
}

func TestHasFeed(t *testing.T) {
	db := createNewTestDB()
	r := New(db)

	r.Fetch("https://visakanv.substack.com/feed")
	r.Fetch("https://meadow.bearblog.dev/feed")

	if r.HasFeed("banana") == true {
		t.Fatal("reaper should not have a banana")
	}
	if r.HasFeed("https://meadow.bearblog.dev/feed") == false {
		t.Fatal("reaper should have meadow.bearblog.dev")
	}
	if r.HasFeed("https://visakanv.substack.com/feed") == false {
		t.Fatal("reaper should have visakanv.substack.com")
	}
}

func TestNewPostsGetAddedToDatabase(t *testing.T) {
	db := createNewTestDB()
	db.WriteFeed("https://meadow.bearblog.dev/feed/")

	r := New(db)

	time.Sleep(1 * time.Second)

	r.Fetch("https://meadow.bearblog.dev/feed")

	time.Sleep(11 * time.Second) // 11 to account for the saver delay

	if len(db.GetLatestPostsForGlobal(10)) == 0 {
		t.Fatal("expected 3 posts in db")
	}
}
