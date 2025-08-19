package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"codeberg.org/meadowingc/mire/constants"
	"codeberg.org/meadowingc/mire/lib"
	"codeberg.org/meadowingc/mire/reaper"
	"codeberg.org/meadowingc/mire/sqlite"
	"codeberg.org/meadowingc/mire/sqlite/user_preferences"
	"github.com/mmcdole/gofeed"
	"golang.org/x/crypto/bcrypt"
)

type Site struct {
	// title of the website
	title string

	// contains every single feed
	reaper *reaper.Reaper

	// site database handle
	db *sqlite.DB
}

var templates *template.Template

// New returns a fully populated & ready for action Site
func New() *Site {
	title := "mire"
	db := sqlite.New(title + ".db?_pragma=journal_mode(WAL)")

	s := Site{
		title:  title,
		reaper: reaper.New(db),
		db:     db,
	}

	funcMap := template.FuncMap{
		"printDomain": s.printDomain,
		"timeSince":   s.timeSince,
		"trimSpace":   strings.TrimSpace,
		"escapeURL":   url.QueryEscape,
		"makeSlice": func(args ...interface{}) []interface{} {
			return args
		},
	}

	tmplFiles := filepath.Join("files", "*.tmpl.html")
	templates = template.Must(template.New("whatever").Funcs(funcMap).ParseGlob(tmplFiles))

	return &s
}

func (s *Site) staticHandler(w http.ResponseWriter, r *http.Request) {
	file := filepath.Join("files", "static", r.PathValue("file"))
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		http.ServeFile(w, r, file)
		return
	}
	http.NotFound(w, r)
}

func (s *Site) indexHandler(w http.ResponseWriter, r *http.Request) {
	if s.loggedIn(r) {
		http.Redirect(w, r, "/u/"+s.username(r), http.StatusSeeOther)
		return
	}
	s.renderPage(w, r, "index", nil)
}

func (s *Site) aboutHandler(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "about", globalSiteStats)
}

func (s *Site) discoverHandler(w http.ResponseWriter, r *http.Request) {
	items := s.db.GetLatestPostsForDiscover(100)
	s.renderPage(w, r, "discover", items)
}

