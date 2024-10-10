package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/arbianshkodra/accelero/internal/compose"
	"github.com/arbianshkodra/accelero/internal/git"
	"github.com/sirupsen/logrus"
)

// Define a struct for tasks
type WebhookTask struct {
	RequestID string
	Payload   []byte
	Timestamp time.Time
}

// StartWorkerPool initializes the worker pool
func StartWorkerPool(ctx context.Context, numWorkers int, taskQueue <-chan WebhookTask) {
	for i := 0; i < numWorkers; i++ {
		go worker(ctx, i, taskQueue)
	}
}

// Worker function to process tasks
func worker(ctx context.Context, id int, taskQueue <-chan WebhookTask) {
	for {
		select {
		case <-ctx.Done():
			logrus.Infof("Worker %d stopping", id)
			return
		case task, ok := <-taskQueue:
			if !ok {
				logrus.Infof("Worker %d: task queue closed", id)
				return
			}
			processTask(ctx, task)
		}
	}
}

// Process an individual task
func processTask(ctx context.Context, task WebhookTask) {
	defer func() {
		if r := recover(); r != nil {
			logrus.Errorf("Recovered from panic in task %s: %v", task.RequestID, r)
		}
	}()

	select {
	case <-ctx.Done():
		logrus.Infof("Task %s cancelled before starting", task.RequestID)
		return
	default:
	}

	// Use a context with timeout for task processing
	taskCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	repoDir := "./data_" + task.RequestID

	// Clone the repository
	if err := git.CloneRepo(taskCtx, repoDir); err != nil {
		logrus.Errorf("Error cloning repository for task %s: %v", task.RequestID, err)
		return
	}

	// Run Docker Compose
	if err := compose.RunDockerCompose(taskCtx, repoDir); err != nil {
		logrus.Errorf("Error running docker-compose for task %s: %v", task.RequestID, err)
		return
	}

	// Clean up the repository directory after processing
	if err := os.RemoveAll(repoDir); err != nil {
		logrus.Warnf("Failed to remove directory %s: %v", repoDir, err)
	}

	logrus.Infof("Task %s completed successfully", task.RequestID)
}

// Modified Webhook handler to enqueue tasks
func Webhook(w http.ResponseWriter, r *http.Request, taskQueue chan<- WebhookTask) {
	// Generate a unique request ID (e.g., UUID)
	requestID := generateRequestID()

	// Read the request payload if necessary
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Create a new task
	task := WebhookTask{
		RequestID: requestID,
		Payload:   payload,
		Timestamp: time.Now(),
	}

	// Enqueue the task
	select {
	case taskQueue <- task:
		logrus.Infof("Enqueued task %s", requestID)
	default:
		logrus.Warnf("Task queue is full, rejecting task %s", requestID)
		http.Error(w, "Server is busy, try again later", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": requestID}); err != nil {
		logrus.Errorf("Failed to write response: %v", err)
	}
}

// Utility function to generate a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
