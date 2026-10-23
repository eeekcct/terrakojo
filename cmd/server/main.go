package main

import (
	"log"
	"net/http"
	"time"

	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
	"github.com/eeekcct/terrakojo/internal/webhook"
)

func main() {
	cfg := config.LoadConfig()
	if cfg.WebhookGithubSecret == "" {
		log.Fatal("GITHUB_WEBHOOK_SECRET must not be empty")
	}

	client, err := kubernetes.NewClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	handler, err := webhook.NewHandler(cfg, client)
	if err != nil {
		log.Fatalf("Failed to create webhook handler: %v", err)
	}
	server := newHTTPServer(handler)
	log.Println("Starting server on ", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func newHTTPServer(handler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	return &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
