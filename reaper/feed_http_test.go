package reaper

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/meadowingc/mire/sqlite"
)

const testRSS = `<?xml version="1.0"?><rss version="2.0"><channel><title>Example</title><link>https://example.com</link><description>Example feed</description></channel></rss>`

func isolatedReaper(db *sqlite.DB) *Reaper {
	select {
	case mutex <- struct{}{}:
	default:
	}
	return &Reaper{feeds: make(map[string]time.Time), saverChannel: make(chan *PostSaveRequest, 10), db: db}
}

func TestFreshnessDeadline(t *testing.T) {
	now := time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		headers http.Header
		want    time.Duration
	}{
		{"no hint", http.Header{}, 0},
		{"max-age", http.Header{"Cache-Control": {"max-age=86400"}}, 24 * time.Hour},
		{"shorter than default", http.Header{"Cache-Control": {"max-age=3600"}}, time.Hour},
		{"age", http.Header{"Cache-Control": {"max-age=86400"}, "Age": {"3600"}}, 23 * time.Hour},
		{"date elapsed", http.Header{"Cache-Control": {"max-age=86400"}, "Date": {now.Add(-time.Hour).Format(http.TimeFormat)}}, 23 * time.Hour},
		{"max-age beats Expires", http.Header{"Cache-Control": {"max-age=86400"}, "Expires": {now.Add(48 * time.Hour).Format(http.TimeFormat)}}, 24 * time.Hour},
		{"expires", http.Header{"Expires": {now.Add(24 * time.Hour).Format(http.TimeFormat)}}, 24 * time.Hour},
		{"expires and age", http.Header{"Date": {now.Format(http.TimeFormat)}, "Expires": {now.Add(24 * time.Hour).Format(http.TimeFormat)}, "Age": {"3600"}}, 23 * time.Hour},
		{"bad max-age falls back", http.Header{"Cache-Control": {"max-age=bad"}, "Expires": {now.Add(24 * time.Hour).Format(http.TimeFormat)}}, 24 * time.Hour},
		{"zero max-age beats Expires", http.Header{"Cache-Control": {"max-age=0"}, "Expires": {now.Add(24 * time.Hour).Format(http.TimeFormat)}}, 0},
		{"no-cache beats max-age", http.Header{"Cache-Control": {"no-cache, max-age=86400"}}, 0},
		{"no-store beats Expires", http.Header{"Cache-Control": {"no-store"}, "Expires": {now.Add(24 * time.Hour).Format(http.TimeFormat)}}, 0},
		{"invalid Expires", http.Header{"Expires": {"broken"}}, 0},
		{"long freshness uncapped", http.Header{"Cache-Control": {"max-age=2592000"}}, 30 * 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := freshnessDeadline(tt.headers, now)
			if tt.want == 0 {
				if !got.IsZero() {
					t.Fatalf("expected no freshness deadline, got %v", got)
				}
			} else if got.Sub(now) != tt.want {
				t.Fatalf("expected deadline after %s, got %s", tt.want, got.Sub(now))
			}
		})
	}
}

func TestFetchPersistsPollingDeadline(t *testing.T) {
	var requests atomic.Int32
	var agent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		agent.Store(req.UserAgent())
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	db.WriteFeed(srv.URL)
	r := isolatedReaper(db)
	before := time.Now()
	if _, err := r.Fetch(srv.URL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agent.Load().(string), "Mire (+https://mire.meadow.cafe) - 0 subscribers") {
		t.Fatalf("unexpected user agent: %q", agent.Load())
	}
	deadline := db.GetFeedPollDeadlines()[srv.URL]
	if deadline.Before(before.Add(24*time.Hour-time.Minute)) || deadline.After(time.Now().Add(24*time.Hour+time.Minute)) {
		t.Fatalf("unexpected persisted deadline: %s", deadline)
	}
	r.refreshAllFeeds()
	if got := requests.Load(); got != 1 {
		t.Fatalf("fresh feed fetched again: %d requests", got)
	}

	// Simulate release-mode startup without launching its background loop.
	restarted := isolatedReaper(db)
	restarted.loadFeedDeadlines()
	restarted.refreshAllFeeds()
	lock()
	restored := restarted.feeds[srv.URL]
	unlock()
	if !restored.Equal(deadline) {
		t.Fatalf("restart lost deadline: got %s, want %s", restored, deadline)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restart fetched fresh feed: %d requests", got)
	}
}

