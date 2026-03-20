package store

import "time"

// Stack represents a deployable unit — a git repo + compose file targeting a Docker host.
type Stack struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	RepoURL           string     `json:"repo_url"`
	RepoUsername      string     `json:"repo_username"`
	RepoToken         string     `json:"-"` // never serialize
	RepoBranch        string     `json:"repo_branch,omitempty"`
	ComposePath       string     `json:"compose_path"`
	ServiceFilter     string     `json:"service_filter,omitempty"`
	AutoDeploy        bool       `json:"auto_deploy"`
	ReconcileInterval int        `json:"reconcile_interval_seconds"` // seconds, 0 = disabled
	Status            string     `json:"status"`                     // active, paused, deploying, error
	LastDeployedAt    *time.Time `json:"last_deployed_at,omitempty"`
	LastReconciledAt  *time.Time `json:"last_reconciled_at,omitempty"`
	GitCommit         string     `json:"git_commit,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`

	// Registry credentials (per-stack, since different stacks may use different registries)
	DockerUsername string `json:"-"`
	DockerPassword string `json:"-"`
	DockerRegistry string `json:"docker_registry,omitempty"`
}

// Deployment represents a single deployment attempt for a stack.
type Deployment struct {
	ID           string     `json:"id"`
	StackID      string     `json:"stack_id"`
	StackName    string     `json:"stack_name"`
	Status       string     `json:"status"` // pending, in_progress, completed, failed, rolled_back
	Trigger      string     `json:"trigger"` // webhook, reconcile, manual
	GitCommit    string     `json:"git_commit,omitempty"`
	Changes      string     `json:"changes,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// ManagedContainer tracks containers that Accelero manages.
type ManagedContainer struct {
	ID            string    `json:"id"`
	StackID       string    `json:"stack_id"`
	ServiceName   string    `json:"service_name"`
	ContainerID   string    `json:"container_id"`
	ContainerName string    `json:"container_name"`
	Image         string    `json:"image"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Stack status constants
const (
	StackStatusActive    = "active"
	StackStatusPaused    = "paused"
	StackStatusDeploying = "deploying"
	StackStatusError     = "error"
)

// Deployment status constants
const (
	DeploymentPending    = "pending"
	DeploymentInProgress = "in_progress"
	DeploymentCompleted  = "completed"
	DeploymentFailed     = "failed"
	DeploymentRolledBack = "rolled_back"
)

// Deployment trigger constants
const (
	TriggerWebhook   = "webhook"
	TriggerReconcile = "reconcile"
	TriggerManual    = "manual"
)
