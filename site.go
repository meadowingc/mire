package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"git.j3s.sh/vore/lib"
	"git.j3s.sh/vore/reaper"
	"git.j3s.sh/vore/sqlite"
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

// New returns a fully populated & ready for action Site
func New() *Site {
	title := "vore"
	db := sqlite.New(title + ".db")
	s := Site{
		title:  title,
		reaper: reaper.Summon(db),
		db:     db,
	}
	go s.reaper.Start()
	return &s
}

func (s *Site) indexHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "GET") {
		return
	}
	if s.loggedIn(r) {
		username := s.username(r)
		fmt.Fprintf(w, `<!DOCTYPE html>
			<title>%s</title>
			<p> { %s <a href=/logout>logout</a> }
			<p> <a href="/%s">view feeds</a>
			<p> <a href="/feeds">edit feeds</a>`, s.title, username, username)
	} else {
		fmt.Fprintf(w, `<!DOCTYPE html>
			<title>%s</title>
			<a href="/login">login</a>`, s.title)
	}
}

func (s *Site) loginHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "GET", "POST") {
		return
	}
	if r.Method == "GET" {
		if s.loggedIn(r) {
			fmt.Fprintf(w, "you are already logged in :3\n")
		} else {
			fmt.Fprintf(w, `<!DOCTYPE html>
				<h3>login</h3>
				<form method="POST" action="/login">
				<label for="username">username:</label>
				<input type="text" name="username" required><br>
				<label for="password">password:</label>
				<input type="password" name="password" required><br>
				<input type="submit" value="login">
				</form>`)
			fmt.Fprintf(w, `<h3>register</h3>
				<form method="POST" action="/register">
				<label for="username">username:</label>
				<input type="text" name="username" required><br>
				<label for="password">password:</label>
				<input type="password" name="password" required><br>
				<input type="submit" value="register">
				</form>`)
		}
	}
	if r.Method == "POST" {
		username := r.FormValue("username")
		password := r.FormValue("password")

		err := s.login(w, username, password)
		if err != nil {
			fmt.Fprintf(w, `<!DOCTYPE html>
				<p>💀 unauthorized: %s`, err)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// TODO: make this take a POST only in accordance w/ some spec
func (s *Site) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "GET", "POST") {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  "session_token",
		Value: "",
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Site) registerHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "POST") {
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	err := s.register(username, password)
	if err != nil {
		internalServerError(w, "failed to register user")
		return
	}
	err = s.login(w, username, password)
	if err != nil {
		internalServerError(w, "extremely weird login error")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Site) userHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "GET") {
		return
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
			<title>%s</title>`, s.title)

	username := strings.TrimPrefix(r.URL.Path, "/")
	feeds := s.reaper.GetUserFeeds(username)
	if len(feeds) == 0 {
		fmt.Fprintf(w, "%s has no feeds 😭", username)
		return
	}

	sortedItems := s.reaper.SortFeedItems(feeds)
	for i := range sortedItems {
		fmt.Fprintf(w, `<p><a href="%s">%s</a>`,
			sortedItems[i].Link, sortedItems[i].Title)
	}
}

// [ ] GET /feeds
//
//	> if no feeds, /discover for ideas
//	pretty-print your feeds
//	<text box with pre-populated list of your feed urls, one per line>
//	button: validate
//	POST /feeds/validate
//	logged out: unauthorized. click here to login.
func (s *Site) feedsHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "GET") {
		return
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
			<title>%s</title>`, s.title)

	if !s.loggedIn(r) {
		fmt.Fprintf(w,
			`<p>⚠️ you are not logged in⚠️
			<p>please click the skull: <a href="/login">💀</a>`)
		return
	}

	feeds := s.reaper.GetUserFeeds(s.username(r))
	fmt.Fprintf(w, `<pre>you are subscribed to %d feeds</pre>`, len(feeds))
	for _, feed := range feeds {
		fmt.Fprintf(w, `
<details>
<summary>%s</summary>
<pre>
title: %s
url: %s
posts: %d
</pre>
</details>`, feed.Title, feed.Title, feed.UpdateURL, len(feed.Items))
	}
	fmt.Fprintf(w, `<pre>add/remove feed URLs to this box to change your subscriptions</pre>
				<form method="POST" action="/feeds/submit">
				<textarea name="submit" rows="10" cols="72">`)
	for _, feed := range feeds {
		fmt.Fprintf(w, "%s\n", feed.UpdateURL)
	}
	fmt.Fprintf(w, `</textarea>
				<br>
				<input type="submit" value="update feeds">
				</form>`)
	// TODO: textbox with feed.URL
	// TODO: validate button
}

func (s *Site) feedsSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, "POST") {
		return
	}
	if !s.loggedIn(r) {
		http.Error(w, "401 unauthorized", 401)
		return
	}
	inputData := r.FormValue("submit")
	if inputData == "" {
		http.Error(w, "400 bad request: you must submit data", 400)
		return
	}

	// TODO: validate user input moar
	feeds := strings.Split(inputData, "\r\n")
	s.db.UnsubscribeAll(s.username(r))
	for _, feed := range feeds {
		// TODO: show diff before submission (like tf plan)
		// TODO: check if feed exists in db already?
		// TODO: validate that title exists
		if feed == "" {
			continue
		}
		err := s.reaper.Add(feed)
		if err == io.EOF {
			http.Error(w, "400 bad request: could not fetch "+feed, 400)
			fmt.Println(err)
			return
		}
		if err != nil {
			http.Error(w, "400 bad request: "+err.Error(), 400)
			fmt.Println(err)
			return
		}
		s.db.WriteFeed(feed)
		s.db.Subscribe(s.username(r), feed)
	}

	http.Redirect(w, r, "/feeds", http.StatusSeeOther)
}

// username fetches a client's username based
// on the sessionToken that user has set. username
// will return "" if there is no sessionToken.
func (s *Site) username(r *http.Request) string {
	sessionToken, err := r.Cookie("session_token")
	if err != nil {
		return ""
	}
	username := s.db.GetUsernameBySessionToken(sessionToken.Value)
	return username
}

func (s *Site) loggedIn(r *http.Request) bool {
	if s.username(r) == "" {
		return false
	}
	return true
}

// login compares the sqlite password field against the user supplied password and
// sets a session token against the supplied writer.
func (s *Site) login(w http.ResponseWriter, username string, password string) error {
	if username == "" {
		return fmt.Errorf("username cannot be nil")
	}
	if password == "" {
		return fmt.Errorf("password cannot be nil")
	}
	if !s.db.UserExists(username) {
		return fmt.Errorf("user does not exist")
	}
	storedPassword := s.db.GetPassword(username)
	err := bcrypt.CompareHashAndPassword([]byte(storedPassword), []byte(password))
	if err != nil {
		return fmt.Errorf("invalid password")
	}
	sessionToken := lib.GenerateSessionToken()
	s.db.SetSessionToken(username, sessionToken)
	http.SetCookie(w, &http.Cookie{
		Name:  "session_token",
		Value: sessionToken,
	})
	return nil
}

func (s *Site) register(username string, password string) error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	s.db.AddUser(username, string(hashedPassword))
	return nil
}
