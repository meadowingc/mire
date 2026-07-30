package sqlite

import (
	"fmt"
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

	latest := db.GetLatestPostsForDiscover(10)
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

func TestSplitViewPosts(t *testing.T) {
	dbName := "sqlite_go_test_split.db"
	os.Remove(dbName)
	db := New(dbName)
	defer os.Remove(dbName)

	feedURL := "https://example.com/feed.xml"
	db.WriteFeed(feedURL)
	db.AddUser("splituser", "pw")
	db.Subscribe("splituser", feedURL)

	now := time.Now()
	// 15 newer READ posts fill the recent window entirely
	for i := 0; i < 15; i++ {
		p := &Post{
			Title:             fmt.Sprintf("read-%d", i),
			URL:               fmt.Sprintf("https://example.com/read-%d", i),
			PublishedDatetime: now.Add(-time.Duration(i) * time.Hour),
		}
		db.SavePostStruct(feedURL, p)
		db.SetReadStatus("splituser", p.URL, true)
	}
	// 3 older UNREAD posts rank beyond the 12 most recent overall
	for i := 0; i < 3; i++ {
		p := &Post{
			Title:             fmt.Sprintf("unread-%d", i),
			URL:               fmt.Sprintf("https://example.com/unread-%d", i),
			PublishedDatetime: now.Add(-time.Duration(20+i) * time.Hour),
		}
		db.SavePostStruct(feedURL, p)
	}

	data := db.GetPostsForSplitView("splituser", 12)
	fd, ok := data[feedURL]
	if !ok {
		t.Fatalf("Expected feed %s in split view data", feedURL)
	}
	if fd.TotalPosts != 18 {
		t.Errorf("Expected TotalPosts=18, got %d", fd.TotalPosts)
	}
	if fd.UnreadCount != 3 {
		t.Errorf("Expected UnreadCount=3, got %d", fd.UnreadCount)
	}
	if len(fd.Posts) != 15 {
		t.Fatalf("Expected 15 posts (12 window + 3 old unread), got %d", len(fd.Posts))
	}
	unreadSeen := 0
	for _, p := range fd.Posts {
		if !p.IsRead {
			unreadSeen++
		}
	}
	if unreadSeen != 3 {
		t.Errorf("Expected all 3 old unread posts in result, got %d", unreadSeen)
	}
}
