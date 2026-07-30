package sqlite

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"github.com/mmcdole/gofeed"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	sql *sql.DB

	// in-memory caches for id lookups that never change once created.
	// usernames are immutable and feeds are only deleted by
	// DeleteOrphanFeeds (which invalidates the cache).
	cacheMu sync.RWMutex
	userIDs map[string]int
	feedIDs map[string]int
}

type Post struct {
	Title             string
	URL               string
	FeedURL           string
	PublishedDatetime time.Time
}

type UserPostEntry struct {
	Post    *gofeed.Item
	IsRead  bool
	FeedURL string
}

var listOfSpammyFeeds = []string{
	".forumcommunity.net",
	".forumfree.it",
	".howtogeek.com",
	".isaechia.it",
	".tumblr.com",
	"//fs.blog",
	"//go.dev",
	"//om.co",
	"404media.co",
	"aeon.co",
	"aftermath.site",
	"anchor.fm",
	"appaddict.app",
	"arktosjournal.com",
	"arstechnica.com",
	"astralcodexten.com",
	"babylonbee.com",
	"basementcommunity.com",
	"beautybay.com",
	"biccy.it",
	"birchtree.me",
	"blog.flickr.net",
	"bloomberg.com",
	"canvastique.com",
	"citationneeded.news",
	"codeberg.org",
	"copykat.com",
	"crimethinc.com",
	"css-tip.com",
	"css-tricks.com",
	"daringfireball.net",
	"datenschutzverein.de",
	"defector.com",
	"digitalcourage.de",
	"digitalrechte.de",
	"f-droid.org",
	"facebook.com",
	"feedbin.com",
	"feedburner.com",
	"fetchrss.com",
	"finshots.in",
	"finshots.in",
	"frame.work",
	"frontendmasters.com",
	"fsfe.org",
	"ghacks.net",
	"google.com",
	"granary.io",
	"grunge.com",
	"home-designing.com",
	"ikeahackers.net",
	"infosec.exchange",
	"internetstealsanddeals.net",
	"introvertspring.com",
	"iphonelife.com",
	"irgendwiejuedisch.com",
	"joinmastodon.org",
	"jw-cdn.org",
	"jw.org",
	"kagifeedback.org",
	"kill-the-newsletter.com",
	"lemonde.fr",
	"librarystack.org",
	"libsyn.com",
	"lifehacker.com",
	"longreads.com",
	"macstories.net",
	"makeuseof.com",
	"manualdousuario.net",
	"marlybird.com",
	"mcsweeneys.net",
	"merriam-webster.com",
	"Millo.co",
	"mooglyblog.com",
	"namecoin.org",
	"nautil.us",
	"navigaweb.net",
	"nesslabs.com",
	"newsletter.pragmaticengineer.com",
	"notthebee.com",
	"nowkalamazoo.org",
	"obsidianstats.com",
	"ohohdeco.com",
	"omny.fm",
	"omnycontent.com",
	"on.substack.com",
	"ootdfinds.com",
	"openmentions.com",
	"pewresearch.org",
	"piccalil.li",
	"pluralistic.net",
	"producthunt.com",
	"psyche.co",
	"publicdomainreview.org",
	"reddit.com",
	"reductress.com",
	"refactoring.fm",
	"sapo.pt",
	"sapo.pt",
	"scotthyoung.com",
	"sentry.io",
	"sidebar.io",
	"simplecast.com",
	"slashdot.org",
	"socialwebfoundation.org",
	"stackexchange.com",
	"status.cafe",
	"talk.tiddlywiki.org",
	"technologyreview.com",
	"tfm.fan",
	"thecrochetcrowd.com",
	"themagicalslowcooker.com",
	"themorningnews.org",
	"theonion.com",
	"theringer.com",
	"thisiscolossal.com",
	"treknews.net",
	"twitch.tv",
	"utoronto.ca",
	"vivaldi.com",
	"vox.com",
	"web.hypothes.is",
	"webtoons.com",
	"welloptimum.com",
	"wolnelektury.pl",
	"youtube.com",
}

// Known feed aggregator domains that should be filtered by feed URL, not post URL
var knownFeedAggregators = []string{
	"daringfireball.net",
	"feedbin.com",
	"feedburner.com",
	"feedle.world",
	"feedproxy.google.com",
	"feeds.feedburner.com",
	"frontendmasters.com",
	"granary.io",
	"kill-the-newsletter.com",
	"libsyn.com",
	"sidebar.io",
	"simplecast.com",
}

