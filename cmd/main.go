package main

import (
	"net/http"
	"os"

	"github.com/arbianshkodra/accelero/internal/handler"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

func init() {
	// Set the log level from the environment variable, default to INFO
	level, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)

	// Set the log format (JSON or text) based on an environment variable
	if os.Getenv("LOG_FORMAT") == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{})
	} else {
		// Use the text formatter as default
		logrus.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
		})
	}
}

func main() {
	// Load environment variables
	requiredEnvVars := []string{"REPO_URL", "REPO_USERNAME", "REPO_TOKEN", "COMPOSE_PATH"}
	for _, envVar := range requiredEnvVars {
		if os.Getenv(envVar) == "" {
			logrus.Fatalf("Environment variable %s must be set", envVar)
		}
	}

	r := mux.NewRouter()
	r.HandleFunc("/webhook", handler.Webhook).Methods("POST")
	http.Handle("/", r)

	logrus.Info("Starting server on :8000")
	if err := http.ListenAndServe(":8000", nil); err != nil {
		logrus.Fatalf("Could not listen on port 8000: %v", err)
	}
}
