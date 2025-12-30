package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kubeden/clopus-watcher/dashboard/db"
	"github.com/kubeden/clopus-watcher/dashboard/handlers"
)

// SessionMiddleware validates NextAuth session from Platform
// On localhost, we just check for session cookie presence and basic format
func SessionMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks and login routes
		if r.URL.Path == "/health" || r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/api/") {
			handler(w, r)
			return
		}

		// Check for NextAuth session cookie (try both secure and non-secure names)
		sessionExists := false

		// Try secure version first (production)
		if cookie, err := r.Cookie("__Secure-next-auth.session-token"); err == nil && cookie.Value != "" {
			sessionExists = true
		}

		// Try non-secure version (development/localhost)
		if !sessionExists {
			if cookie, err := r.Cookie("next-auth.session-token"); err == nil && cookie.Value != "" {
				sessionExists = true
			}
		}

		// If no session found, redirect to Platform login
		if !sessionExists {
			log.Printf("No session cookie found for %s - redirecting to Platform login", r.RemoteAddr)
			redirectToPlatformLogin(w, r)
			return
		}

		// Session exists - allow access
		handler(w, r)
	}
}

// redirectToPlatformLogin builds the login URL and redirects
func redirectToPlatformLogin(w http.ResponseWriter, r *http.Request) {
	platformURL := os.Getenv("PLATFORM_URL")
	if platformURL == "" {
		platformURL = "http://localhost:3000"
	}

	// Build the full return URL: scheme://host/path?query
	returnURL := buildFullURL(r)

	// Build login URL with redirect parameter
	loginURLObj, _ := url.Parse(platformURL)
	loginURLObj.Path = "/login"
	q := loginURLObj.Query()
	q.Set("redirect", returnURL)
	loginURLObj.RawQuery = q.Encode()

	log.Printf("Redirecting unauthenticated request to Platform login")
	http.Redirect(w, r, loginURLObj.String(), http.StatusFound)
}

// buildFullURL constructs the full URL from the request
func buildFullURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	// Use r.Host (which includes the port)
	host := r.Host
	path := r.RequestURI

	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}

// LoginHandler redirects to Platform login
// If called directly at /login, it redirects to Platform with a redirect param
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	platformURL := os.Getenv("PLATFORM_URL")
	if platformURL == "" {
		platformURL = "http://localhost:3000"
	}

	// Get the redirect parameter if provided
	redirectParam := r.URL.Query().Get("redirect")
	if redirectParam == "" {
		// Default to dashboard root if no redirect specified
		redirectParam = "http://localhost:3003/"
	}

	// Build login URL
	loginURLObj, _ := url.Parse(platformURL)
	loginURLObj.Path = "/login"
	q := loginURLObj.Query()
	q.Set("redirect", redirectParam)
	loginURLObj.RawQuery = q.Encode()

	log.Printf("Redirecting to Platform login (from /login handler)")
	http.Redirect(w, r, loginURLObj.String(), http.StatusFound)
}

func main() {
	// Use PostgreSQL via DATABASE_URL (from shared secrets)
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatalf("DATABASE_URL environment variable not set - required for PostgreSQL connection")
	}

	// Add SSL mode for local development (disable SSL for Docker/local postgres)
	if !strings.Contains(databaseURL, "sslmode") {
		if strings.Contains(databaseURL, "?") {
			databaseURL += "&sslmode=disable"
		} else {
			databaseURL += "?sslmode=disable"
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	database, err := db.New(databaseURL)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Template functions
	funcMap := template.FuncMap{
		"dict": func(values ...interface{}) map[string]interface{} {
			m := make(map[string]interface{})
			for i := 0; i < len(values); i += 2 {
				if i+1 < len(values) {
					m[values[i].(string)] = values[i+1]
				}
			}
			return m
		},
	}

	// Parse all templates together
	tmpl, err := template.New("").Funcs(funcMap).ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("Failed to parse templates: %v", err)
	}

	tmpl, err = tmpl.ParseGlob("templates/partials/*.html")
	if err != nil {
		log.Fatalf("Failed to parse partials: %v", err)
	}

	logPath := os.Getenv("LOG_PATH")
	if logPath == "" {
		logPath = "/tmp/clopus-watcher.log"
	}

	h := handlers.New(database, tmpl, logPath)

	// Login route (no auth required)
	http.HandleFunc("/login", LoginHandler)

	// Health check (no auth required) - simple endpoint for readiness probe
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	// Page routes (with auth)
	http.HandleFunc("/", SessionMiddleware(h.Index))

	// HTMX partial routes (with auth)
	http.HandleFunc("/partials/runs", SessionMiddleware(h.RunsList))
	http.HandleFunc("/partials/run", SessionMiddleware(h.RunDetail))
	http.HandleFunc("/partials/stats", SessionMiddleware(h.Stats))
	http.HandleFunc("/partials/log", SessionMiddleware(h.LiveLog))

	// API routes (no auth for local dev, add if needed)
	http.HandleFunc("/api/namespaces", h.APINamespaces)
	http.HandleFunc("/api/runs", h.APIRuns)
	http.HandleFunc("/api/run", h.APIRun)

	addr := ":" + port
	log.Printf("Dashboard starting on port %s with session validation", port)
	log.Printf("Listening on %s", addr)
	server := &http.Server{
		Addr:    addr,
		Handler: http.DefaultServeMux,
	}
	log.Fatal(server.ListenAndServe())
}