func (s *Site) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if s.loggedIn(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			s.renderPage(w, r, "login", nil)
		}
	}
	if r.Method == "POST" {
		username := r.FormValue("username")
		password := r.FormValue("password")

		err := s.login(w, username, password)
		if err != nil {
			s.renderErr("loginHandler", w, err.Error(), http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// TODO: make this take a POST only in accordance w/ some spec
func (s *Site) logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:  "session_token",
		Value: "",
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Site) registerHandler(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	err := s.register(username, password)
	if err != nil {
		s.renderErr("registerHandler", w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = s.login(w, username, password)
	if err != nil {
		s.renderErr("registerHandler", w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Site) userHandler(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	isUserRequestingOwnPage := s.username(r) == username

	if !s.db.UserExists(username) {
		http.NotFound(w, r)
		return
	}

	// logged in user preferences
	loggedInUsername := s.username(r)
	var userPreferences *user_preferences.UserPreferences
	if loggedInUsername != "" {
		userPreferences = user_preferences.GetUserPreferences(s.db, s.db.GetUserID(username))
	} else {
		userPreferences = user_preferences.GetDefaultUserPreferences()
	}

	numPostsToShow := 200
	if isUserRequestingOwnPage {
		numPostsToShow = userPreferences.NumPostsToShowInHomeScreen
	}

	items := s.db.GetPostsForUser(username, numPostsToShow)

	// get the N oldest unread items
	oldestUnreadPosts := make([]*sqlite.UserPostEntry, 0)
	favoritesUnread := make([]*sqlite.UserPostEntry, 0)

	if isUserRequestingOwnPage && userPreferences.NumUnreadPostsToShowInHomeScreen > 0 {
		// sort inversely by date
		oldestItems := make([]*sqlite.UserPostEntry, len(items))
		copy(oldestItems, items)
		sort.Slice(oldestItems, func(i, j int) bool {
			return oldestItems[j].Post.PublishedParsed.After(*oldestItems[i].Post.PublishedParsed)
		})

		// get N unread posts
		for _, item := range oldestItems {
			if !item.IsRead {
				oldestUnreadPosts = append(oldestUnreadPosts, item)
			}

			if len(oldestUnreadPosts) >= userPreferences.NumUnreadPostsToShowInHomeScreen {
				break
			}
		}

		// get unread favorites
		favoritesUnreadFromDb, err := s.db.GetFavoriteUnreadPosts(username, userPreferences.NumUnreadPostsToShowInHomeScreen)
		if err != nil {
			s.renderErr("userHandler", w, err.Error(), http.StatusInternalServerError)
			return
		}

		favoritesUnread = favoritesUnreadFromDb
	}

	data := struct {
		User              string
		Items             []*sqlite.UserPostEntry
		OldestUnread      []*sqlite.UserPostEntry
		RequestingOwnPage bool
		UserPreferences   *user_preferences.UserPreferences
		FavoritesUnread   []*sqlite.UserPostEntry
	}{
		User:              username,
		Items:             items,
		OldestUnread:      oldestUnreadPosts,
		RequestingOwnPage: isUserRequestingOwnPage,
		UserPreferences:   userPreferences,
		FavoritesUnread:   favoritesUnread,
	}

	s.renderPage(w, r, "user", data)
}

func (s *Site) userBlogrollHandler(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")

	if !s.db.UserExists(username) {
		http.NotFound(w, r)
		return
	}

	items := s.db.GetUserFeedURLs(username)
	data := struct {
		User  string
		Items []string
	}{
		User:  username,
		Items: items,
	}

	s.renderPage(w, r, "blogroll", data)
}

func (s *Site) settingsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("settingsHandler", w, "", http.StatusUnauthorized)
		return
	}

	username := s.username(r)
	if !s.db.UserExists(username) {
		http.NotFound(w, r)
		return
	}

	urlsAndErrors := s.db.GetUserFeedURLsForSettings(s.username(r))

	sort.Slice(urlsAndErrors, func(i, j int) bool {
		return urlsAndErrors[i].URL < urlsAndErrors[j].URL
	})

	userPreferences := user_preferences.GetUserPreferences(s.db, s.db.GetUserID(username))

	data := struct {
		UrlsAndErrors   []sqlite.FeedUrlForSettings
		UserPreferences *user_preferences.UserPreferences
	}{
		UrlsAndErrors:   urlsAndErrors,
		UserPreferences: userPreferences,
	}

	s.renderPage(w, r, "settings", data)
}

func (s *Site) settingsSubscribeHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("settingsSubscribeHandler", w, "", http.StatusUnauthorized)
		return
	}

	// validate user input
	var validatedURLs []string
	for _, inputURL := range strings.Split(r.FormValue("submit"), "\r\n") {
		inputURL = strings.TrimSpace(inputURL)
		if inputURL == "" {
			continue
		}

		// if the entry is already in reaper, don't validate
		if s.reaper.HasFeed(inputURL) {
			validatedURLs = append(validatedURLs, inputURL)
			continue
		}
		if _, err := url.ParseRequestURI(inputURL); err != nil {
			e := fmt.Sprintf("can't parse url '%s': %s", inputURL, err)
			s.renderErr("settingsSubscribeHandler", w, e, http.StatusBadRequest)
			return
		}
		validatedURLs = append(validatedURLs, inputURL)
	}

	// write to reaper + db
	semaphore := make(chan struct{}, 20)
	var wg sync.WaitGroup

	for _, u := range validatedURLs {
		semaphore <- struct{}{} // acquire a token
		wg.Add(1)               // increment the WaitGroup counter
		go func(u string) {
			defer func() {
				<-semaphore // release the token when done
				wg.Done()   // decrement the WaitGroup counter
			}()

			// if it's in reaper, it's in the db, safe to skip
			if s.reaper.HasFeed(u) {
				return
			}

			// save feed to dabase
			s.db.WriteFeed(u)

			// add empty feed entry to reaper
			s.reaper.AddFeedStub(u)

			// try to get posts and save them
			err := s.reaper.Fetch(u)
			if err != nil {
				fmt.Printf("reaper: can't fetch '%s' %s\n", u, err)
				s.db.SetFeedFetchError(u, err.Error())
				return
			}

			newFeed := s.reaper.GetFeed(u)

			// update fetch time in DB
			s.db.UpdateFeedLastRefreshTime(newFeed.FeedLink, time.Now())

			// save feed posts to db
			for _, post := range newFeed.Items {
				s.db.SavePost(u, post.Title, post.Link, *post.PublishedParsed)
			}

			log.Printf("reaper: registered new feed '%s' with '%d' posts\n", u, len(newFeed.Items))
		}(u)
	}

	wg.Wait() // wait for all goroutines to finish

	// TODO: the below is convoluted and can definitely be improved

	username := s.username(r)
	userOldFeeds := s.db.GetUserFeedURLsForSettings(username)

	userOldFeedsMap := make(map[string]sqlite.FeedUrlForSettings)
	for _, oldFeed := range userOldFeeds {
		userOldFeedsMap[oldFeed.URL] = oldFeed
	}

	// subscribe to all listed feeds exclusively
	s.db.UnsubscribeAll(username)
	for _, url := range validatedURLs {
		s.db.Subscribe(username, url)

		// If the user was previously "favoriting" this feed, preserve favorite status
		if oldFeed, ok := userOldFeedsMap[url]; ok && oldFeed.IsFavorite {
			s.db.SetFeedFavoriteStatus(username, url, oldFeed.IsFavorite)
		}
	}

	s.db.DeleteOrphanedPostReads(username)
	orphanedFeeds := s.db.DeleteOrphanFeeds()
	for _, feedUrl := range orphanedFeeds {
		s.reaper.RemoveFeed(feedUrl)
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Site) changePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("changePasswordHandler", w, "", http.StatusUnauthorized)
		return
	}

	username := s.username(r)
	currentPassword := r.FormValue("currentPassword")
	newPassword := r.FormValue("newPassword")
	confirmNewPassword := r.FormValue("confirmNewPassword")

	if newPassword != confirmNewPassword {
		s.renderErr("changePasswordHandler", w, "New passwords do not match", http.StatusBadRequest)
		return
	}

	storedPassword := s.db.GetPassword(username)
	err := bcrypt.CompareHashAndPassword([]byte(storedPassword), []byte(currentPassword))
	if err != nil {
		s.renderErr("changePasswordHandler", w, "Current password is incorrect", http.StatusUnauthorized)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		s.renderErr("changePasswordHandler", w, "Failed to hash new password", http.StatusInternalServerError)
		return
	}

	err = s.db.UpdatePassword(username, string(hashedPassword))
	if err != nil {
		s.renderErr("changePasswordHandler", w, "Failed to update password", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Site) settingsPreferencesHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("settingsPreferencesHandler", w, "", http.StatusUnauthorized)
		return
	}

	newPreferences := &user_preferences.UserPreferences{}

	valPointer := reflect.ValueOf(newPreferences)
	val := valPointer.Elem()
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("db")
		if tag == "" {
			log.Fatalf("settingsPreferencesHandler:: Field %s does not have a 'db' tag", field.Name)
		}

		// `tag` is the expected form name
		newValueForField := r.FormValue(tag)
		if val.Field(i).Kind() == reflect.Bool {
			// Checkboxes return "on" if checked, otherwise they are not included in the form data
			val.Field(i).SetBool(newValueForField == "on")
		} else {
			if newValueForField == "" {
				e := fmt.Sprintf("no value passed for the required field '%s'", tag)
				s.renderErr("settingsPreferencesHandler", w, e, http.StatusBadRequest)
				return
			}
			user_preferences.SetFieldValue(val.Field(i), newValueForField)
		}
	}

	// validate newPreferences
	if newPreferences.NumPostsToShowInHomeScreen < 1 || newPreferences.NumPostsToShowInHomeScreen > 300 {
		e := fmt.Sprintf("invalid number of posts to show '%d'", newPreferences.NumPostsToShowInHomeScreen)
		s.renderErr("settingsPreferencesHandler", w, e, http.StatusBadRequest)
		return
	}

	if newPreferences.NumUnreadPostsToShowInHomeScreen < 0 || newPreferences.NumUnreadPostsToShowInHomeScreen > 20 {
		e := fmt.Sprintf("invalid number of unread posts to show '%d'", newPreferences.NumUnreadPostsToShowInHomeScreen)
		s.renderErr("settingsPreferencesHandler", w, e, http.StatusBadRequest)
		return
	}

	username := s.username(r)
	userId := s.db.GetUserID(username)
	user_preferences.SaveUserPreferences(s.db, userId, newPreferences)

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Site) feedDetailsHandler(w http.ResponseWriter, r *http.Request) {
	encodedURL := r.PathValue("url")
	decodedURL, err := url.QueryUnescape(encodedURL)
	if err != nil {
		e := fmt.Sprintf("failed to decode URL '%s' %s", encodedURL, err)
		s.renderErr("feedDetailsHandler", w, e, http.StatusBadRequest)
		return
	}

	fetchErr, err := s.db.GetFeedFetchError(decodedURL)
	if err != nil {
		e := fmt.Sprintf("failed to fetch feed error '%s' %s", encodedURL, err)
		s.renderErr("feedDetailsHandler", w, e, http.StatusBadRequest)
		return
	}

	// Get user preferences for logged in users
	loggedInUsername := s.username(r)
	var userPreferences *user_preferences.UserPreferences
	var posts []*sqlite.UserPostEntry

	if loggedInUsername != "" {
		userPreferences = user_preferences.GetUserPreferences(s.db, s.db.GetUserID(loggedInUsername))
		// Get posts with read status for logged in users
		posts = s.db.GetPostsForFeedWithReadStatus(decodedURL, loggedInUsername)
	} else {
		userPreferences = user_preferences.GetDefaultUserPreferences()
		// For non-logged in users, convert regular posts to UserPostEntry format
		regularPosts := s.db.GetPostsForFeed(decodedURL)
		posts = make([]*sqlite.UserPostEntry, len(regularPosts))
		for i, post := range regularPosts {
			posts[i] = &sqlite.UserPostEntry{
				Post: &gofeed.Item{
					Title:           post.Title,
					Link:            post.URL,
					PublishedParsed: &post.PublishedDatetime,
				},
				IsRead:  false, // Non-logged in users see everything as unread
				FeedURL: post.FeedURL,
			}
		}
	}

	feed := s.reaper.GetFeed(decodedURL)
	if feed == nil {
		// Feed not found in reaper, maybe it exists in DB but hasn't been loaded
		// Try to fetch it first
		if !s.reaper.HasFeed(decodedURL) {
			// Add the feed to reaper and try to fetch it
			s.reaper.AddFeedStub(decodedURL)
			err := s.reaper.Fetch(decodedURL)
			if err != nil {
				// If we can't fetch it, create a minimal feed object
				feed = &gofeed.Feed{
					Title:    decodedURL,
					FeedLink: decodedURL,
					Link:     decodedURL,
				}
			} else {
				feed = s.reaper.GetFeed(decodedURL)
			}
		} else {
			// Feed exists in reaper but GetFeed returned nil - this shouldn't happen
			// Create a minimal feed object as fallback
			feed = &gofeed.Feed{
				Title:    decodedURL,
				FeedLink: decodedURL,
				Link:     decodedURL,
			}
		}
	}

	// Check if user is subscribed to this feed
	var isSubscribed bool
	if loggedInUsername != "" {
		isSubscribed = s.db.IsUserSubscribedToFeed(loggedInUsername, decodedURL)
	}

	var isFavorite bool
	if loggedInUsername != "" && isSubscribed {
		isFavorite = s.db.IsFeedFavorite(loggedInUsername, decodedURL)
	}

	feedData := struct {
		Feed            *gofeed.Feed
		Posts           []*sqlite.UserPostEntry
		FetchFailure    string
		UserPreferences *user_preferences.UserPreferences
		IsSubscribed    bool
		IsFavorite      bool
		FeedURL         string
	}{
		Feed:            feed,
		Posts:           posts,
		FetchFailure:    fetchErr,
		UserPreferences: userPreferences,
		IsSubscribed:    isSubscribed,
		IsFavorite:      isFavorite,
		FeedURL:         decodedURL,
	}

	s.renderPage(w, r, "feedDetails", feedData)
}

