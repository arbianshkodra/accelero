package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/arbianshkodra/accelero/internal/compose"
	"github.com/arbianshkodra/accelero/internal/git"
	"github.com/sirupsen/logrus"
)

// Security constants
const (
	MaxPayloadSize = 1024 * 1024 // 1MB max payload size
	MaxRequestIDLength = 64
)

// Validation patterns
var (
	requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
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

// StartWorkerPool initializes the worker pool and status cleanup routine
func StartWorkerPool(ctx context.Context, numWorkers int, taskQueue <-chan WebhookTask) {
	for i := 0; i < numWorkers; i++ {
		go worker(ctx, i, taskQueue)
	}
	
	// Start the status cleanup routine
	go statusCleanupRoutine(ctx)
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

	// Create secure directory path to prevent directory traversal
	repoDir, err := createSecureRepoDir(task.RequestID)
	if err != nil {
		logrus.Errorf("Error creating secure directory for task %s: %v", task.RequestID, err)
		updateDeploymentStatus(task.RequestID, "failed", fmt.Sprintf("Directory creation error: %v", err), "")
		return
	}

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
	requestID, err := generateSecureRequestID()
	if err != nil {
		logrus.Errorf("Failed to generate request ID: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Validate HTTP method
	if r.Method != http.MethodPost {
		logrus.Warnf("Invalid HTTP method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate content type
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		logrus.Warnf("Invalid content type %s from %s", contentType, r.RemoteAddr)
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Limit payload size to prevent DoS attacks
	r.Body = http.MaxBytesReader(w, r.Body, MaxPayloadSize)

	// Read the request payload with size limit
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Validate payload is not empty
	if len(payload) == 0 {
		logrus.Warnf("Empty payload from %s", r.RemoteAddr)
		http.Error(w, "Empty payload", http.StatusBadRequest)
		return
	}

	// Validate payload structure - must be valid JSON
	var data map[string]interface{}
	if err := json.Unmarshal(payload, &data); err != nil {
		logrus.Errorf("Invalid JSON payload from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Optional: Validate required fields in payload
	if err := validatePayloadStructure(data); err != nil {
		logrus.Errorf("Invalid payload structure from %s: %v", r.RemoteAddr, err)
		http.Error(w, fmt.Sprintf("Invalid payload structure: %v", err), http.StatusBadRequest)
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

// Add a new status endpoint with input validation
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	// Only allow GET requests
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract query parameters
	requestID := r.URL.Query().Get("id")
	showStats := r.URL.Query().Get("stats") == "true"

	var response interface{}
	if requestID != "" {
		// Validate request ID format and length
		if err := validateRequestID(requestID); err != nil {
			logrus.Warnf("Invalid request ID format from %s: %v", r.RemoteAddr, err)
			http.Error(w, "Invalid request ID format", http.StatusBadRequest)
			return
		}

		status := GetDeploymentStatus(requestID)
		if status == nil {
			http.Error(w, "Deployment not found", http.StatusNotFound)
			return
		}
		response = status
	} else if showStats {
		response = getStatusStatistics()
	} else {
		response = GetAllDeploymentStatuses()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logrus.Errorf("Failed to encode status response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// generateSecureRequestID generates a cryptographically secure request ID
func generateSecureRequestID() (string, error) {
	bytes := make([]byte, 16) // 128-bit random value
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// validateRequestID validates the format and length of request IDs
func validateRequestID(requestID string) error {
	if len(requestID) == 0 {
		return fmt.Errorf("request ID cannot be empty")
	}
	if len(requestID) > MaxRequestIDLength {
		return fmt.Errorf("request ID too long (max %d characters)", MaxRequestIDLength)
	}
	if !requestIDPattern.MatchString(requestID) {
		return fmt.Errorf("request ID contains invalid characters")
	}
	return nil
}

// createSecureRepoDir creates a secure directory path to prevent directory traversal
func createSecureRepoDir(requestID string) (string, error) {
	// Validate request ID first
	if err := validateRequestID(requestID); err != nil {
		return "", fmt.Errorf("invalid request ID: %w", err)
	}

	// Create a secure base directory
	baseDir := "./data"
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create base directory: %w", err)
	}

	// Create secure subdirectory using cleaned request ID
	repoDir := filepath.Join(baseDir, "deployment_"+requestID)
	
	// Ensure the path is within our base directory (prevent directory traversal)
	absRepoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}
	
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base directory: %w", err)
	}
	
	if !strings.HasPrefix(absRepoDir, absBaseDir) {
		return "", fmt.Errorf("directory traversal attempt detected")
	}

	return repoDir, nil
}

// validatePayloadStructure validates the webhook payload structure
func validatePayloadStructure(data map[string]interface{}) error {
	// This is a basic validation - extend based on your webhook requirements
	// For example, if you expect specific fields:
	
	// Example validation (uncomment and modify as needed):
	// if _, ok := data["repository"]; !ok {
	//     return fmt.Errorf("missing required field: repository")
	// }
	
	// Add more validation as needed for your specific webhook format
	return nil
}

// statusCleanupRoutine periodically cleans up old deployment statuses to prevent memory leaks
func statusCleanupRoutine(ctx context.Context) {
	// Configurable cleanup interval (default: 1 hour)
	cleanupInterval := getCleanupInterval()
	
	// Configurable max age for deployment statuses (default: 24 hours)
	maxAge := getStatusMaxAge()
	
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	
	logrus.Infof("Starting status cleanup routine (interval: %v, max age: %v)", cleanupInterval, maxAge)
	
	for {
		select {
		case <-ctx.Done():
			logrus.Info("Status cleanup routine stopping")
			return
		case <-ticker.C:
			cleanupOldStatuses(maxAge)
		}
	}
}

// cleanupOldStatuses removes deployment statuses older than the specified age
func cleanupOldStatuses(maxAge time.Duration) {
	statusMutex.Lock()
	defer statusMutex.Unlock()
	
	now := time.Now()
	initialCount := len(deploymentStatuses)
	removedCount := 0
	
	for requestID, status := range deploymentStatuses {
		// Calculate age based on completion time if available, otherwise start time
		statusAge := now.Sub(status.StartTime)
		if !status.CompletedAt.IsZero() {
			statusAge = now.Sub(status.CompletedAt)
		}
		
		if statusAge > maxAge {
			delete(deploymentStatuses, requestID)
			removedCount++
		}
	}
	
	if removedCount > 0 {
		logrus.Infof("Cleaned up %d old deployment statuses (total: %d -> %d)", 
			removedCount, initialCount, len(deploymentStatuses))
	} else {
		logrus.Debugf("No old deployment statuses to clean up (total: %d)", len(deploymentStatuses))
	}
}

// GetStatusMapSize returns the current size of the deployment status map (for monitoring)
func GetStatusMapSize() int {
	statusMutex.RLock()
	defer statusMutex.RUnlock()
	return len(deploymentStatuses)
}

// getCleanupInterval returns the configurable cleanup interval
func getCleanupInterval() time.Duration {
	if intervalStr := os.Getenv("STATUS_CLEANUP_INTERVAL"); intervalStr != "" {
		if duration, err := time.ParseDuration(intervalStr); err == nil {
			logrus.Infof("Using custom status cleanup interval: %v", duration)
			return duration
		} else {
			logrus.Warnf("Invalid STATUS_CLEANUP_INTERVAL '%s', using default: 1h", intervalStr)
		}
	}
	return 1 * time.Hour
}

// getStatusMaxAge returns the configurable max age for status cleanup
func getStatusMaxAge() time.Duration {
	if maxAgeStr := os.Getenv("STATUS_MAX_AGE"); maxAgeStr != "" {
		if duration, err := time.ParseDuration(maxAgeStr); err == nil {
			logrus.Infof("Using custom status max age: %v", duration)
			return duration
		} else {
			logrus.Warnf("Invalid STATUS_MAX_AGE '%s', using default: 24h", maxAgeStr)
		}
	}
	return 24 * time.Hour
}

// StatusStatistics represents memory and performance statistics
type StatusStatistics struct {
	TotalStatuses     int                    `json:"total_statuses"`
	StatusBreakdown   map[string]int         `json:"status_breakdown"`
	OldestStatus      *time.Time             `json:"oldest_status,omitempty"`
	NewestStatus      *time.Time             `json:"newest_status,omitempty"`
	CleanupInterval   string                 `json:"cleanup_interval"`
	StatusMaxAge      string                 `json:"status_max_age"`
	MemoryUsageBytes  int                    `json:"estimated_memory_bytes"`
}

// getStatusStatistics returns memory and performance statistics
func getStatusStatistics() StatusStatistics {
	statusMutex.RLock()
	defer statusMutex.RUnlock()
	
	stats := StatusStatistics{
		TotalStatuses:   len(deploymentStatuses),
		StatusBreakdown: make(map[string]int),
		CleanupInterval: getCleanupInterval().String(),
		StatusMaxAge:    getStatusMaxAge().String(),
	}
	
	var oldestTime, newestTime *time.Time
	memoryUsage := 0
	
	for _, status := range deploymentStatuses {
		// Count by status
		stats.StatusBreakdown[status.Status]++
		
		// Track oldest and newest
		if oldestTime == nil || status.StartTime.Before(*oldestTime) {
			oldestTime = &status.StartTime
		}
		if newestTime == nil || status.StartTime.After(*newestTime) {
			newestTime = &status.StartTime
		}
		
		// Estimate memory usage (rough calculation)
		memoryUsage += len(status.RequestID) + len(status.ServiceName) + 
					   len(status.Status) + len(status.ErrorMessage) + 
					   len(status.ImageTag) + 100 // overhead for struct fields
	}
	
	stats.OldestStatus = oldestTime
	stats.NewestStatus = newestTime
	stats.MemoryUsageBytes = memoryUsage
	
	return stats
}
