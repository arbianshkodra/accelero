package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/arbianshkodra/accelero/internal/handler"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/docker/docker/client"
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
		logrus.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: "2006-01-02T15:04:05",
		})
	} else {
		// Use the text formatter as default
		logrus.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02T15:04:05",
		})
	}
}

func main() {
	// Load environment variables
	requiredEnvVars := []string{"REPO_URL", "REPO_USERNAME", "REPO_TOKEN", "COMPOSE_PATH"}
	for _, envVar := range requiredEnvVars {
		if value := os.Getenv(envVar); value == "" {
			logrus.Fatalf("Environment variable %s must be set", envVar)
		}
	}

	// Create a context that can be used to shutdown the worker pool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize dynamic worker pool based on system resources
	numWorkers := calculateOptimalWorkers()
	taskQueueSize := calculateOptimalQueueSize(numWorkers)
	taskQueue := make(chan handler.WebhookTask, taskQueueSize)
	
	logrus.Infof("Starting worker pool with %d workers and queue size %d", numWorkers, taskQueueSize)
	handler.StartWorkerPool(ctx, numWorkers, taskQueue)

	// Create Docker client
	dockerSock := os.Getenv("DOCKER_SOCK")
	if dockerSock == "" {
		dockerSock = "unix:///var/run/docker.sock"
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerSock),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		logrus.Fatalf("Failed to create docker client: %v", err)
	}

	// Check if Docker daemon is actually running and accessible
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		logrus.Fatalf("Cannot connect to the Docker daemon at %s. Is the docker daemon running? Error: %v", dockerSock, err)
	}
	logrus.Info("Connected to Docker daemon successfully")

	// Start cleanup routine
	startCleanupRoutine(cli)

	// Set up the HTTP server
	r := mux.NewRouter()

	// Apply the authentication middleware to protected routes
	apiRouter := r.PathPrefix("/").Subrouter()
	apiRouter.Use(middleware.APIKeyAuth)

	apiRouter.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		handler.Webhook(w, r, taskQueue)
	}).Methods("POST")

	// Add the status endpoint
	apiRouter.HandleFunc("/status", handler.StatusHandler).Methods("GET")

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

// startCleanupRoutine starts a goroutine that periodically cleans up Docker resources
func startCleanupRoutine(cli *client.Client) {
	ticker := time.NewTicker(24 * time.Hour) // Adjust the interval as needed
	go func() {
		for range ticker.C {
			logrus.Info("Starting cleanup of Docker resources")
			if err := service.CleanupResources(cli); err != nil {
				logrus.Errorf("Failed to clean up Docker resources: %v", err)
			} else {
				logrus.Info("Docker resources cleaned up successfully")
			}
		}
	}()
}

// calculateOptimalWorkers determines the optimal number of workers based on system resources
func calculateOptimalWorkers() int {
	// Check for environment variable override
	if workerStr := os.Getenv("WORKER_COUNT"); workerStr != "" {
		if workers, err := strconv.Atoi(workerStr); err == nil && workers > 0 {
			logrus.Infof("Using custom worker count from WORKER_COUNT: %d", workers)
			return workers
		}
	}

	// Calculate based on CPU cores
	cpuCores := runtime.NumCPU()
	
	// Base calculation: 2 workers per CPU core for I/O intensive tasks
	workers := cpuCores * 2
	
	// Set reasonable bounds
	const minWorkers = 2
	const maxWorkers = 50
	
	if workers < minWorkers {
		workers = minWorkers
	} else if workers > maxWorkers {
		workers = maxWorkers
	}
	
	logrus.Infof("Calculated %d workers based on %d CPU cores", workers, cpuCores)
	return workers
}

// calculateOptimalQueueSize determines the optimal queue size based on worker count
func calculateOptimalQueueSize(numWorkers int) int {
	// Check for environment variable override
	if queueStr := os.Getenv("QUEUE_SIZE"); queueStr != "" {
		if queueSize, err := strconv.Atoi(queueStr); err == nil && queueSize > 0 {
			logrus.Infof("Using custom queue size from QUEUE_SIZE: %d", queueSize)
			return queueSize
		}
	}

	// Calculate queue size: 10-20 tasks per worker
	queueSize := numWorkers * 15
	
	// Set reasonable bounds
	const minQueueSize = 50
	const maxQueueSize = 1000
	
	if queueSize < minQueueSize {
		queueSize = minQueueSize
	} else if queueSize > maxQueueSize {
		queueSize = maxQueueSize
	}
	
	logrus.Infof("Calculated queue size %d based on %d workers", queueSize, numWorkers)
	return queueSize
}
