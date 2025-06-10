package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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

	// Setup channel to listen for interrupt signal (ctrl+c)
	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt, syscall.SIGTERM)

	server := &http.Server{Addr: ":5544", Handler: router}
	go func() {
		log.Println("main: listening on http://localhost:5544")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// Block until we receive our interrupt
	<-interruptChan

	log.Println("main: shutting down server...")

	err := s.db.Close()
	if err != nil {
		log.Fatalf("main: database shutdown failed: %+v", err)
	}

	if err := server.Shutdown(context.TODO()); err != nil {
		log.Fatalf("main: server shutdown failed: %+v", err)
	}

	log.Println("main: server gracefully stopped")
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
	router.Get("/discover", s.discoverHandler)
	router.Get("/random", s.visitRandomPostHandler)
	router.Get("/settings", s.settingsHandler)
	router.Post("/settings/subscribe", s.settingsSubscribeHandler)
	router.Post("/settings/change-password", s.changePasswordHandler)
	router.Post("/settings/preferences", s.settingsPreferencesHandler)
	router.Get("/login", s.loginHandler)
	router.Post("/login", s.loginHandler)
	router.Get("/logout", s.logoutHandler)
	router.Post("/logout", s.logoutHandler)
	router.Post("/register", s.registerHandler)
	router.Get("/feeds/{url}", s.feedDetailsHandler)

	// api functions
	router.Post("/api/v1/set-post-read-status/{postUrl}", s.apiSetPostReadStatus)
	router.Post("/api/v1/toggle-favorite-feed-status/{feedUrl}", s.apiSetFavoriteFeedHandler)
	router.Post("/api/v1/toggle-subscription/{feedUrl}", s.apiToggleSubscriptionHandler)
	router.Get("/api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	// legacy redirects
	router.Get("/global", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/discover", http.StatusMovedPermanently)
	})

	return router
}
