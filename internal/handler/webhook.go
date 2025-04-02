package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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

// DeploymentStatus represents the current status of a deployment
type DeploymentStatus struct {
	RequestID    string    `json:"request_id"`
	ServiceName  string    `json:"service_name"`
	Status       string    `json:"status"` // pending, in_progress, completed, failed, rolled_back
	StartTime    time.Time `json:"start_time"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	ImageTag     string    `json:"image_tag,omitempty"`
}

// Global map to track deployment statuses
var (
	deploymentStatuses = make(map[string]*DeploymentStatus)
	statusMutex        sync.RWMutex
)

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
			updateDeploymentStatus(task.RequestID, "failed", fmt.Sprintf("Panic: %v", r), "")
		}
	}()

	select {
	case <-ctx.Done():
		logrus.Infof("Task %s cancelled before starting", task.RequestID)
		updateDeploymentStatus(task.RequestID, "cancelled", "Task cancelled before starting", "")
		return
	default:
	}

	// Use a context with timeout for task processing
	taskCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	repoDir := "./data_" + task.RequestID

	// Create initial deployment status
	createDeploymentStatus(task.RequestID, "pending", "")

	// Update status to in_progress
	updateDeploymentStatus(task.RequestID, "in_progress", "", "")

	// Clone the repository
	if err := git.CloneRepo(taskCtx, repoDir); err != nil {
		logrus.Errorf("Error cloning repository for task %s: %v", task.RequestID, err)
		updateDeploymentStatus(task.RequestID, "failed", fmt.Sprintf("Git clone error: %v", err), "")
		return
	}

	// Run Docker Compose
	if err := compose.RunDockerCompose(taskCtx, repoDir); err != nil {
		logrus.Errorf("Error running docker-compose for task %s: %v", task.RequestID, err)
		// The error might already include information about a rollback attempt
		if isRollbackError(err) {
			updateDeploymentStatus(task.RequestID, "rolled_back", err.Error(), "")
		} else {
			updateDeploymentStatus(task.RequestID, "failed", fmt.Sprintf("Deployment error: %v", err), "")
		}
		return
	}

	// Clean up the repository directory after processing
	if err := os.RemoveAll(repoDir); err != nil {
		logrus.Warnf("Failed to remove directory %s: %v", repoDir, err)
	}

	updateDeploymentStatus(task.RequestID, "completed", "", "")
	logrus.Infof("Task %s completed successfully", task.RequestID)
}

// Helper to check if an error indicates a rollback was attempted
func isRollbackError(err error) bool {
	return err != nil && (strings.Contains(fmt.Sprint(err), "rolled back") ||
		strings.Contains(fmt.Sprint(err), "rollback"))
}

// Create a new deployment status
func createDeploymentStatus(requestID, status, serviceName string) {
	statusMutex.Lock()
	defer statusMutex.Unlock()

	deploymentStatuses[requestID] = &DeploymentStatus{
		RequestID:   requestID,
		ServiceName: serviceName,
		Status:      status,
		StartTime:   time.Now(),
	}
}

// Update an existing deployment status
func updateDeploymentStatus(requestID, status, errorMsg, imageTag string) {
	statusMutex.Lock()
	defer statusMutex.Unlock()

	if status == "completed" || status == "failed" || status == "rolled_back" {
		if ds, exists := deploymentStatuses[requestID]; exists {
			ds.Status = status
			ds.ErrorMessage = errorMsg
			ds.CompletedAt = time.Now()
			ds.ImageTag = imageTag
		}
	} else {
		if ds, exists := deploymentStatuses[requestID]; exists {
			ds.Status = status
			ds.ErrorMessage = errorMsg
			ds.ImageTag = imageTag
		}
	}
}

// GetDeploymentStatus returns the current status of a deployment
func GetDeploymentStatus(requestID string) *DeploymentStatus {
	statusMutex.RLock()
	defer statusMutex.RUnlock()

	if status, exists := deploymentStatuses[requestID]; exists {
		return status
	}
	return nil
}

// GetAllDeploymentStatuses returns all deployment statuses
func GetAllDeploymentStatuses() []*DeploymentStatus {
	statusMutex.RLock()
	defer statusMutex.RUnlock()

	statuses := make([]*DeploymentStatus, 0, len(deploymentStatuses))
	for _, status := range deploymentStatuses {
		statuses = append(statuses, status)
	}
	return statuses
}

// Modified Webhook handler to enqueue tasks
func Webhook(w http.ResponseWriter, r *http.Request, taskQueue chan<- WebhookTask) {
	// Generate a unique request ID (e.g., UUID)
	requestID := generateRequestID()

	// Validate HTTP method
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate content type if necessary
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}

	// Read the request payload if necessary
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Validate payload structure if expecting JSON
	var data map[string]interface{}
	if err := json.Unmarshal(payload, &data); err != nil {
		logrus.Errorf("Invalid JSON payload: %v", err)
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Create a new task
	task := WebhookTask{
		RequestID: requestID,
		Payload:   payload,
		Timestamp: time.Now(),
	}

	// Create initial deployment status
	createDeploymentStatus(requestID, "pending", "")

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
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status":     "accepted",
		"request_id": requestID,
		"message":    "Deployment queued. Use the request_id to check status.",
	}); err != nil {
		logrus.Errorf("Failed to write response: %v", err)
	}
}

// Add a new status endpoint
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	// Extract request ID from query params if provided
	requestID := r.URL.Query().Get("id")

	var response interface{}
	if requestID != "" {
		status := GetDeploymentStatus(requestID)
		if status == nil {
			http.Error(w, "Deployment not found", http.StatusNotFound)
			return
		}
		response = status
	} else {
		response = GetAllDeploymentStatuses()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logrus.Errorf("Failed to encode status response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// Utility function to generate a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