// splitFeedHandler serves the "split feed" page aggregating per-feed unread + recent posts.
func (s *Site) splitFeedHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username := s.username(r)
	feedURLs := s.db.GetUserFeedURLs(username)

	// Get user preferences
	userPreferences := user_preferences.GetUserPreferences(s.db, s.db.GetUserID(username))

	type perFeed struct {
		URL         string
		Title       string
		UnreadCount int
		Posts       []*sqlite.UserPostEntry
		AnchorID    string
	}

	feedsData := make([]perFeed, 0, len(feedURLs))
	totalUnread := 0
	totalPosts := 0

	for _, feedURL := range feedURLs {
		allPosts := s.db.GetPostsForFeedWithReadStatus(feedURL, username) // DESC by published_at
		totalPosts += len(allPosts)

		unread := make([]*sqlite.UserPostEntry, 0, 12)
		read := make([]*sqlite.UserPostEntry, 0, 12)

		for _, p := range allPosts {
			if !p.IsRead {
				unread = append(unread, p)
			} else {
				read = append(read, p)
			}
		}

		unreadCount := len(unread)
		totalUnread += unreadCount

		display := make([]*sqlite.UserPostEntry, 0, 12)
		// Take unread first
		for _, p := range unread {
			if len(display) >= 12 {
				break
			}
			display = append(display, p)
		}
		// Fill with newest read until 12
		if len(display) < 12 {
			for _, p := range read {
				if len(display) >= 12 {
					break
				}
				display = append(display, p)
			}
		}

		// Sort chosen posts by date DESC
		sort.Slice(display, func(i, j int) bool {
			if display[i].Post.PublishedParsed == nil || display[j].Post.PublishedParsed == nil {
				return false
			}
			return display[i].Post.PublishedParsed.After(*display[j].Post.PublishedParsed)
		})

		feedObj := s.reaper.GetFeed(feedURL)
		title := ""
		if feedObj != nil && strings.TrimSpace(feedObj.Title) != "" {
			title = feedObj.Title
		} else {
			title = s.printDomain(feedURL)
		}

		feedsData = append(feedsData, perFeed{
			URL:         feedURL,
			Title:       title,
			UnreadCount: unreadCount,
			Posts:       display,
			AnchorID:    s.sanitizeAnchorID(title),
		})
	}

	// Alphabetical by Title
	sort.Slice(feedsData, func(i, j int) bool {
		return strings.ToLower(feedsData[i].Title) < strings.ToLower(feedsData[j].Title)
	})

	data := struct {
		Feeds           []perFeed
		TotalUnread     int
		TotalPosts      int
		UserPreferences *user_preferences.UserPreferences
	}{
		Feeds:           feedsData,
		TotalUnread:     totalUnread,
		TotalPosts:      totalPosts,
		UserPreferences: userPreferences,
	}

	s.renderPageWithTitle(w, r, "split", fmt.Sprintf("(%d/%d) - Split View | %s", totalUnread, totalPosts, s.title), data)
}

