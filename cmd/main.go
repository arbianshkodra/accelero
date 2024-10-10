package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// Create a context that can be used to shutdown the worker pool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize the worker pool
	numWorkers := 5 // Adjust this number based on your needs
	taskQueue := make(chan handler.WebhookTask, 100)
	handler.StartWorkerPool(ctx, numWorkers, taskQueue)

	// Set up the HTTP server
	r := mux.NewRouter()
	r.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		handler.Webhook(w, r, taskQueue)
	}).Methods("POST")

	server := &http.Server{
		Addr:    ":8000",
		Handler: r,
	}

	// Set up signal handling to gracefully shut down
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		logrus.Info("Shutting down...")
		cancel()

		// Close the task queue to unblock workers waiting on it
		close(taskQueue)

		// Create a context with timeout for the server shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logrus.Errorf("HTTP server Shutdown: %v", err)
		} else {
			logrus.Info("HTTP server stopped")
		}
	}()

	logrus.Info("Starting server on :8000")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Fatalf("Could not listen on port 8000: %v", err)
	}

	logrus.Info("Application exited")
}
