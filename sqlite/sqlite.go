package sqlite

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"time"

	"git.j3s.sh/vore/rss"
	_ "github.com/glebarez/go-sqlite"
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

func lock() {
	<-mutex
}

func unlock() {
	mutex <- struct{}{}
}

func (db *DB) GetUsernameBySessionToken(token string) string {
	var username string

	lock()
	err := db.sql.QueryRow("SELECT username FROM user WHERE session_token=?", token).Scan(&username)
	unlock()

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

	lock()
	err := db.sql.QueryRow("SELECT password FROM user WHERE username=?", username).Scan(&password)
	unlock()

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

	lock()
	err := db.sql.QueryRow("SELECT session_token FROM user WHERE username=?", username).Scan(&result)
	unlock()

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

	lock()
	err := db.sql.QueryRow("SELECT id FROM subscribe WHERE user_id=? AND feed_id=?", uid, fid).Scan(&id)
	unlock()

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

	lock()
	err := db.sql.QueryRow("SELECT username FROM user WHERE username=?", username).Scan(&result)
	unlock()

	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		log.Fatal(err)
	}
	return true
}

func (db *DB) GetAllFeedURLs() []string {
	// TODO: BAD SELECT STATEMENT!! SORRY :( --wesley
	lock()
	defer unlock()

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

	lock()
	defer unlock()

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
			LEFT JOIN subscribe ON post.feed_id = subscribe.feed_id
			WHERE subscribe.user_id != ?
        )`, userId, userId)

	if err != nil {
		log.Fatal(err)
	}
}

// DeleteOrphanFeeds deletes all feeds that are not subscribed to by any user,
// as well as all posts that belong to those feeds.
func (db *DB) DeleteOrphanFeeds() {
	lock()
	defer unlock()

	// Delete posts that belong to the orphan feeds
	_, err := db.sql.Exec(`
		DELETE FROM post
		WHERE feed_id NOT IN (SELECT feed_id FROM subscribe)`)
	if err != nil {
		log.Fatal(err)
	}

	// Delete the orphan feeds
	_, err = db.sql.Exec(`
		DELETE FROM feed
		WHERE id NOT IN (SELECT feed_id FROM subscribe)`)
	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) GetUserID(username string) int {
	var uid int

	lock()
	err := db.sql.QueryRow("SELECT id FROM user WHERE username=?", username).Scan(&uid)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
	return uid
}

func (db *DB) GetFeedID(feedURL string) int {
	var fid int

	lock()
	err := db.sql.QueryRow("SELECT id FROM feed WHERE url=?", feedURL).Scan(&fid)
	unlock()

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

	lock()
	err := db.sql.QueryRow("SELECT fetch_error FROM feed WHERE url=?", url).Scan(&result)
	unlock()

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
	_, err := db.sql.Exec("INSERT INTO post (feed_id, title, url, published_at) VALUES (?, ?, ?, ?) ON CONFLICT(url) DO NOTHING",
		feedId, title, url, publishedDatetime)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
}

func (db *DB) GetPostId(postUrl string) int {
	var pid int

	lock()
	err := db.sql.QueryRow("SELECT id FROM post WHERE url=?", postUrl).Scan(&pid)
	unlock()

	if err != nil {
		log.Fatal(err)
	}
	return pid
}

func (db *DB) GetLatestPosts(limit int) []*Post {
	lock()
	defer unlock()

	rows, err := db.sql.Query("SELECT title, url, published_at FROM post ORDER BY published_at DESC LIMIT ?", limit)
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

func (db *DB) GetPostsForFeed(feedUrl string) []*Post {
	feedId := db.GetFeedID(feedUrl)

	lock()
	defer unlock()

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

func (db *DB) GetPostsForUser(username string) []*rss.Item {
	uid := db.GetUserID(username)

	lock()
	rows, err := db.sql.Query(`
		SELECT p.title, p.url, p.published_at
		FROM post p
		JOIN feed f ON p.feed_id = f.id
		JOIN subscribe s ON f.id = s.feed_id
		JOIN user u ON s.user_id = u.id
		WHERE u.id = ?`, uid)
	if err != nil {
		log.Fatal(err)
	}

	var posts []*rss.Item
	for rows.Next() {
		var p rss.Item
		err = rows.Scan(&p.Title, &p.Link, &p.Date)
		if err != nil {
			log.Fatal(err)
		}

		posts = append(posts, &p)
	}

	rows.Close()
	unlock()

	for _, p := range posts {
		p.Read = db.GetReadStatus(username, p.Link)
	}

	return posts
}

func (db *DB) SetReadStatus(username string, postUrl string, read bool) {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl)

	var exists bool
	lock()
	err := db.sql.QueryRow("SELECT 1 FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}

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
	postId := db.GetPostId(postUrl)

	var read bool

	lock()
	err := db.sql.QueryRow("SELECT has_read FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&read)
	unlock()

	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}

	db.SetReadStatus(username, postUrl, !read)
}

func (db *DB) GetReadStatus(username string, postUrl string) bool {
	userId := db.GetUserID(username)
	postId := db.GetPostId(postUrl)

	var read bool

	lock()
	err := db.sql.QueryRow("SELECT has_read FROM post_read WHERE user_id=? AND post_id=?", userId, postId).Scan(&read)
	unlock()

	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}
	return read
}