// sanitizeAnchorID converts a string to a safe anchor id.
func (s *Site) sanitizeAnchorID(str string) string {
	str = strings.TrimSpace(str)
	if str == "" {
		return "feed"
	}
	re := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	str = re.ReplaceAllString(str, "-")
	str = strings.Trim(str, "-")
	if str == "" {
		return "feed"
	}
	return str
}

// username fetches a client's username based
// on the sessionToken that user has set. username
// will return "" if there is no sessionToken.
func (s *Site) username(r *http.Request) string {
	cookie, err := r.Cookie("session_token")
	if err == http.ErrNoCookie {
		return ""
	}
	if err != nil {
		log.Println(err)
	}
	username := s.db.GetUsernameBySessionToken(cookie.Value)
	return username
}

func (s *Site) loggedIn(r *http.Request) bool {
	return s.username(r) != ""
}

// login compares the sqlite password field against the user supplied password and
// sets a session token against the supplied writer.
func (s *Site) login(w http.ResponseWriter, username string, password string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	if password == "" {
		return fmt.Errorf("password cannot be empty")
	}
	if !s.db.UserExists(username) {
		return fmt.Errorf("user '%s' does not exist", username)
	}
	storedPassword := s.db.GetPassword(username)
	err := bcrypt.CompareHashAndPassword([]byte(storedPassword), []byte(password))
	if err != nil {
		return fmt.Errorf("invalid password")
	}
	sessionToken, err := s.db.GetSessionToken(username)
	if err != nil {
		return err
	}
	if sessionToken == "" {
		sessionToken = lib.GenerateSecureToken(32)
		err := s.db.SetSessionToken(username, sessionToken)
		if err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:    "session_token",
		Expires: time.Now().Add(time.Hour * 24 * 365),
		Value:   sessionToken,
	})
	return nil
}

