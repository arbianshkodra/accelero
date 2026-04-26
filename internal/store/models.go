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

	// SecretsHash is the SHA-256 digest of the per-stack secrets that
	// were active at the most recent successful deploy. The reconciler
	// compares it against the hash of the *current* secret set to
	// detect "secrets rotated since last deploy" drift. Updated by the
	// deployer on success only — failed deploys leave it untouched so
	// the next reconcile keeps reporting drift until a deploy actually
	// applies the new values.
	SecretsHash string `json:"-"`
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

// StackSecret is a per-stack key/value pair stored encrypted at rest and
// injected into managed containers at deploy time (follow-up PR for the
// injection half). Value is plaintext in the Go type; the SQLite column
// holds ciphertext whenever at-rest encryption is enabled. The API only
// ever returns the Name — Value leaves the process only through the
// deploy path.
type StackSecret struct {
	StackID   string    `json:"stack_id"`
	Name      string    `json:"name"`
	Value     string    `json:"-"` // never serialised back to clients
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// AuditEntry is an append-only record of a notable API action or system
// event. Stored in a separate table; no update/delete path. Indexed on
// (timestamp desc) for the listing endpoint and on (stack_id, timestamp)
// for per-stack filters.
type AuditEntry struct {
	ID           string            `json:"id"`
	Timestamp    time.Time         `json:"timestamp"`
	Actor        string            `json:"actor"`                   // "api-key", "system:reconciler", ...
	RemoteAddr   string            `json:"remote_addr,omitempty"`   // best-effort, from RemoteAddr
	RequestID    string            `json:"request_id,omitempty"`    // correlates with logs
	Operation    string            `json:"operation"`               // "stack.create", "deploy.start", ...
	ResourceType string            `json:"resource_type,omitempty"` // "stack", "deployment"
	ResourceID   string            `json:"resource_id,omitempty"`
	StackID      string            `json:"stack_id,omitempty"`  // denormalised for filtering
	StackName    string            `json:"stack_name,omitempty"`
	Outcome      string            `json:"outcome"`                // "success", "failure", "in_progress"
	ErrorMessage string            `json:"error_message,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`     // operation-specific details
}

// AuditFilter narrows an AuditEntry list query. Empty fields are
// ignored. Limit is required and capped at the store level.
type AuditFilter struct {
	StackID   string    // exact match on stack_id
	StackName string    // exact match on stack_name
	Actor     string    // exact match on actor
	Operation string    // exact match on operation
	Since     time.Time // inclusive; zero = no lower bound
	Limit     int       // max rows; 0 → store default; store caps at maxAuditListLimit
}

// Audit outcome constants.
const (
	AuditOutcomeSuccess    = "success"
	AuditOutcomeFailure    = "failure"
	AuditOutcomeInProgress = "in_progress"
)

// Audit operation constants for the actions wired in today.
// Add more as new write paths grow.
const (
	AuditOpStackCreate       = "stack.create"
	AuditOpStackUpdate       = "stack.update"
	AuditOpStackDelete       = "stack.delete"
	AuditOpDeployStart       = "deploy.start"
	AuditOpDeployComplete    = "deploy.complete"
	AuditOpDeployFailed      = "deploy.failed"
	AuditOpDeployRolledBack  = "deploy.rolled_back"
	AuditOpDriftDetected     = "drift.detected"
	AuditOpDriftAutoDeployed = "drift.auto_deployed"
	// Operator actions that bypass the GitOps flow. Always audited so
	// the trail captures "someone did something imperative here" even
	// though the action itself may not change desired state (restart)
	// or may deliberately override it (exec, volume write — not shipped
	// yet).
	AuditOpContainerRestart   = "container.restart"
	AuditOpContainerExecStart = "container.exec_start"
	AuditOpContainerExecEnd   = "container.exec_end"
	AuditOpVolumeBrowse       = "volume.browse"
	AuditOpVolumeRead         = "volume.read"
	AuditOpVolumeWrite        = "volume.write"
	AuditOpAdminEncrypt       = "admin.encrypt-existing"

	// Per-stack secrets CRUD. Values never appear in audit metadata —
	// only the name and outcome do. See handler.StackSecretsSet /
	// handler.StackSecretsDelete.
	AuditOpStackSecretSet    = "stack.secret.set"
	AuditOpStackSecretDelete = "stack.secret.delete"
)