var mutex = make(chan struct{}, 1)

// New opens a sqlite database, populates it with tables, and
// returns a ready-to-use *sqlite.DB object which is used for
// abstracting database queries.
func New(path string) *DB {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)")
	if err != nil {
		log.Fatal(err)
	}

	var latestVersion int
	row := db.QueryRow("SELECT MAX(version) FROM schema_migrations")
	err = row.Scan(&latestVersion)
	if err != nil {
		if strings.Contains(err.Error(), "converting NULL to int is unsupported") {
			// assume that we're starting from ground zero
			latestVersion = 0
		} else {
			log.Fatal(err)
		}
	}

	files, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range files {
		var version int
		_, err = fmt.Sscanf(f.Name(), "%d_", &version)
		if err != nil {
			log.Fatal(err)
		}

		// Apply migration if not already applied
		if version > latestVersion {
			fileData, _ := fs.ReadFile(migrationFiles, "migrations/"+f.Name())
			_, err := db.Exec(string(fileData))
			if err != nil {
				log.Fatalf("Failed to apply migration %s: %v", f.Name(), err)
			}
			_, err = db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version)
			if err != nil {
				log.Fatalf("Failed to record migration version %d: %v", version, err)
			}
			fmt.Printf("Applied migration %s\n", f.Name())
		}
	}

	// open up mutex (non-blocking: the token is already there if this is
	// not the first DB opened in this process)
	select {
	case mutex <- struct{}{}:
	default:
	}

	return &DB{
		sql:     db,
		userIDs: make(map[string]int),
		feedIDs: make(map[string]int),
	}
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) TryParseDate(dateStr string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC822,
		time.RFC822Z,
		time.RFC850,
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		// custom formats
		"Mon Jan 2 03:04:05 PM MST 2006",
		"2006-01-02 15:04:05-07:00",
	}

	for _, layout := range formats {
		date, err := time.Parse(layout, dateStr)
		if err == nil {
			return date, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse date: %s", dateStr)
}

func lock() {
	<-mutex
}

func unlock() {
	mutex <- struct{}{}
}

func (db *DB) GetUsernameBySessionToken(token string) string {
	var username string

	err := db.sql.QueryRow("SELECT username FROM user WHERE session_token=?", token).Scan(&username)

	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		log.Fatal(err)
	}

	return username
}

func (db *DB) GetPassword(username string) string {
	var password string

	err := db.sql.QueryRow("SELECT password FROM user WHERE username=?", username).Scan(&password)

	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		log.Fatal(err)
	}
	return password
}

func (db *DB) GetSessionToken(username string) (string, error) {
	var result sql.NullString

	err := db.sql.QueryRow("SELECT session_token FROM user WHERE username=?", username).Scan(&result)

	if err == sql.ErrNoRows {
		return "", nil
	}
	return result.String, err
}

func (db *DB) SetSessionToken(username string, token string) error {
	lock()
	_, err := db.sql.Exec("UPDATE user SET session_token=? WHERE username=?", token, username)
	unlock()

	return err
}

func (db *DB) AddUser(username string, passwordHash string) error {
	lock()
	_, err := db.sql.Exec("INSERT INTO user (username, password) VALUES (?, ?)", username, passwordHash)
	unlock()

	return err
}

