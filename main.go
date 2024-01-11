package main

import (
	"log"
	"net/http"

	"github.com/jba/muxpatterns"
)

func main() {
	s := New()
	mux := muxpatterns.NewServeMux()

	mux.HandleFunc("GET /{$}", s.indexHandler)
	mux.HandleFunc("GET /about", s.aboutHandler)
	mux.HandleFunc("GET /{username}", s.userHandler)
	mux.HandleFunc("GET /static/{file}", s.staticHandler)
	mux.HandleFunc("GET /global", s.globalHandler)
	mux.HandleFunc("GET /random", s.visitRandomPostHandler)
	mux.HandleFunc("GET /settings", s.settingsHandler)
	mux.HandleFunc("POST /settings/submit", s.settingsSubmitHandler)
	mux.HandleFunc("GET /login", s.loginHandler)
	mux.HandleFunc("POST /login", s.loginHandler)
	mux.HandleFunc("GET /logout", s.logoutHandler)
	mux.HandleFunc("POST /logout", s.logoutHandler)
	mux.HandleFunc("POST /register", s.registerHandler)
	mux.HandleFunc("GET /feeds/{url}", s.feedDetailsHandler)

	// api functions
	mux.HandleFunc("POST /api/v1/set-post-status/{postUrl}", s.apiSetPostReadStatus)

	// left in-place for backwards compat
	mux.HandleFunc("GET /feeds", s.settingsHandler)
	mux.HandleFunc("POST /feeds/submit", s.settingsSubmitHandler)

	log.Println("main: listening on http://localhost:5544")
	log.Fatal(http.ListenAndServe(":5544", mux))
}
