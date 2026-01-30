package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codeberg.org/meadowingc/mire/constants"
	"github.com/fatih/color"
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
	router.Use(Logger)

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
	router.Get("/split", s.splitFeedHandler)

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

// Logger is a custom middleware that logs HTTP requests without IP addresses
// for GDPR compliance
func Logger(next http.Handler) http.Handler {
	// Define color functions
	gray := color.New(color.FgHiBlack).SprintFunc()
	blue := color.New(color.FgBlue).SprintFunc()
	magenta := color.New(color.FgMagenta).SprintFunc()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer wrapper to capture status code
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Process the request
		next.ServeHTTP(ww, r)

		// Log the request details (without IP address for GDPR compliance)
		duration := time.Since(start)

		// Determine log level and status color based on status code
		var logLevel string
		var statusStr string
		switch {
		case ww.statusCode >= 500:
			logLevel = "ERROR"
			statusStr = color.New(color.FgRed).Sprintf("%d", ww.statusCode)
		case ww.statusCode >= 400:
			logLevel = "WARN"
			statusStr = color.New(color.FgYellow).Sprintf("%d", ww.statusCode)
		case ww.statusCode >= 300:
			logLevel = "INFO"
			statusStr = color.New(color.FgCyan).Sprintf("%d", ww.statusCode)
		case ww.statusCode >= 200:
			logLevel = "INFO"
			statusStr = color.New(color.FgGreen).Sprintf("%d", ww.statusCode)
		default:
			logLevel = "INFO"
			statusStr = color.New(color.FgWhite).Sprintf("%d", ww.statusCode)
		}

		// Format duration with appropriate color
		var durationStr string
		if duration > 500*time.Millisecond {
			durationStr = color.New(color.FgRed).Sprintf("%v", duration)
		} else if duration > 100*time.Millisecond {
			durationStr = color.New(color.FgYellow).Sprintf("%v", duration)
		} else {
			durationStr = color.New(color.FgGreen).Sprintf("%v", duration)
		}

		// Format response size
		var sizeStr string
		if ww.bytesWritten > 1024*1024 {
			sizeStr = fmt.Sprintf("%.1fMB", float64(ww.bytesWritten)/(1024*1024))
		} else if ww.bytesWritten > 1024 {
			sizeStr = fmt.Sprintf("%.1fKB", float64(ww.bytesWritten)/1024)
		} else {
			sizeStr = fmt.Sprintf("%dB", ww.bytesWritten)
		}

		log.Printf("%s %s %s %s %s %s",
			gray(fmt.Sprintf("[%s]", logLevel)), // [INFO] in gray
			blue(r.Method),                      // GET in blue
			magenta(r.URL.Path),                 // /path in magenta
			statusStr,                           // 200 in appropriate color
			durationStr,                         // 2ms in appropriate color
			gray(fmt.Sprintf("(%s)", sizeStr)),  // (1.2KB) in gray
		)
	})
}

// responseWriter is a wrapper to capture the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	wroteHeader  bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.statusCode = code
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}