func (db *DB) Subscribe(username string, feedURL string) {
	uid := db.GetUserID(username)
	fid := db.GetFeedID(feedURL)

	// Default is_favorite to false when subscribing to a new feed
	lock()
	_, err := db.sql.Exec(`
		INSERT INTO subscribe (user_id, feed_id, is_favorite) VALUES (?, ?, ?)
		ON CONFLICT(user_id, feed_id) DO NOTHING`, uid, fid, false)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

// SetFeedFavoriteStatus toggles the favorite status of a feed for a user.
func (db *DB) SetFeedFavoriteStatus(username string, feedURL string, isFavorite bool) error {
	userId := db.GetUserID(username)
	feedId := db.GetFeedID(feedURL)

	lock()
	defer unlock()

	_, err := db.sql.Exec("UPDATE subscribe SET is_favorite=? WHERE user_id=? AND feed_id=?", isFavorite, userId, feedId)
	return err
}

// GetFavoriteUnreadPosts fetches unread posts from favorite feeds for a user.
func (db *DB) GetFavoriteUnreadPosts(username string, limit int) ([]*UserPostEntry, error) {
	userId := db.GetUserID(username)
	rows, err := db.sql.Query(`
		SELECT p.title, p.url, p.published_at, pr.has_read, f.url
		FROM post p
		JOIN feed f ON p.feed_id = f.id
		JOIN subscribe s ON f.id = s.feed_id
		LEFT JOIN post_read pr ON p.id = pr.post_id AND pr.user_id = ?
		WHERE s.user_id = ? AND s.is_favorite = 1 AND (pr.has_read IS NULL OR pr.has_read = 0)
		ORDER BY p.published_at ASC
		LIMIT ?`, userId, userId, limit)
	if err != nil {
		if err == sql.ErrNoRows {
			return []*UserPostEntry{}, nil
		} else {
			return nil, err
		}
	}
	defer rows.Close()

	var favoriteUnreadPosts []*UserPostEntry
	for rows.Next() {
		var entry UserPostEntry
		var p gofeed.Item
		var hasRead sql.NullBool
		var feedURL string
		err = rows.Scan(&p.Title, &p.Link, &p.PublishedParsed, &hasRead, &feedURL)
		if err != nil {
			return nil, err
		}

		entry.Post = &p
		entry.FeedURL = feedURL
		entry.IsRead = hasRead.Valid && hasRead.Bool // IsRead is true if hasRead is not NULL and is true

		favoriteUnreadPosts = append(favoriteUnreadPosts, &entry)
	}

	return favoriteUnreadPosts, nil
}

func (db *DB) UnsubscribeAll(username string) {
	userId := db.GetUserID(username)

	lock()
	_, err := db.sql.Exec("DELETE FROM subscribe WHERE user_id=?", userId)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) UserExists(username string) bool {
	var result string

	err := db.sql.QueryRow("SELECT username FROM user WHERE username=?", username).Scan(&result)

	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		log.Fatal(err)
	}
	return true
}

func (db *DB) GetAllFeedURLs() []string {
	rows, err := db.sql.Query("SELECT url FROM feed")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var url string
		err = rows.Scan(&url)
		if err != nil {
			log.Fatal(err)
		}
		urls = append(urls, url)
	}
	return urls
}

func (db *DB) GetNumSubscribersForFeed(feedUrl string) int {
	var count int
	query := `
SELECT COUNT(s.id) 
FROM subscribe s
JOIN feed f ON s.feed_id = f.id
WHERE f.url = ?
`
	err := db.sql.QueryRow(query, feedUrl).Scan(&count)
	if err != nil {
		log.Printf("Error getting number of subscribers for feed: %v", err)
		return 0
	}
	return count

}

func (db *DB) GetUserFeedURLs(username string) []string {
	uid := db.GetUserID(username)

	// this query returns sql rows representing the list of
	// rss feed urls the user is subscribed to
	rows, err := db.sql.Query(`
		SELECT f.url
		FROM feed f
		JOIN subscribe s ON f.id = s.feed_id
		JOIN user u ON s.user_id = u.id
		WHERE u.id = ?`, uid)
	if err == sql.ErrNoRows {
		return []string{}
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var url string
		err = rows.Scan(&url)
		if err != nil {
			log.Fatal(err)
		}
		urls = append(urls, url)
	}
	return urls
}

type FeedUrlForSettings struct {
	URL        string
	Error      string
	IsFavorite bool
}

