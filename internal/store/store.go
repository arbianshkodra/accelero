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

	// Audit log — append-only; no update or delete path. Immutability
	// is the point, so the interface exposes exactly two methods.
	CreateAuditEntry(e *AuditEntry) error
	ListAuditEntries(filter AuditFilter) ([]*AuditEntry, error)

	// Lifecycle
	Ping(ctx context.Context) error
	Close() error
}
