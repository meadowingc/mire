package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/meadowingc/mire/reaper"
	"codeberg.org/meadowingc/mire/sqlite"
	"github.com/go-chi/chi/v5"
)

// newTestServer builds a fully wired Site backed by a temporary database and
// returns an httptest server running the real router.
func newTestServer(t *testing.T) (*httptest.Server, *sqlite.DB) {
	t.Helper()

	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { db.Close() })

	s := &Site{
		title:  "mire",
		reaper: reaper.New(db),
		db:     db,
	}

	var router *chi.Mux = buildRouter(s)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return server, db
}

// sessionCookie registers a new user and returns its session cookie.
func sessionCookie(t *testing.T, server *httptest.Server, username string) *http.Cookie {
	t.Helper()

	// register responds with a redirect; don't follow it so we can capture
	// the Set-Cookie header from the redirect response
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.PostForm(server.URL+"/register", url.Values{
		"username": {username},
		"password": {"hunter2"},
	})
	if err != nil {
		t.Fatalf("register request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected redirect after register, got %d", resp.StatusCode)
	}

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "session_token" && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("expected a session_token cookie after register, got none")
	return nil
}

func get(t *testing.T, server *httptest.Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, req)
	return recorder
}

// seedFeedWithPosts creates a feed with posts and subscribes the user to it.
func seedFeedWithPosts(t *testing.T, db *sqlite.DB, username, feedURL string, posts []*sqlite.Post) {
	t.Helper()
	db.WriteFeed(feedURL)
	db.SavePosts(feedURL, posts)
	db.Subscribe(username, feedURL)
}

func TestE2ERegisterLoginAndHomePage(t *testing.T) {
	server, _ := newTestServer(t)
	cookie := sessionCookie(t, server, "alice")

	resp := get(t, server, "/u/alice", cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for user home page, got %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "alice") {
		t.Error("expected home page to contain the username")
	}

	// root should redirect a logged-in user to their home page
	resp = get(t, server, "/", cookie)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect from / for logged-in user, got %d", resp.Code)
	}
	if loc := resp.Header().Get("Location"); loc != "/u/alice" {
		t.Errorf("expected redirect to /u/alice, got %s", loc)
	}
}

func TestE2ESubscriptionTimelineAndReadStatus(t *testing.T) {
	server, db := newTestServer(t)
	cookie := sessionCookie(t, server, "bob")

	posts := []*sqlite.Post{
		{Title: "first post", URL: "https://blog.example.com/1", PublishedDatetime: time.Now().Add(-2 * time.Hour)},
		{Title: "second post", URL: "https://blog.example.com/2", PublishedDatetime: time.Now().Add(-1 * time.Hour)},
	}
	seedFeedWithPosts(t, db, "bob", "https://blog.example.com/feed", posts)

	// the user's timeline should render both posts
	resp := get(t, server, "/u/bob", cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for timeline, got %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "first post") || !strings.Contains(body, "second post") {
		t.Error("expected timeline to contain both posts")
	}

	// mark the first post as read through the API
	postURL := url.QueryEscape("https://blog.example.com/1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/set-post-read-status/"+postURL,
		strings.NewReader("new_has_read=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after setting read status, got %d", recorder.Code)
	}
	if !db.GetReadStatus("bob", "https://blog.example.com/1") {
		t.Error("expected post to be marked as read")
	}
	if db.GetReadStatus("bob", "https://blog.example.com/2") {
		t.Error("expected second post to remain unread")
	}
}

func TestE2ESubscribeAndUnsubscribeViaAPI(t *testing.T) {
	server, db := newTestServer(t)
	cookie := sessionCookie(t, server, "carol")
	feedURL := url.QueryEscape("https://carol.example.com/feed")

	// subscribe
	req := httptest.NewRequest(http.MethodPost, "/api/v1/toggle-subscription/"+feedURL,
		strings.NewReader("subscribe=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 after subscribing, got %d", recorder.Code)
	}
	if !db.IsUserSubscribedToFeed("carol", "https://carol.example.com/feed") {
		t.Error("expected user to be subscribed")
	}

	// settings page should list the feed
	resp := get(t, server, "/settings", cookie)
	if !strings.Contains(resp.Body.String(), "carol.example.com") {
		t.Error("expected settings page to list the subscribed feed")
	}

	// unsubscribe
	req = httptest.NewRequest(http.MethodPost, "/api/v1/toggle-subscription/"+feedURL,
		strings.NewReader("subscribe=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 after unsubscribing, got %d", recorder.Code)
	}
	if db.IsUserSubscribedToFeed("carol", "https://carol.example.com/feed") {
		t.Error("expected user to be unsubscribed")
	}
}

func TestE2EDiscoverPage(t *testing.T) {
	server, db := newTestServer(t)

	db.WriteFeed("https://disco.example.com/feed")
	db.SavePosts("https://disco.example.com/feed", []*sqlite.Post{
		{Title: "discoverable post", URL: "https://disco.example.com/1", PublishedDatetime: time.Now()},
		{Title: "spam post", URL: "https://www.youtube.com/watch?v=spam", PublishedDatetime: time.Now()},
	})

	resp := get(t, server, "/discover", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for discover page, got %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "discoverable post") {
		t.Error("expected discover page to show the regular post")
	}
	if strings.Contains(body, "spam post") {
		t.Error("expected discover page to filter out spammy domains")
	}

	// a second request should hit the cache and return the same content
	resp = get(t, server, "/discover", nil)
	if !strings.Contains(resp.Body.String(), "discoverable post") {
		t.Error("expected cached discover page to still show the post")
	}
}

func TestE2ESplitView(t *testing.T) {
	server, db := newTestServer(t)
	cookie := sessionCookie(t, server, "dave")

	seedFeedWithPosts(t, db, "dave", "https://split.example.com/feed", []*sqlite.Post{
		{Title: "split post one", URL: "https://split.example.com/1", PublishedDatetime: time.Now().Add(-1 * time.Hour)},
		{Title: "split post two", URL: "https://split.example.com/2", PublishedDatetime: time.Now()},
	})
	db.SetReadStatus("dave", "https://split.example.com/1", true)

	// title is served from the feed metadata in the database
	db.UpdateFeedMetadata("https://split.example.com/feed", "Daves Split Feed", "", "")

	resp := get(t, server, "/split", cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for split view, got %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "split post one") || !strings.Contains(body, "split post two") {
		t.Error("expected split view to show both posts")
	}
	// one read + one unread post → (1/2) in the title
	if !strings.Contains(body, "(1/2)") {
		t.Error("expected split view title to show 1 unread out of 2 posts")
	}
	if !strings.Contains(body, "Daves Split Feed") {
		t.Error("expected split view to show the feed title from the database")
	}
}

func TestE2EAuthGuards(t *testing.T) {
	server, _ := newTestServer(t)

	// settings requires a session
	resp := get(t, server, "/settings", nil)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for /settings without a session, got %d", resp.Code)
	}

	// split view redirects anonymous users to login
	resp = get(t, server, "/split", nil)
	if resp.Code != http.StatusSeeOther || resp.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login for anonymous /split, got %d", resp.Code)
	}

	// unknown users 404
	resp = get(t, server, "/u/nobody", nil)
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown user, got %d", resp.Code)
	}
}