func (db *DB) GetUserFeedURLsForSettings(username string) []FeedUrlForSettings {
	uid := db.GetUserID(username)

	rows, err := db.sql.Query(`
		SELECT f.url, f.fetch_error, s.is_favorite
		FROM feed f
		JOIN subscribe s ON f.id = s.feed_id
		JOIN user u ON s.user_id = u.id
		WHERE u.id = ?`, uid)
	if err == sql.ErrNoRows {
		return []FeedUrlForSettings{}
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var feedErrors []FeedUrlForSettings
	for rows.Next() {
		var feedError FeedUrlForSettings
		var fetchError sql.NullString
		var isFavorite sql.NullBool

		err = rows.Scan(&feedError.URL, &fetchError, &isFavorite)
		if err != nil {
			log.Fatal(err)
		}
		if fetchError.Valid {
			feedError.Error = fetchError.String
		}
		if isFavorite.Valid {
			feedError.IsFavorite = isFavorite.Bool
		}
		feedErrors = append(feedErrors, feedError)
	}
	return feedErrors
}

// DeleteOrphanedPostReads deletes all post_read entries for a given user if
// that user is not subscribed to the feed that the post belongs to.
func (db *DB) DeleteOrphanedPostReads(username string) {
	userId := db.GetUserID(username)

	lock()
	defer unlock()

	_, err := db.sql.Exec(`
        DELETE FROM post_read 
        WHERE user_id = ? AND post_id IN (
            SELECT post.id FROM post
            WHERE post.feed_id NOT IN (
                SELECT feed_id FROM subscribe WHERE user_id = ?
            )
        )`, userId, userId)

	if err != nil {
		log.Fatal(err)
	}
}

// DeleteOrphanFeeds deletes all feeds that are not subscribed to by any user,
// as well as all posts that belong to those feeds.
func (db *DB) DeleteOrphanFeeds() []string {
	lock()
	defer unlock()

	// Select the URLs of the orphan feeds (feeds that are not subscribed to by any user)
	rows, err := db.sql.Query(`
        SELECT url FROM feed
        WHERE id NOT IN (SELECT feed_id FROM subscribe)`)
	if err != nil {
		return []string{}
	}

	var orphanFeedUrls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			rows.Close()
			return orphanFeedUrls
		}
		orphanFeedUrls = append(orphanFeedUrls, url)
	}
	rows.Close()

	// Delete posts that belong to the orphan feeds (feeds that are not
	// subscribed to by any user)
	_, err = db.sql.Exec(`
		DELETE FROM post
		WHERE feed_id NOT IN (SELECT feed_id FROM subscribe)`)
	if err != nil {
		log.Fatal(err)
	}

	// Delete the orphan feeds (feeds that are not subscribed to by any user)
	_, err = db.sql.Exec(`
		DELETE FROM feed
		WHERE id NOT IN (SELECT feed_id FROM subscribe)`)
	if err != nil {
		log.Fatal(err)
	}

	// invalidate cached ids of feeds that no longer exist
	db.cacheMu.Lock()
	for _, url := range orphanFeedUrls {
		delete(db.feedIDs, url)
	}
	db.cacheMu.Unlock()

	return orphanFeedUrls
}

func (db *DB) GetUserID(username string) int {
	db.cacheMu.RLock()
	uid, ok := db.userIDs[username]
	db.cacheMu.RUnlock()
	if ok {
		return uid
	}

	err := db.sql.QueryRow("SELECT id FROM user WHERE username=?", username).Scan(&uid)

	if err != nil {
		log.Fatal(err)
	}

	db.cacheMu.Lock()
	db.userIDs[username] = uid
	db.cacheMu.Unlock()
	return uid
}

func (db *DB) GetFeedID(feedURL string) int {
	db.cacheMu.RLock()
	fid, ok := db.feedIDs[feedURL]
	db.cacheMu.RUnlock()
	if ok {
		return fid
	}

	err := db.sql.QueryRow("SELECT id FROM feed WHERE url=?", feedURL).Scan(&fid)

	if err == sql.ErrNoRows {
		// Feed doesn't exist, return 0 to indicate no feed found
		return 0
	}
	if err != nil {
		log.Fatal(err)
	}

	db.cacheMu.Lock()
	db.feedIDs[feedURL] = fid
	db.cacheMu.Unlock()
	return fid
}

// WriteFeed writes an rss feed to the database for permanent storage
// if the given feed already exists, WriteFeed does nothing.
func (db *DB) WriteFeed(url string) {
	lock()
	_, err := db.sql.Exec(`INSERT INTO feed(url) VALUES(?) ON CONFLICT(url) DO NOTHING`, url)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) SetFeedFetchError(url string, fetchErr string) error {
	lock()
	_, err := db.sql.Exec("UPDATE feed SET fetch_error=? WHERE url=?", fetchErr, url)
	unlock()

	if err != nil {
		return err
	}
	return nil
}

