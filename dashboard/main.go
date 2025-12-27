package main

import (
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/kubeden/clopus-watcher/dashboard/db"
	"github.com/kubeden/clopus-watcher/dashboard/handlers"
)

func main() {
	sqlitePath := os.Getenv("SQLITE_PATH")
	if sqlitePath == "" {
		sqlitePath = "/data/watcher.db"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logPath := os.Getenv("LOG_PATH")
	if logPath == "" {
		logPath = "/data/watcher.log"
	}

	database, err := db.New(sqlitePath)
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

	h := handlers.New(database, tmpl, logPath)

	// Page routes
	http.HandleFunc("/", h.Index)

	// HTMX partial routes
	http.HandleFunc("/partials/runs", h.RunsList)
	http.HandleFunc("/partials/run", h.RunDetail)
	http.HandleFunc("/partials/stats", h.Stats)
	http.HandleFunc("/partials/log", h.LiveLog)

	// API routes
	http.HandleFunc("/api/namespaces", h.APINamespaces)
	http.HandleFunc("/api/runs", h.APIRuns)
	http.HandleFunc("/api/run", h.APIRun)

	// Health check
	http.HandleFunc("/health", h.Health)

	log.Printf("Dashboard starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
