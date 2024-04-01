package main

import (
	"log"
	"net/http"

	"codeberg.org/meadowingc/mire/constants"
)

func main() {
	if constants.DEBUG_MODE {
		log.Println("main: running in debug mode")
	} else {
		log.Println("main: running in release mode")
	}

	s := New()

	http.HandleFunc("GET /{$}", s.indexHandler)
	http.HandleFunc("GET /about", s.aboutHandler)
	http.HandleFunc("GET /u/{username}/{$}", s.userHandler)
	http.HandleFunc("GET /u/{username}/blogroll", s.userBlogrollHandler)
	http.HandleFunc("GET /static/{file}", s.staticHandler)
	http.HandleFunc("GET /global", s.globalHandler)
	http.HandleFunc("GET /random", s.visitRandomPostHandler)
	http.HandleFunc("GET /settings", s.settingsHandler)
	http.HandleFunc("POST /settings/submit", s.settingsSubmitHandler)
	http.HandleFunc("GET /login", s.loginHandler)
	http.HandleFunc("POST /login", s.loginHandler)
	http.HandleFunc("GET /logout", s.logoutHandler)
	http.HandleFunc("POST /logout", s.logoutHandler)
	http.HandleFunc("POST /register", s.registerHandler)
	http.HandleFunc("GET /feeds/{url}", s.feedDetailsHandler)

	// api functions
	http.HandleFunc("POST /api/v1/set-post-status/{postUrl}", s.apiSetPostReadStatus)
	http.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	// left in-place for backwards compat
	http.HandleFunc("GET /feeds", s.settingsHandler)
	http.HandleFunc("POST /feeds/submit", s.settingsSubmitHandler)

	go statsCalculatorProcess(s)

	log.Println("main: listening on http://localhost:5544")
	log.Fatal(http.ListenAndServe(":5544", nil))
}