func (db *DB) GetFeedFetchError(url string) (string, error) {
	var result sql.NullString

	err := db.sql.QueryRow("SELECT fetch_error FROM feed WHERE url=?", url).Scan(&result)

	if err == sql.ErrNoRows {
		// Feed doesn't exist in database, return empty error
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if result.Valid {
		return result.String, nil
	}
	return "", nil
}

// UpdateFeedRefreshState records the outcome of a refresh attempt (timestamp
// + fetch error) in a single write.
func (db *DB) UpdateFeedRefreshState(feedURL string, fetchErr string, refreshedAt time.Time) {
	lock()
	_, err := db.sql.Exec("UPDATE feed SET last_refreshed=?, fetch_error=? WHERE url=?", refreshedAt.UTC(), fetchErr, feedURL)
	unlock()
	if err != nil {
		log.Printf("UpdateFeedRefreshState:: Error updating refresh state for feed %s: %v", feedURL, err)
	}
}

func (db *DB) SavePostStruct(feedUrl string, post *Post) {
	db.SavePost(feedUrl, post.Title, post.URL, post.PublishedDatetime)
}

func (db *DB) SavePost(feedUrl string, title string, url string, publishedDatetime time.Time) {
	feedId := db.GetFeedID(feedUrl)

	lock()
	_, err := db.sql.Exec(
		"INSERT INTO post (feed_id, title, url, published_at) VALUES (?, ?, ?, ?) ON CONFLICT(feed_id, url) DO NOTHING",
		feedId, title, url, publishedDatetime,
	)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

// SavePosts inserts multiple posts for the same feed in a single transaction,
// which is dramatically faster than one transaction per post.
func (db *DB) SavePosts(feedUrl string, posts []*Post) {
	if len(posts) == 0 {
		return
	}

	feedId := db.GetFeedID(feedUrl)
	if feedId == 0 {
		log.Printf("[err] SavePosts: unknown feed '%s', skipping %d posts\n", feedUrl, len(posts))
		return
	}

	lock()
	defer unlock()

	tx, err := db.sql.Begin()
	if err != nil {
		log.Printf("[err] SavePosts: could not begin transaction: %v\n", err)
		return
	}

	stmt, err := tx.Prepare("INSERT INTO post (feed_id, title, url, published_at) VALUES (?, ?, ?, ?) ON CONFLICT(feed_id, url) DO NOTHING")
	if err != nil {
		tx.Rollback()
		log.Printf("[err] SavePosts: could not prepare statement: %v\n", err)
		return
	}
	defer stmt.Close()

	for _, p := range posts {
		if _, err := stmt.Exec(feedId, p.Title, p.URL, p.PublishedDatetime); err != nil {
			tx.Rollback()
			log.Printf("[err] SavePosts: could not insert post '%s': %v\n", p.URL, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[err] SavePosts: could not commit transaction: %v\n", err)
	}
}

func (db *DB) GetPostId(postUrl string) int {
	var pid int

	err := db.sql.QueryRow("SELECT id FROM post WHERE url=?", postUrl).Scan(&pid)
	if err != nil {
		log.Fatal(err)
	}

	return pid
}

// isSpammyPostURL reports whether a post URL matches one of the domains in
// the spammy-feeds blocklist (equivalent to SQL `LIKE '%domain%'`).
func isSpammyPostURL(postURL string) bool {
	for _, domain := range listOfSpammyFeeds {
		if strings.Contains(postURL, domain) {
			return true
		}
	}
	return false
}

// isAggregatorFeedURL reports whether a feed URL belongs to a known feed
// aggregator domain.
func isAggregatorFeedURL(feedURL string) bool {
	for _, aggregator := range knownFeedAggregators {
		if strings.Contains(feedURL, aggregator) {
			return true
		}
	}
	return false
}

func (db *DB) GetLatestPostsForDiscover(limit int) []*Post {
	// Oversample the most recent posts and filter out spammy domains in Go.
	// Pushing ~140 `NOT LIKE '%…%'` conditions down to SQLite forced a scan of
	// the whole posts×feeds join; this query is backed by the published_at
	// index instead.
	oversampledLimit := limit * 5

	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, f.url
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        ORDER BY p.published_at DESC
        LIMIT ?`, oversampledLimit)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	posts := make([]*Post, 0, limit)
	seen := make(map[string]struct{})
	for rows.Next() && len(posts) < limit {
		var p Post
		err = rows.Scan(&p.Title, &p.URL, &p.PublishedDatetime, &p.FeedURL)
		if err != nil {
			log.Fatal(err)
		}

		// dedupe by post url (the same url may exist under multiple feeds)
		if _, ok := seen[p.URL]; ok {
			continue
		}
		if isSpammyPostURL(p.URL) || isAggregatorFeedURL(p.FeedURL) {
			continue
		}

		seen[p.URL] = struct{}{}
		posts = append(posts, &p)
	}
	return posts
}

func (db *DB) GetPostsForFeed(feedUrl string) []*Post {
	feedId := db.GetFeedID(feedUrl)

	// If feed doesn't exist, return empty list
	if feedId == 0 {
		return []*Post{}
	}

	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, f.url
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        WHERE feed_id=?`, feedId)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var posts []*Post
	for rows.Next() {
		var p Post
		err = rows.Scan(&p.Title, &p.URL, &p.PublishedDatetime, &p.FeedURL)
		if err != nil {
			log.Fatal(err)
		}
		posts = append(posts, &p)
	}
	return posts
}

func (db *DB) GetPostsForFeedWithReadStatus(feedUrl string, username string) []*UserPostEntry {
	uid := db.GetUserID(username)
	feedId := db.GetFeedID(feedUrl)

	// If feed doesn't exist, return empty list
	if feedId == 0 {
		return []*UserPostEntry{}
	}

	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, pr.has_read, f.url
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        LEFT JOIN post_read pr ON p.id = pr.post_id AND pr.user_id = ?
        WHERE p.feed_id = ?
        ORDER BY p.published_at DESC`, uid, feedId)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var userPostsEntries []*UserPostEntry
	for rows.Next() {
		var entry UserPostEntry
		var p gofeed.Item
		var hasRead sql.NullBool
		var feedURL string
		err = rows.Scan(&p.Title, &p.Link, &p.PublishedParsed, &hasRead, &feedURL)
		if err != nil {
			log.Fatal(err)
		}

		entry.Post = &p
		entry.FeedURL = feedURL
		entry.IsRead = hasRead.Valid && hasRead.Bool // IsRead is true if hasRead is not NULL and is true

		userPostsEntries = append(userPostsEntries, &entry)
	}

	return userPostsEntries
}

func (db *DB) GetPostsForUser(username string, limit int) []*UserPostEntry {
	uid := db.GetUserID(username)

	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, pr.has_read, f.url
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        JOIN subscribe s ON f.id = s.feed_id
        LEFT JOIN post_read pr ON p.id = pr.post_id AND pr.user_id = ?
        WHERE s.user_id = ?
        ORDER BY p.published_at DESC
        LIMIT ?`, uid, uid, limit)
	if err != nil {
		log.Fatal(err)
	}

	var userPostsEntries []*UserPostEntry
	for rows.Next() {
		var entry UserPostEntry
		var p gofeed.Item
		var hasRead sql.NullBool
		var feedURL string
		err = rows.Scan(&p.Title, &p.Link, &p.PublishedParsed, &hasRead, &feedURL)
		if err != nil {
			log.Fatal(err)
		}

		entry.Post = &p
		entry.FeedURL = feedURL
		entry.IsRead = hasRead.Valid && hasRead.Bool // IsRead is true if hasRead is not NULL and is true

		userPostsEntries = append(userPostsEntries, &entry)
	}

	rows.Close()

	return userPostsEntries
}

// SplitFeedData holds the per-feed data needed to render the split view:
// total/unread post counts and the candidate posts to display.
type SplitFeedData struct {
	TotalPosts  int
	UnreadCount int
	Posts       []*UserPostEntry
}

// GetPostsForSplitView returns, for every feed the user is subscribed to, the
// per-feed total/unread counts plus up to perFeedLimit recent posts (and any
// unread posts beyond that window, since the split view prefers showing
// unread). It runs two queries total instead of one query per feed.
func (db *DB) GetPostsForSplitView(username string, perFeedLimit int) map[string]*SplitFeedData {
	uid := db.GetUserID(username)
	result := make(map[string]*SplitFeedData)

	// per-feed total and unread counts
	countRows, err := db.sql.Query(`
		SELECT f.url, COUNT(*),
			COALESCE(SUM(CASE WHEN pr.has_read IS NULL OR pr.has_read = 0 THEN 1 ELSE 0 END), 0)
		FROM post p
		JOIN feed f ON p.feed_id = f.id
		JOIN subscribe s ON s.feed_id = p.feed_id AND s.user_id = ?
		LEFT JOIN post_read pr ON pr.post_id = p.id AND pr.user_id = ?
		GROUP BY p.feed_id`, uid, uid)
	if err != nil {
		log.Fatal(err)
	}
	for countRows.Next() {
		var feedURL string
		var data SplitFeedData
		if err := countRows.Scan(&feedURL, &data.TotalPosts, &data.UnreadCount); err != nil {
			log.Fatal(err)
		}
		result[feedURL] = &data
	}
	countRows.Close()

	// Per-feed candidates: the perFeedLimit most recent posts (rn_all), plus
	// up to perFeedLimit unread posts regardless of age (rn_state) — an old
	// unread post may rank beyond the recent window but still needs to be
	// displayable. Read posts needed to fill the window are always covered by
	// rn_all: if a feed has u < perFeedLimit unread posts, the newest
	// perFeedLimit-u read posts all rank within the first perFeedLimit rows.
	rows, err := db.sql.Query(`
		WITH ranked AS (
			SELECT
				f.url AS feed_url,
				p.title, p.url, p.published_at, pr.has_read,
				ROW_NUMBER() OVER (PARTITION BY p.feed_id ORDER BY p.published_at DESC) AS rn_all,
				ROW_NUMBER() OVER (
					PARTITION BY p.feed_id, CASE WHEN pr.has_read IS NULL OR pr.has_read = 0 THEN 0 ELSE 1 END
					ORDER BY p.published_at DESC
				) AS rn_state
			FROM post p
			JOIN feed f ON p.feed_id = f.id
			JOIN subscribe s ON s.feed_id = p.feed_id AND s.user_id = ?
			LEFT JOIN post_read pr ON pr.post_id = p.id AND pr.user_id = ?
		)
		SELECT feed_url, title, url, published_at, has_read
		FROM ranked
		WHERE rn_all <= ? OR (COALESCE(has_read, 0) = 0 AND rn_state <= ?)
		ORDER BY feed_url, rn_all`, uid, uid, perFeedLimit, perFeedLimit)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var feedURL string
		var p gofeed.Item
		var hasRead sql.NullBool
		if err := rows.Scan(&feedURL, &p.Title, &p.Link, &p.PublishedParsed, &hasRead); err != nil {
			log.Fatal(err)
		}

		data, ok := result[feedURL]
		if !ok {
			continue
		}
		data.Posts = append(data.Posts, &UserPostEntry{
			Post:    &p,
			IsRead:  hasRead.Valid && hasRead.Bool,
			FeedURL: feedURL,
		})
	}

	return result
}

func (db *DB) GetRandomPost() *Post {
	// Sample recent posts and pick one at random in Go, filtering out spammy
	// domains as we go. This replaces running ORDER BY RANDOM() with ~140
	// NOT LIKE filters over the whole post table. Two-step selection (random
	// feed, then random post) keeps every sampled feed at equal weight
	// regardless of post count.
	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, f.url
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        ORDER BY p.published_at DESC
        LIMIT 500`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	postsByFeed := make(map[string][]*Post)
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.Title, &p.URL, &p.PublishedDatetime, &p.FeedURL); err != nil {
			log.Fatal(err)
		}
		if isSpammyPostURL(p.URL) || isAggregatorFeedURL(p.FeedURL) {
			continue
		}
		postsByFeed[p.FeedURL] = append(postsByFeed[p.FeedURL], &p)
	}

	if len(postsByFeed) == 0 {
		log.Fatal("GetRandomPost: no posts to pick from")
	}

	feeds := make([][]*Post, 0, len(postsByFeed))
	for _, posts := range postsByFeed {
		feeds = append(feeds, posts)
	}
	chosenFeed := feeds[rand.Intn(len(feeds))]
	return chosenFeed[rand.Intn(len(chosenFeed))]
}

func (db *DB) SetReadStatus(username string, postUrl string, read bool) {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl)

	lock()
	_, err := db.sql.Exec(`
		INSERT INTO post_read (user_id, post_id, has_read) VALUES (?, ?, ?)
		ON CONFLICT(user_id, post_id) DO UPDATE SET has_read=excluded.has_read`,
		userId, postId, read)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) ToggleReadStatus(username string, postUrl string) {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl)

	var read bool
	err := db.sql.QueryRow("SELECT has_read FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&read)
	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}

	lock()
	_, err = db.sql.Exec(`
		INSERT INTO post_read (user_id, post_id, has_read) VALUES (?, ?, ?)
		ON CONFLICT(user_id, post_id) DO UPDATE SET has_read=excluded.has_read`,
		userId, postId, !read)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) GetReadStatus(username string, postUrl string) bool {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl)

	var read bool

	err := db.sql.QueryRow("SELECT has_read FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&read)

	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}
	return read
}

func (db *DB) GetGlobalNumReadPosts() int {
	var count int
	err := db.sql.QueryRow("SELECT COUNT(*) FROM post_read WHERE has_read=1").Scan(&count)

	if err != nil {
		log.Fatal(err)
	}
	return count
}

func (db *DB) GetGlobalNumUniqueFeeds() int {
	var count int
	err := db.sql.QueryRow("SELECT COUNT(DISTINCT feed_id) FROM subscribe").Scan(&count)

	if err != nil {
		log.Fatal(err)
	}
	return count
}

func (db *DB) GetGlobalNumUsers() int {
	var count int
	err := db.sql.QueryRow("SELECT COUNT(*) FROM user").Scan(&count)

	if err != nil {
		log.Fatal(err)
	}
	return count
}

func (db *DB) GetSingleUserPreference(userId int, preferenceName string) *string {
	var preferenceValue string

	query := `SELECT preference_value FROM user_preferences WHERE user_id = ? AND preference_name = ?`
	err := db.sql.QueryRow(query, userId, preferenceName).Scan(&preferenceValue)
	if err != nil {
		if err == sql.ErrNoRows {
			// Preference not found for this user
			return nil
		}
		log.Fatal("getGenericUserPreference:: QueryRow failed: ", err)
	}

	return &preferenceValue
}

func (db *DB) SaveSingleUserPreference(userId int, preferenceName, preferenceValue string) error {
	lock()
	_, err := db.sql.Exec(`
		INSERT INTO user_preferences (user_id, preference_name, preference_value) VALUES (?, ?, ?)
		ON CONFLICT(user_id, preference_name) DO UPDATE SET preference_value=excluded.preference_value`,
		userId, preferenceName, preferenceValue)
	unlock()
	if err != nil {
		log.Printf("SaveUserPreference:: Error saving user preference: %v", err)
	}
	return err
}

// GetAllUserPreferences fetches all preferences for a user in a single query,
// keyed by preference name.
func (db *DB) GetAllUserPreferences(userId int) map[string]string {
	prefs := make(map[string]string)

	rows, err := db.sql.Query("SELECT preference_name, preference_value FROM user_preferences WHERE user_id = ?", userId)
	if err != nil {
		log.Printf("GetAllUserPreferences:: Query failed: %v", err)
		return prefs
	}
	defer rows.Close()

	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			log.Printf("GetAllUserPreferences:: Scan failed: %v", err)
			return prefs
		}
		prefs[name] = value
	}
	return prefs
}

func (db *DB) GetFeedLastRefreshTime(feedURL string) time.Time {
	var lastRefreshed time.Time
	err := db.sql.QueryRow("SELECT last_refreshed FROM feed WHERE url=?", feedURL).Scan(&lastRefreshed)
	if err != nil {
		log.Printf("GetLastRefreshTime:: Error getting last refresh time for feed %s: %v", feedURL, err)
		return time.Time{} // Return zero time on error
	}
	return lastRefreshed
}

func (db *DB) UpdateFeedLastRefreshTime(feedURL string, lastRefreshed time.Time) {
	lock()
	_, err := db.sql.Exec("UPDATE feed SET last_refreshed=? WHERE url=?", lastRefreshed.UTC(), feedURL)
	unlock()
	if err != nil {
		log.Printf("UpdateLastRefreshTime:: Error updating last refresh time for feed %s: %v", feedURL, err)
	}
}

func (db *DB) UpdatePassword(username string, newPassword string) error {
	lock()
	_, err := db.sql.Exec("UPDATE user SET password=? WHERE username=?", newPassword, username)
	unlock()
	return err
}

// IsUserSubscribedToFeed checks if a user is subscribed to a specific feed
func (db *DB) IsUserSubscribedToFeed(username string, feedURL string) bool {
	userId := db.GetUserID(username)

	var count int
	err := db.sql.QueryRow(`
		SELECT COUNT(*) 
		FROM subscribe s
		JOIN feed f ON s.feed_id = f.id
		WHERE s.user_id = ? AND f.url = ?`, userId, feedURL).Scan(&count)

	if err != nil {
		log.Printf("Error checking if user is subscribed to feed: %v", err)
		return false
	}

	return count > 0
}

// IsFeedFavorite checks if a feed is marked favorite by the user.
func (db *DB) IsFeedFavorite(username string, feedURL string) bool {
	userId := db.GetUserID(username)
	feedId := db.GetFeedID(feedURL)
	if feedId == 0 {
		return false
	}

	var isFavorite bool
	err := db.sql.QueryRow(`SELECT is_favorite FROM subscribe WHERE user_id=? AND feed_id=?`, userId, feedId).Scan(&isFavorite)
	if err != nil {
		if err == sql.ErrNoRows {
			return false
		}
		log.Printf("Error checking if feed is favorite: %v", err)
		return false
	}
	return isFavorite
}

// Unsubscribe removes a user's subscription to a specific feed
func (db *DB) Unsubscribe(username string, feedURL string) error {
	userId := db.GetUserID(username)
	feedId := db.GetFeedID(feedURL)

	lock()
	_, err := db.sql.Exec("DELETE FROM subscribe WHERE user_id=? AND feed_id=?", userId, feedId)
	unlock()

	return err
}