func (s *Site) register(username string, password string) error {
	if s.db.UserExists(username) {
		return fmt.Errorf("user '%s' already exists", username)
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	err = s.db.AddUser(username, string(hashedPassword))
	if err != nil {
		return err
	}
	return nil
}

func (s *Site) visitRandomPostHandler(w http.ResponseWriter, r *http.Request) {
	post := s.db.GetRandomPost()

	http.Redirect(w, r, post.URL, http.StatusSeeOther)
}

func (s *Site) apiSetPostReadStatus(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("visitRandomPostHandler", w, "", http.StatusUnauthorized)
		return
	}

	postUrlEncoded := r.PathValue("postUrl")
	postUrl, err := url.QueryUnescape(postUrlEncoded)
	if err != nil {
		s.renderErr("visitRandomPostHandler", w, err.Error(), http.StatusBadRequest)
		return
	}

	currentUsername := s.username(r)

	hasRead := r.FormValue("new_has_read") == "true"

	s.db.SetReadStatus(currentUsername, postUrl, hasRead)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderPage renders the given page and passes data to the
// template execution engine. it's normally the last thing a
// handler should do tbh.
func (s *Site) renderPage(w http.ResponseWriter, r *http.Request, page string, data any) {
	// fields on this anon struct are generally
	// pulled out of Data when they're globally required
	// callers should jam anything they want into Data
	pageData := struct {
		Title      string
		Username   string
		LoggedIn   bool
		CutePhrase string
		Data       any
	}{
		Title:      page + " | " + s.title,
		Username:   s.username(r),
		LoggedIn:   s.loggedIn(r),
		CutePhrase: s.randomCutePhrase(),
		Data:       data,
	}

	if constants.DEBUG_MODE {
		funcMap := template.FuncMap{
			"printDomain": s.printDomain,
			"timeSince":   s.timeSince,
			"trimSpace":   strings.TrimSpace,
			"escapeURL":   url.QueryEscape,
			"makeSlice": func(args ...interface{}) []interface{} {
				return args
			},
		}

		tmplFiles := filepath.Join("files", "*.tmpl.html")
		templates = template.Must(template.New("whatever").Funcs(funcMap).ParseGlob(tmplFiles))
	}

	err := templates.ExecuteTemplate(w, page, pageData)
	if err != nil {
		s.renderErr("renderPage", w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", http.DetectContentType([]byte(page)))
}

// renderPageWithTitle is like renderPage but allows explicit title override.
func (s *Site) renderPageWithTitle(w http.ResponseWriter, r *http.Request, templateName string, title string, data any) {
	pageData := struct {
		Title      string
		Username   string
		LoggedIn   bool
		CutePhrase string
		Data       any
	}{
		Title:      title,
		Username:   s.username(r),
		LoggedIn:   s.loggedIn(r),
		CutePhrase: s.randomCutePhrase(),
		Data:       data,
	}

	if constants.DEBUG_MODE {
		funcMap := template.FuncMap{
			"printDomain": s.printDomain,
			"timeSince":   s.timeSince,
			"trimSpace":   strings.TrimSpace,
			"escapeURL":   url.QueryEscape,
			"makeSlice": func(args ...interface{}) []interface{} {
				return args
			},
		}
		tmplFiles := filepath.Join("files", "*.tmpl.html")
		templates = template.Must(template.New("whatever").Funcs(funcMap).ParseGlob(tmplFiles))
	}

	err := templates.ExecuteTemplate(w, templateName, pageData)
	if err != nil {
		s.renderErr("renderPageWithTitle", w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType([]byte(templateName)))
}

// printDomain does a best-effort uri parse and
// prints the base domain, otherwise returning the
// unmodified string
func (s *Site) printDomain(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err == nil {
		hostname := parsedURL.Hostname()
		if hostname == "medium.com" || strings.HasSuffix(hostname, ".medium.com") {
			// Handle Medium URLs
			pathSegments := strings.Split(parsedURL.Path, "/")
			for _, segment := range pathSegments {
				if len(segment) > 0 && segment[0] == '@' {
					return "medium.com/" + segment
				}
			}
		}
		return hostname
	}
	// do our best to trim it manually if url parsing fails
	trimmedStr := strings.TrimSpace(rawURL)
	trimmedStr = strings.TrimPrefix(trimmedStr, "http://")
	trimmedStr = strings.TrimPrefix(trimmedStr, "https://")

	return strings.Split(trimmedStr, "/")[0]
}

func (s *Site) timeSince(t time.Time) string {
	now := time.Now()
	duration := now.Sub(t)

	minutes := int(duration.Minutes())
	hours := int(duration.Hours())
	days := int(duration.Hours() / 24)
	weeks := int(duration.Hours() / (24 * 7))
	months := int(duration.Hours() / (24 * 7 * 4))
	years := int(duration.Hours() / (24 * 7 * 4 * 12))

	if years > 100 {
		return "over 100 years ago ಠ_ಠ"
	} else if years > 1 {
		return fmt.Sprintf("%d years ago", years)
	} else if months > 1 {
		return fmt.Sprintf("%d months ago", months)
	} else if weeks > 1 {
		return fmt.Sprintf("%d weeks ago", weeks)
	} else if days > 1 {
		return fmt.Sprintf("%d days ago", days)
	} else if hours > 1 {
		return fmt.Sprintf("%d hours ago", hours)
	} else if minutes > 1 {
		return fmt.Sprintf("%d mins ago", minutes)
	} else {
		return "just now"
	}
}

// renderErr sets the correct http status in the header,
// optionally decorates certain errors, then renders the err page
func (s *Site) renderErr(caller string, w http.ResponseWriter, error string, code int) {
	var prefix string
	switch code {
	case http.StatusBadRequest:
		prefix = "400 bad request\n"
	case http.StatusUnauthorized:
		prefix = "401 unauthorized\n"
	case http.StatusInternalServerError:
		prefix = "(╥﹏╥) oopsie woopsie, uwu\n"
		prefix += "we made a fucky wucky (╥﹏╥)\n\n"
		prefix += "500 internal server error\n"
	}
	log.Println(caller + ":: " + prefix + error)
	http.Error(w, prefix+error, code)
}

func (s *Site) randomCutePhrase() string {
	phrases := []string{
		"nom nom posts (๑ᵔ⤙ᵔ๑)",
		"^(;,;)^ vawr",
		"devouring feeds since 2024",
		"tfw new rss post (⊙ _ ⊙ )",
		"( ˘͈ ᵕ ˘͈♡) <3",
		"a no-bullshit feed reader",
		"*chomp* good feeds",
		// TODO: GPT generated quotes too much?
		"(｡♥‿♥｡) love for feeds",
		"(*^‿^*) nom nom feeds",
		"(^・ω・^ ) feed me and tell me I'm pretty",
		"(^・ω・^ ) feeds are life",
		"(^・ω・^ ) feeds are meow-nificent",
		"(^・ω・^ ) feeds are purr-fect",
		"(^・ω・^ ) feeds are the best",
		"(^・ω・^ ) feeds, feeds, feeds",
		"(^・ω・^ ) I'm all about that feed",
		"(^・ω・^ ) purr-fect feeds",
		"(^._.^)ﾉ all feeds, all the time",
		"(^._.^)ﾉ feed-reading, my favorite hobby",
		"(^._.^)ﾉ feeds are everything",
		"(^._.^)ﾉ feeds are the cat's pajamas",
		"(^._.^)ﾉ feeds for days",
		"(^._.^)ﾉ keep the posts coming",
		"(^._.^)ﾉ new posts, yay!",
		"(^◕ᴥ◕^) feed-reading beast",
		"(=^･ω･^=) feed me, Seymour",
		"(=^･ω･^=) feeds make the world go round",
		"(=^･ω･^=) got feeds?",
		"(=^･ω･^=) more posts, please",
		"(=^‿^=) feeds are love",
		"(=^‿^=) feeds are my jam",
		"(=^‿^=) feeds are pawsome",
		"(=^‿^=) feeds are the bee's knees",
		"(=^‿^=) I can haz more posts?",
		"(=^‿^=) I'm here for the feeds",
		"(✿◠‿◠) feed me more posts",
		"(づ｡◕‿‿◕｡)づ delicious posts",
	}
	i := rand.Intn(len(phrases))
	return phrases[i]
}

// apiSetFavoriteFeedHandler toggles the favorite status of a feed for the user.
func (s *Site) apiSetFavoriteFeedHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("apiToggleFavoriteFeedHandler", w, "", http.StatusUnauthorized)
		return
	}

	feedUrlEncoded := r.PathValue("feedUrl")
	if feedUrlEncoded == "" {
		s.renderErr("apiToggleFavoriteFeedHandler", w, "Feed URL is required", http.StatusBadRequest)
		return
	}

	feedUrl, err := url.QueryUnescape(feedUrlEncoded)
	if err != nil {
		s.renderErr("apiToggleFavoriteFeedHandler", w, err.Error(), http.StatusBadRequest)
		return
	}

	username := s.username(r)
	isFavorite := r.FormValue("new_is_favorite") == "true"

	err = s.db.SetFeedFavoriteStatus(username, feedUrl, isFavorite)
	if err != nil {
		s.renderErr("apiToggleFavoriteFeedHandler", w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// apiToggleSubscriptionHandler handles subscribing/unsubscribing to a feed
func (s *Site) apiToggleSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.renderErr("apiToggleSubscriptionHandler", w, "", http.StatusUnauthorized)
		return
	}

	feedUrlEncoded := r.PathValue("feedUrl")
	if feedUrlEncoded == "" {
		s.renderErr("apiToggleSubscriptionHandler", w, "Feed URL is required", http.StatusBadRequest)
		return
	}

	feedUrl, err := url.QueryUnescape(feedUrlEncoded)
	if err != nil {
		s.renderErr("apiToggleSubscriptionHandler", w, err.Error(), http.StatusBadRequest)
		return
	}

	username := s.username(r)
	shouldSubscribe := r.FormValue("subscribe") == "true"

	if shouldSubscribe {
		// Subscribe to the feed
		// First ensure the feed exists in the database
		s.db.WriteFeed(feedUrl)
		s.db.Subscribe(username, feedUrl)

		// Add to reaper if not already there
		if !s.reaper.HasFeed(feedUrl) {
			s.reaper.AddFeedStub(feedUrl)
			// Try to fetch the feed in the background
			go func() {
				err := s.reaper.Fetch(feedUrl)
				if err != nil {
					log.Printf("Failed to fetch feed %s: %v", feedUrl, err)
					s.db.SetFeedFetchError(feedUrl, err.Error())
				}
			}()
		}
	} else {
		// Unsubscribe from the feed
		err = s.db.Unsubscribe(username, feedUrl)
		if err != nil {
			s.renderErr("apiToggleSubscriptionHandler", w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Clean up orphaned data
		s.db.DeleteOrphanedPostReads(username)
		orphanedFeeds := s.db.DeleteOrphanFeeds()
		for _, orphanedFeedUrl := range orphanedFeeds {
			s.reaper.RemoveFeed(orphanedFeedUrl)
		}
	}

	w.WriteHeader(http.StatusOK)
}
