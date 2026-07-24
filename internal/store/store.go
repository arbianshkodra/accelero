package store

import (
	"context"
	"time"
)

// Store defines the persistence interface for Accelero.
type Store interface {
	// Stack operations
	CreateStack(stack *Stack) error
	GetStack(id string) (*Stack, error)
	GetStackByName(name string) (*Stack, error)
	ListStacks() ([]*Stack, error)
	UpdateStack(stack *Stack) error
	DeleteStack(id string) error

	// Deployment operations
	CreateDeployment(d *Deployment) error
	GetDeployment(id string) (*Deployment, error)
	ListDeployments(stackID string, limit int) ([]*Deployment, error)
	UpdateDeployment(d *Deployment) error
	CleanupOldDeployments(maxAge time.Duration) (int, error)
	// Approval gates: at-most-one open approval per stack (dedupe) and the
	// cross-stack queue (API listing + timeout sweep).
	GetPendingApproval(stackID string) (*Deployment, error)
	ListPendingApprovals() ([]*Deployment, error)

	// Docker hosts (multi-host). The DOCKER_SOCK daemon is the implicit
	// default host and has no row; stacks with host_id="" target it.
	CreateDockerHost(h *DockerHost) error
	GetDockerHost(id string) (*DockerHost, error)
	GetDockerHostByName(name string) (*DockerHost, error)
	ListDockerHosts() ([]*DockerHost, error)
	DeleteDockerHost(id string) (bool, error)
	CountStacksOnHost(hostID string) (int, error)

	// API keys (RBAC). Keys are stored hashed; GetAPIKeyByHash returns
	// (nil, nil) when no key matches.
	CreateAPIKey(k *APIKey) error
	GetAPIKeyByHash(hash string) (*APIKey, error)
	ListAPIKeys() ([]*APIKey, error)
	DeleteAPIKey(id string) (bool, error)
	TouchAPIKey(id string, t time.Time) error

	// Container tracking
	TrackContainer(c *ManagedContainer) error
	ListContainers(stackID string) ([]*ManagedContainer, error)
	RemoveContainer(containerID string) error
	RemoveContainersByStack(stackID string) error

	// Per-stack secrets. Upsert is the only mutation path — operators
	// only ever "set this key"; the store handles the first-write vs
	// rewrite distinction internally. List returns all rows *including*
	// values: the handler layer decides what the API exposes. Delete on
	// an absent name returns (false, nil) so the handler can map to 404.
	UpsertStackSecret(s *StackSecret) error
	ListStackSecrets(stackID string) ([]*StackSecret, error)
	DeleteStackSecret(stackID, name string) (bool, error)

	// Per-stack Docker registry credentials. Same upsert / list (with
	// passwords) / delete shape as stack secrets. Passwords are
	// encrypted at rest when a cipher is attached.
	UpsertStackRegistry(r *StackRegistry) error
	ListStackRegistries(stackID string) ([]*StackRegistry, error)
	DeleteStackRegistry(stackID, server string) (bool, error)

	// Audit log — append-only; no update or delete API. Retention is
	// time-based and goes through CleanupOldAuditEntries, which is the
	// only path that removes rows. Prune decisions stay with the app
	// (see config.AuditMaxAge), not with individual callers.
	CreateAuditEntry(e *AuditEntry) error
	ListAuditEntries(filter AuditFilter) ([]*AuditEntry, error)
	CleanupOldAuditEntries(maxAge time.Duration) (int, error)

	// ListStacksNeedingEncryption returns IDs of stacks whose
	// repo_token or docker_password is still in pre-encryption
	// plaintext form. Empty slice means nothing to migrate.
	ListStacksNeedingEncryption() ([]string, error)

	// Backup writes a consistent snapshot of the database to destPath
	// (which must not already exist) using SQLite's VACUUM INTO. It is
	// safe to run against a live WAL database and produces a compact,
	// self-contained copy.
	Backup(ctx context.Context, destPath string) error

	// Lifecycle
	Ping(ctx context.Context) error
	Close() error
}
