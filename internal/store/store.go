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

	// Container tracking
	TrackContainer(c *ManagedContainer) error
	ListContainers(stackID string) ([]*ManagedContainer, error)
	RemoveContainer(containerID string) error
	RemoveContainersByStack(stackID string) error

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

	// Lifecycle
	Ping(ctx context.Context) error
	Close() error
}
