package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/api"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/store/sqlite"
)

func main() {
	port := flag.Int("port", 8080, "HTTP listen port")
	dbPath := flag.String("db-path", "./cooldown.db", "Path to SQLite database file")
	cacheTTL := flag.Duration("cache-ttl", 24*time.Hour, "Cache TTL for user and PR data")
	tokenCacheTTL := flag.Duration("token-cache-ttl", 5*time.Minute, "Cache TTL for GitHub token validation")
	flag.Parse()

	// Initialize store
	store, err := sqlite.New(*dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer store.Close()

	// Initialize handlers and middleware
	handler := api.NewHandler(store, *cacheTTL)
	tokenValidator := api.NewTokenValidator(*tokenCacheTTL)

	// Set up routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handler.HealthCheck)
	mux.Handle("POST /check", tokenValidator.Middleware(http.HandlerFunc(handler.Check)))

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("PR Cooldown server starting on :%d", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	log.Println("Server stopped")
}