func TestScheduledFetchFallbackAndFailure(t *testing.T) {
	var requests atomic.Int32
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if status.Load() != 0 {
			http.Error(w, "temporary error", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	db.WriteFeed(srv.URL)
	r := isolatedReaper(db)
	r.feeds[srv.URL] = time.Time{}
	before := time.Now()
	r.refreshAllFeeds()
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected one fetch, got %d", got)
	}
	deadline := db.GetFeedPollDeadlines()[srv.URL]
	if deadline.Before(before.Add(3*time.Hour)) || deadline.After(time.Now().Add(3*time.Hour+time.Minute)) {
		t.Fatalf("missing three-hour fallback: %s", deadline)
	}
	r.refreshAllFeeds()
	if got := requests.Load(); got != 1 {
		t.Fatalf("fetched before three-hour deadline: %d", got)
	}

	status.Store(1)
	lock()
	r.feeds[srv.URL] = time.Time{}
	unlock()
	r.refreshAllFeeds()
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected failed retry, got %d", got)
	}
	failedDeadline := db.GetFeedPollDeadlines()[srv.URL]
	if failedDeadline.Before(time.Now().Add(3*time.Hour-time.Minute)) ||
		failedDeadline.After(time.Now().Add(3*time.Hour+time.Minute)) {
		t.Fatalf("failure not scheduled for ordinary retry: %s", failedDeadline)
	}
}

func TestFetchUsesFinalRedirectResponseHeaders(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "max-age=86400")
		fmt.Fprint(w, testRSS)
	}))
	defer final.Close()
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "max-age=2592000")
		http.Redirect(w, req, final.URL, http.StatusFound)
	}))
	defer start.Close()

	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	db.WriteFeed(start.URL)
	r := isolatedReaper(db)
	if _, err := r.Fetch(start.URL); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(db.GetFeedPollDeadlines()[start.URL])
	if remaining < 23*time.Hour || remaining > 25*time.Hour {
		t.Fatalf("expected final response's 24-hour deadline, got %s", remaining)
	}
}

func TestPlainAtomSubtitle(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
 <title>Example</title><id>https://example.com/feed</id><updated>2026-09-24T00:00:00Z</updated>
 <subtitle>Important things <!-- -->hiding &amp; waiting; <!-- -->curated. &lt;!-- literal --&gt;</subtitle>
</feed>`)
	subtitle, found, err := plainAtomSubtitle(body)
	if err != nil || !found {
		t.Fatalf("could not decode subtitle: found=%t, err=%v", found, err)
	}
	want := "Important things hiding & waiting; curated. <!-- literal -->"
	if subtitle != want {
		t.Fatalf("got %q, want %q", subtitle, want)
	}
	for _, kind := range []string{"html", "xhtml"} {
		changed := strings.Replace(string(body), "<subtitle>", `<subtitle type="`+kind+`">`, 1)
		if _, found, err := plainAtomSubtitle([]byte(changed)); err != nil || found {
			t.Fatalf("should not change %s subtitle: found=%t, err=%v", kind, found, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, string(body))
	}))
	defer srv.Close()
	db := sqlite.New(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	db.WriteFeed(srv.URL)
	r := isolatedReaper(db)
	feed, err := r.Fetch(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if feed.Description != want || db.GetFeedMetadata(srv.URL).Description != want {
		t.Fatalf("incorrect parsed or stored subtitle: %q, %+v", feed.Description, db.GetFeedMetadata(srv.URL))
	}
}
