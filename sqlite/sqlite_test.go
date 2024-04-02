package sqlite

import (
	"os"
	"testing"
	"time"
)

func createNewTestDB() *DB {
	// remove old db if it exists
	os.Remove("sqlite_go_test.db")

	db := New("sqlite_go_test.db")
	return db
}

func TestPostsOps(t *testing.T) {
	db := createNewTestDB()

	testPost := &Post{
		Title:             "Test Post",
		URL:               "https://example.com",
		PublishedDatetime: time.Now(),
	}

	const testFeedUrl = "http://example-feed.com"
	db.WriteFeed(testFeedUrl)

	// create posts
	db.SavePostStruct(testFeedUrl, testPost)
	db.SavePost(testFeedUrl, "Test Post 2", "https://example.com/2", time.Now())

	latest := db.GetLatestPostsForGlobal(10)
	if len(latest) != 2 {
		t.Errorf("Expected 2 posts, got %d", len(latest))
	}

	if latest[0].Title != "Test Post 2" {
		t.Errorf("Expected first post to be Test Post 2, got %s", latest[0].Title)
	}

	db.AddUser("testuser", "testpass")
	db.Subscribe("testuser", testFeedUrl)

	posts := db.GetPostsForUser("testuser", 100)
	if len(posts) != 2 {
		t.Errorf("Expected 2 posts, got %d", len(posts))
	}
}

func TestReadStatus(t *testing.T) {
	db := createNewTestDB()

	const testFeedUrl = "http://example-feed.com"
	db.WriteFeed(testFeedUrl)
	db.AddUser("testuser", "testpass")
	db.Subscribe("testuser", testFeedUrl)

	testPost := &Post{
		Title:             "Test Post",
		URL:               "https://example.com",
		PublishedDatetime: time.Now(),
	}

	db.SavePostStruct(testFeedUrl, testPost)

	if db.GetReadStatus("testuser", testPost.URL) {
		t.Errorf("Expected post to be unread")
	}

	db.SetReadStatus("testuser", testPost.URL, true)

	if !db.GetReadStatus("testuser", testPost.URL) {
		t.Errorf("Expected post to be read")
	}

	db.ToggleReadStatus("testuser", testPost.URL)

	if db.GetReadStatus("testuser", testPost.URL) {
		t.Errorf("Expected post to be unread")
	}
}
