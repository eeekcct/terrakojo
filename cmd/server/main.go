package main

import (
	"log"
	"net/http"

	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/eeekcct/terrakojo/internal/webhook"
)

func main() {
	handler, err := webhook.NewHandler(config.LoadConfig(), nil)
	if err != nil {
		log.Fatalf("Failed to create webhook handler: %v", err)
	}
	addr := ":8080"
	log.Println("Starting server on ", addr)

	http.Handle("/webhook", handler)
	log.Fatal(http.ListenAndServe(addr, nil))
}
