package store

import "time"

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

	// Lifecycle
	Close() error
}
