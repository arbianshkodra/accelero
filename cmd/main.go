package main

import (
	"log"
	"net/http"
	"os"

	"github.com/arbianshkodra/accelero/internal/handler"

	"github.com/gorilla/mux"
)

func main() {
	// Load environment variables
	requiredEnvVars := []string{"REPO_URL", "REPO_USERNAME", "REPO_TOKEN", "COMPOSE_PATH"}
	for _, envVar := range requiredEnvVars {
		if os.Getenv(envVar) == "" {
			log.Fatalf("Environment variable %s must be set", envVar)
		}
	}

	r := mux.NewRouter()
	r.HandleFunc("/webhook", handler.Webhook).Methods("POST")
	http.Handle("/", r)

	log.Println("Starting server on :8000")
	if err := http.ListenAndServe(":8000", nil); err != nil {
		log.Fatalf("could not listen on port 8000 %v", err)
	}
}
