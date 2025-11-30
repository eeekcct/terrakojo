package main

import (
	"log"
	"net/http"

	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
	"github.com/eeekcct/terrakojo/internal/webhook"
)

func main() {
	client, err := kubernetes.NewClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	handler, err := webhook.NewHandler(config.LoadConfig(), client)
	if err != nil {
		log.Fatalf("Failed to create webhook handler: %v", err)
	}
	addr := ":8080"
	log.Println("Starting server on ", addr)

	http.Handle("/webhook", handler)
	log.Fatal(http.ListenAndServe(addr, nil))
}
