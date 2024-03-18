package reaper

import (
	"os"
	"testing"
	"time"

	"codeberg.org/meadowingc/mire/rss"
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
	f1 := rss.Feed{UpdateURL: "something"}
	f2 := rss.Feed{UpdateURL: "strange"}
	r.addFeed(&f1)
	r.addFeed(&f2)
	if r.HasFeed("banana") == true {
		t.Fatal("reaper should not have a banana")
	}
	if r.HasFeed("something") == false {
		t.Fatal("reaper should have something")
	}
	if r.HasFeed("strange") == false {
		t.Fatal("reaper should have strange")
	}
}

func TestNewPostsGetAddedToDatabase(t *testing.T) {
	db := createNewTestDB()
	db.WriteFeed("https://meadow.bearblog.dev/feed/")

	r := New(db)

	time.Sleep(1 * time.Second)

	f1 := rss.Feed{UpdateURL: "https://meadow.bearblog.dev/feed/"}
	r.addFeed(&f1)

	time.Sleep(11 * time.Second) // 11 to account for the saver delay

	if len(db.GetLatestPosts(10)) == 0 {
		t.Fatal("expected 3 posts in db")
	}
}
