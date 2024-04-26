package sqlite

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"github.com/mmcdole/gofeed"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	sql *sql.DB
}

type Post struct {
	Title             string
	URL               string
	PublishedDatetime time.Time
}

type UserPostEntry struct {
	Post   *gofeed.Item
	IsRead bool
}

var listOfSpammyFeeds = []string{
	"slashdot.org",
	"thisiscolossal.com",
	"vox.com",
	"arstechnica.com",
	"www.youtube.com",
	"www.facebook.com",
	"longreads.com",
	"nautil.us",
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

	// open up mutex
	mutex <- struct{}{}

	return &DB{sql: db}
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

	var id int

	err := db.sql.QueryRow("SELECT id FROM subscribe WHERE user_id=? AND feed_id=?", uid, fid).Scan(&id)

	if err == sql.ErrNoRows {
		lock()
		_, err := db.sql.Exec("INSERT INTO subscribe (user_id, feed_id) VALUES (?, ?)", uid, fid)
		unlock()

		if err != nil {
			log.Fatal(err)
		}
		return
	}
	if err != nil {
		log.Fatal(err)
	}
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

type FeedUrlWithError struct {
	URL   string
	Error string
}

func (db *DB) GetUserFeedURLsWithFetchErrors(username string) []FeedUrlWithError {
	uid := db.GetUserID(username)

	rows, err := db.sql.Query(`
		SELECT f.url, f.fetch_error
		FROM feed f
		JOIN subscribe s ON f.id = s.feed_id
		JOIN user u ON s.user_id = u.id
		WHERE u.id = ?`, uid)
	if err == sql.ErrNoRows {
		return []FeedUrlWithError{}
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var feedErrors []FeedUrlWithError
	for rows.Next() {
		var feedError FeedUrlWithError
		var fetchError sql.NullString
		err = rows.Scan(&feedError.URL, &fetchError)
		if err != nil {
			log.Fatal(err)
		}
		if fetchError.Valid {
			feedError.Error = fetchError.String
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
	defer rows.Close()

	var orphanFeedUrls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return orphanFeedUrls
		}
		orphanFeedUrls = append(orphanFeedUrls, url)
	}

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

	return orphanFeedUrls
}

func (db *DB) GetUserID(username string) int {
	var uid int

	err := db.sql.QueryRow("SELECT id FROM user WHERE username=?", username).Scan(&uid)

	if err != nil {
		log.Fatal(err)
	}
	return uid
}

func (db *DB) GetFeedID(feedURL string) int {
	var fid int

	err := db.sql.QueryRow("SELECT id FROM feed WHERE url=?", feedURL).Scan(&fid)

	if err != nil {
		log.Fatal(err)
	}
	return fid
}

// WriteFeed writes an rss feed to the database for permanent storage
// if the given feed already exists, WriteFeed does nothing.
func (db *DB) WriteFeed(url string) {
	lock()
	_, err := db.sql.Exec(`INSERT INTO feed(url) VALUES(?)
				ON CONFLICT(url) DO NOTHING`, url)
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

	if err != nil {
		return "", err
	}
	if result.Valid {
		return result.String, nil
	}
	return "", nil
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

func (db *DB) GetPostId(postUrl, username string) int {
	var uid = db.GetUserID(username)
	var pid int

	// Try to get the post ID from the feeds the user is subscribed to
	err := db.sql.QueryRow(`
		SELECT p.id FROM post p
		JOIN feed f ON p.feed_id = f.id
		JOIN subscribe s ON f.id = s.feed_id
		WHERE p.url = ? AND s.user_id = ?`, postUrl, uid).Scan(&pid)

	if err == sql.ErrNoRows {
		// If no such post is found, get the ID of the first post with the given URL from the database
		err = db.sql.QueryRow("SELECT id FROM post WHERE url=?", postUrl).Scan(&pid)
	}

	if err != nil {
		log.Fatal(err)
	}

	return pid
}

func (db *DB) GetLatestPostsForGlobal(limit int) []*Post {
	query := `
        SELECT title, url, MAX(published_at) as published_at
        FROM post
        WHERE `

	// Add a 'NOT LIKE' clause for each item in the exclusion list
	for i, url := range listOfSpammyFeeds {
		if i > 0 {
			query += " AND "
		}
		query += fmt.Sprintf("url NOT LIKE '%%%s%%'", url)
	}

	query += `
        GROUP BY url
        ORDER BY published_at DESC
        LIMIT ?`

	rows, err := db.sql.Query(query, limit)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var posts []*Post
	for rows.Next() {
		var p Post
		var publishedTime string
		err = rows.Scan(&p.Title, &p.URL, &publishedTime)
		if err != nil {
			log.Fatal(err)
		}

		p.PublishedDatetime, err = db.TryParseDate(publishedTime)
		if err != nil {
			log.Fatal(err)
		}

		posts = append(posts, &p)
	}
	return posts
}

func (db *DB) GetPostsForFeed(feedUrl string) []*Post {
	feedId := db.GetFeedID(feedUrl)

	rows, err := db.sql.Query("SELECT title, url, published_at FROM post WHERE feed_id=?", feedId)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var posts []*Post
	for rows.Next() {
		var p Post
		err = rows.Scan(&p.Title, &p.URL, &p.PublishedDatetime)
		if err != nil {
			log.Fatal(err)
		}
		posts = append(posts, &p)
	}
	return posts
}

func (db *DB) GetPostsForUser(username string, limit int) []*UserPostEntry {
	uid := db.GetUserID(username)

	rows, err := db.sql.Query(`
        SELECT p.title, p.url, p.published_at, pr.has_read
        FROM post p
        JOIN feed f ON p.feed_id = f.id
        JOIN subscribe s ON f.id = s.feed_id
        JOIN user u ON s.user_id = u.id
        LEFT JOIN post_read pr ON p.id = pr.post_id AND u.id = pr.user_id
        WHERE u.id = ?
        ORDER BY p.published_at DESC
        LIMIT ?`, uid, limit)
	if err != nil {
		log.Fatal(err)
	}

	var userPostsEntries []*UserPostEntry
	for rows.Next() {
		var entry UserPostEntry
		var p gofeed.Item
		var hasRead sql.NullBool
		err = rows.Scan(&p.Title, &p.Link, &p.PublishedParsed, &hasRead)
		if err != nil {
			log.Fatal(err)
		}

		entry.Post = &p
		entry.IsRead = hasRead.Valid && hasRead.Bool // IsRead is true if hasRead is not NULL and is true

		userPostsEntries = append(userPostsEntries, &entry)
	}

	rows.Close()

	return userPostsEntries
}

func (db *DB) GetRandomPost() *Post {
	var p Post

	// Select a random post from a feed that has at least one post
	err := db.sql.QueryRow(`
        SELECT title, url, published_at 
        FROM post 
        WHERE feed_id IN (SELECT id FROM feed WHERE EXISTS (SELECT 1 FROM post WHERE feed_id = feed.id))
        ORDER BY RANDOM() 
        LIMIT 1
    `).Scan(&p.Title, &p.URL, &p.PublishedDatetime)

	if err != nil {
		log.Fatal(err)
	}

	return &p
}

func (db *DB) SetReadStatus(username string, postUrl string, read bool) {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl, username)

	var exists bool
	err := db.sql.QueryRow("SELECT 1 FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}

	lock()
	if exists {
		_, err = db.sql.Exec("UPDATE post_read SET has_read=? WHERE user_id=? AND post_id=?", read, userId, postId)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		_, err = db.sql.Exec("INSERT INTO post_read(user_id, post_id, has_read) VALUES(?, ?, ?)", userId, postId, read)
		if err != nil {
			log.Fatal(err)
		}
	}
	unlock()
}

func (db *DB) ToggleReadStatus(username string, postUrl string) {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl, username)

	var read bool

	err := db.sql.QueryRow("SELECT has_read FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&read)

	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}

	db.SetReadStatus(username, postUrl, !read)
}

func (db *DB) GetReadStatus(username string, postUrl string) bool {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl, username)

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
	// Check if the preference already exists
	var exists bool
	err := db.sql.QueryRow("SELECT EXISTS(SELECT 1 FROM user_preferences WHERE user_id = ? AND preference_name = ?)", userId, preferenceName).Scan(&exists)
	if err != nil {
		log.Printf("SaveUserPreference:: Error checking if preference exists: %v", err)
		return err
	}

	if exists {
		// Update existing preference
		lock()
		_, err := db.sql.Exec("UPDATE user_preferences SET preference_value = ? WHERE user_id = ? AND preference_name = ?", preferenceValue, userId, preferenceName)
		unlock()
		if err != nil {
			log.Printf("SaveUserPreference:: Error updating user preference: %v", err)
			return err
		}
	} else {
		// Insert new preference
		lock()
		_, err := db.sql.Exec("INSERT INTO user_preferences (user_id, preference_name, preference_value) VALUES (?, ?, ?)", userId, preferenceName, preferenceValue)
		unlock()
		if err != nil {
			log.Printf("SaveUserPreference:: Error inserting user preference: %v", err)
			return err
		}
	}

	return nil
}
