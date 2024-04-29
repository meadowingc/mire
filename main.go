package main

import (
	"log"
	"net/http"

	"codeberg.org/meadowingc/mire/constants"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	if constants.DEBUG_MODE {
		log.Println("main: running in debug mode")
	} else {
		log.Println("main: running in release mode")
	}

	s := New()
	router := buildRouter(s)

	go statsCalculatorProcess(s)

	log.Println("main: listening on http://localhost:5544")
	log.Fatal(http.ListenAndServe(":5544", router))
}

func buildRouter(s *Site) *chi.Mux {
	router := chi.NewRouter()
	router.Use(middleware.Logger)

	// if constants.DEBUG_MODE {
	//   router.Use(middleware.Logger)
	// }

	router.Use(middleware.Heartbeat("/ping"))
	router.Use(middleware.SetHeader("X-Clacks-Overhead", "GNU Terry Pratchett"))

	// router.Use(middleware.Compress())
	router.Use(middleware.NoCache)
	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)

	router.Get("/", s.indexHandler)
	router.Get("/about", s.aboutHandler)
	router.Get("/u/{username}", s.userHandler)
	router.Get("/u/{username}/blogroll", s.userBlogrollHandler)
	router.Get("/static/{file}", s.staticHandler)
	router.Get("/global", s.globalHandler)
	router.Get("/random", s.visitRandomPostHandler)
	router.Get("/settings", s.settingsHandler)
	router.Post("/settings/subscribe", s.settingsSubscribeHandler)
	router.Post("/settings/preferences", s.settingsPreferencesHandler)
	router.Get("/login", s.loginHandler)
	router.Post("/login", s.loginHandler)
	router.Get("/logout", s.logoutHandler)
	router.Post("/logout", s.logoutHandler)
	router.Post("/register", s.registerHandler)
	router.Get("/feeds/{url}", s.feedDetailsHandler)

	// api functions
	router.Post("/api/v1/set-post-read-status/{postUrl}", s.apiSetPostReadStatus)
	router.Get("/api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	return router
}
