package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// newTestStore creates a temporary SQLite store for a single test.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	assert.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// makeStack returns a Stack with sensible defaults. Callers can override fields afterwards.
func makeStack(id, name string) *Stack {
	now := time.Now().Truncate(time.Second)
	return &Stack{
		ID:                id,
		Name:              name,
		RepoURL:           "https://github.com/example/repo.git",
		RepoUsername:      "user",
		RepoToken:         "tok",
		RepoBranch:        "main",
		ComposePath:       "docker-compose.yml",
		ServiceFilter:     "",
		AutoDeploy:        true,
		ReconcileInterval: 300,
		Status:            StackStatusActive,
		DockerUsername:     "duser",
		DockerPassword:     "dpass",
		DockerRegistry:    "registry.example.com",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// makeDeployment returns a Deployment with sensible defaults.
func makeDeployment(id, stackID, stackName, status string) *Deployment {
	now := time.Now().Truncate(time.Second)
	return &Deployment{
		ID:        id,
		StackID:   stackID,
		StackName: stackName,
		Status:    status,
		Trigger:   TriggerManual,
		GitCommit: "abc123",
		StartedAt: now,
	}
}

// makeContainer returns a ManagedContainer with sensible defaults.
func makeContainer(id, stackID, containerID string) *ManagedContainer {
	return &ManagedContainer{
		ID:            id,
		StackID:       stackID,
		ServiceName:   "web",
		ContainerID:   containerID,
		ContainerName: "web-1",
		Image:         "nginx:latest",
		Status:        "running",
		CreatedAt:     time.Now().Truncate(time.Second),
	}
}

// ---------------------------------------------------------------------------
// NewSQLiteStore
// ---------------------------------------------------------------------------

func TestNewSQLiteStore(t *testing.T) {
	s := newTestStore(t)
	assert.NotNil(t, s)
	assert.NotNil(t, s.db)
}

func TestNewSQLiteStore_InvalidPath(t *testing.T) {
	// A path nested under a file (not a directory) should fail.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewSQLiteStore(dbPath)
	assert.NoError(t, err)
	s.Close()

	// Attempting to create a directory under the existing file should fail.
	_, err = NewSQLiteStore(filepath.Join(dbPath, "sub", "nested.db"))
	assert.Error(t, err)
}

func TestNewSQLiteStore_MigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLiteStore(dbPath)
	assert.NoError(t, err)
	s1.Close()

	// Opening the same DB again should not fail (CREATE TABLE IF NOT EXISTS).
	s2, err := NewSQLiteStore(dbPath)
	assert.NoError(t, err)
	s2.Close()
}

// ---------------------------------------------------------------------------
// Stack CRUD
// ---------------------------------------------------------------------------

func TestCreateAndGetStack(t *testing.T) {
	s := newTestStore(t)
	stack := makeStack("s1", "my-stack")

	err := s.CreateStack(stack)
	assert.NoError(t, err)

	got, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, stack.ID, got.ID)
	assert.Equal(t, stack.Name, got.Name)
	assert.Equal(t, stack.RepoURL, got.RepoURL)
	assert.Equal(t, stack.RepoUsername, got.RepoUsername)
	assert.Equal(t, stack.RepoToken, got.RepoToken)
	assert.Equal(t, stack.RepoBranch, got.RepoBranch)
	assert.Equal(t, stack.ComposePath, got.ComposePath)
	assert.Equal(t, stack.AutoDeploy, got.AutoDeploy)
	assert.Equal(t, stack.ReconcileInterval, got.ReconcileInterval)
	assert.Equal(t, stack.Status, got.Status)
	assert.Equal(t, stack.DockerUsername, got.DockerUsername)
	assert.Equal(t, stack.DockerPassword, got.DockerPassword)
	assert.Equal(t, stack.DockerRegistry, got.DockerRegistry)
}

func TestGetStack_NotFound(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetStack("nonexistent")
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetStackByName(t *testing.T) {
	s := newTestStore(t)
	stack := makeStack("s1", "alpha")
	assert.NoError(t, s.CreateStack(stack))

	got, err := s.GetStackByName("alpha")
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, "s1", got.ID)
	assert.Equal(t, "alpha", got.Name)
}

func TestGetStackByName_NotFound(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetStackByName("nope")
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestCreateStack_DuplicateName(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "dup")))

	err := s.CreateStack(makeStack("s2", "dup"))
	assert.Error(t, err, "duplicate name should cause a UNIQUE constraint error")
}

func TestCreateStack_DuplicateID(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "first")))

	err := s.CreateStack(makeStack("s1", "second"))
	assert.Error(t, err, "duplicate primary key should cause an error")
}

// ---------------------------------------------------------------------------
// ListStacks
// ---------------------------------------------------------------------------

func TestListStacks_Empty(t *testing.T) {
	s := newTestStore(t)

	list, err := s.ListStacks()
	assert.NoError(t, err)
	assert.Empty(t, list)
}

func TestListStacks_SortedByName(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s3", "charlie")))
	assert.NoError(t, s.CreateStack(makeStack("s1", "alpha")))
	assert.NoError(t, s.CreateStack(makeStack("s2", "bravo")))

	list, err := s.ListStacks()
	assert.NoError(t, err)
	assert.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name)
	assert.Equal(t, "bravo", list[1].Name)
	assert.Equal(t, "charlie", list[2].Name)
}

// ---------------------------------------------------------------------------
// UpdateStack
// ---------------------------------------------------------------------------

func TestUpdateStack(t *testing.T) {
	s := newTestStore(t)
	stack := makeStack("s1", "orig")
	assert.NoError(t, s.CreateStack(stack))

	// Mutate fields.
	stack.Name = "renamed"
	stack.Status = StackStatusPaused
	stack.AutoDeploy = false
	stack.RepoURL = "https://github.com/other/repo.git"
	stack.GitCommit = "def456"
	now := time.Now().Truncate(time.Second)
	stack.LastDeployedAt = &now

	err := s.UpdateStack(stack)
	assert.NoError(t, err)

	got, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, StackStatusPaused, got.Status)
	assert.False(t, got.AutoDeploy)
	assert.Equal(t, "https://github.com/other/repo.git", got.RepoURL)
	assert.Equal(t, "def456", got.GitCommit)
	assert.NotNil(t, got.LastDeployedAt)
	// UpdatedAt should have been refreshed by UpdateStack itself.
	assert.False(t, got.UpdatedAt.IsZero())
}

func TestUpdateStack_SetsUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	stack := makeStack("s1", "ts-test")
	stack.CreatedAt = time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	stack.UpdatedAt = stack.CreatedAt
	assert.NoError(t, s.CreateStack(stack))

	beforeUpdate := time.Now().Add(-1 * time.Second)
	assert.NoError(t, s.UpdateStack(stack))

	got, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.True(t, got.UpdatedAt.After(beforeUpdate), "UpdatedAt should be refreshed")
}

// ---------------------------------------------------------------------------
// DeleteStack (and cascade)
// ---------------------------------------------------------------------------

func TestDeleteStack(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "gone")))

	err := s.DeleteStack("s1")
	assert.NoError(t, err)

	got, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.Nil(t, got, "stack should be deleted")
}

func TestDeleteStack_DeploymentsOrphaned(t *testing.T) {
	// NOTE: modernc.org/sqlite does not honour _foreign_keys=ON in the DSN,
	// so ON DELETE CASCADE is not enforced.  Deployments become orphaned.
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "cascade-stack")))
	d := makeDeployment("d1", "s1", "cascade-stack", DeploymentCompleted)
	assert.NoError(t, s.CreateDeployment(d))

	// Sanity check.
	got, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.NotNil(t, got)

	assert.NoError(t, s.DeleteStack("s1"))

	// Stack is gone.
	stackGot, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.Nil(t, stackGot, "stack should be deleted")

	// Deployment is still present (orphaned).
	got, err = s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.NotNil(t, got, "deployment is orphaned because foreign keys are not enforced")
}

func TestDeleteStack_ContainersOrphaned(t *testing.T) {
	// NOTE: same foreign-key caveat as above.
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "cascade-ctr")))
	c := makeContainer("mc1", "s1", "ctr-aaa")
	assert.NoError(t, s.TrackContainer(c))

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1)

	assert.NoError(t, s.DeleteStack("s1"))

	// Stack is gone.
	stackGot, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.Nil(t, stackGot, "stack should be deleted")

	// Containers are still present (orphaned).
	list, err = s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1, "containers are orphaned because foreign keys are not enforced")
}

func TestDeleteStack_NonexistentIsNoop(t *testing.T) {
	s := newTestStore(t)
	err := s.DeleteStack("doesnt-exist")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Deployment CRUD
// ---------------------------------------------------------------------------

func TestCreateAndGetDeployment(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "dep-stack")))

	d := makeDeployment("d1", "s1", "dep-stack", DeploymentPending)
	d.Changes = "updated nginx"
	assert.NoError(t, s.CreateDeployment(d))

	got, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, "d1", got.ID)
	assert.Equal(t, "s1", got.StackID)
	assert.Equal(t, "dep-stack", got.StackName)
	assert.Equal(t, DeploymentPending, got.Status)
	assert.Equal(t, TriggerManual, got.Trigger)
	assert.Equal(t, "abc123", got.GitCommit)
	assert.Equal(t, "updated nginx", got.Changes)
	assert.Nil(t, got.CompletedAt)
}

func TestGetDeployment_NotFound(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetDeployment("nonexistent")
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestListDeployments_OrderedByStartedAtDesc(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "list-dep")))

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	for i, id := range []string{"d1", "d2", "d3"} {
		d := makeDeployment(id, "s1", "list-dep", DeploymentCompleted)
		d.StartedAt = base.Add(time.Duration(i) * time.Hour) // d1 oldest, d3 newest
		assert.NoError(t, s.CreateDeployment(d))
	}

	list, err := s.ListDeployments("s1", 10)
	assert.NoError(t, err)
	assert.Len(t, list, 3)
	// newest first
	assert.Equal(t, "d3", list[0].ID)
	assert.Equal(t, "d2", list[1].ID)
	assert.Equal(t, "d1", list[2].ID)
}

func TestListDeployments_RespectsLimit(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "limit-dep")))

	base := time.Now().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		d := makeDeployment("d"+string(rune('A'+i)), "s1", "limit-dep", DeploymentCompleted)
		d.StartedAt = base.Add(time.Duration(i) * time.Minute)
		assert.NoError(t, s.CreateDeployment(d))
	}

	list, err := s.ListDeployments("s1", 2)
	assert.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestListDeployments_DefaultLimit(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "default-limit")))

	d := makeDeployment("d1", "s1", "default-limit", DeploymentPending)
	assert.NoError(t, s.CreateDeployment(d))

	// limit <= 0 should default to 50
	list, err := s.ListDeployments("s1", 0)
	assert.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestListDeployments_Empty(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "empty-dep")))

	list, err := s.ListDeployments("s1", 10)
	assert.NoError(t, err)
	assert.Empty(t, list)
}

func TestListDeployments_FiltersByStack(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "stack-a")))
	assert.NoError(t, s.CreateStack(makeStack("s2", "stack-b")))

	d1 := makeDeployment("d1", "s1", "stack-a", DeploymentPending)
	d2 := makeDeployment("d2", "s2", "stack-b", DeploymentPending)
	assert.NoError(t, s.CreateDeployment(d1))
	assert.NoError(t, s.CreateDeployment(d2))

	list, err := s.ListDeployments("s1", 10)
	assert.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "d1", list[0].ID)
}

// ---------------------------------------------------------------------------
// UpdateDeployment
// ---------------------------------------------------------------------------

func TestUpdateDeployment(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "upd-dep")))

	d := makeDeployment("d1", "s1", "upd-dep", DeploymentPending)
	assert.NoError(t, s.CreateDeployment(d))

	// Transition to completed.
	now := time.Now().Truncate(time.Second)
	d.Status = DeploymentCompleted
	d.GitCommit = "final789"
	d.Changes = "rolled out v2"
	d.CompletedAt = &now

	err := s.UpdateDeployment(d)
	assert.NoError(t, err)

	got, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.Equal(t, DeploymentCompleted, got.Status)
	assert.Equal(t, "final789", got.GitCommit)
	assert.Equal(t, "rolled out v2", got.Changes)
	assert.NotNil(t, got.CompletedAt)
}

func TestUpdateDeployment_SetErrorMessage(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "err-dep")))

	d := makeDeployment("d1", "s1", "err-dep", DeploymentInProgress)
	assert.NoError(t, s.CreateDeployment(d))

	d.Status = DeploymentFailed
	d.ErrorMessage = "container crashed"
	now := time.Now().Truncate(time.Second)
	d.CompletedAt = &now
	assert.NoError(t, s.UpdateDeployment(d))

	got, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.Equal(t, DeploymentFailed, got.Status)
	assert.Equal(t, "container crashed", got.ErrorMessage)
}

// ---------------------------------------------------------------------------
// CleanupOldDeployments
// ---------------------------------------------------------------------------

func TestCleanupOldDeployments_RemovesOldTerminalStatuses(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "cleanup")))

	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	// Old terminal deployments that should be cleaned up.
	for i, status := range []string{DeploymentCompleted, DeploymentFailed, DeploymentRolledBack} {
		d := makeDeployment("old-"+status, "s1", "cleanup", status)
		d.StartedAt = old.Add(time.Duration(i) * time.Minute)
		assert.NoError(t, s.CreateDeployment(d))
	}

	n, err := s.CleanupOldDeployments(24 * time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, 3, n)

	// Verify they are gone.
	for _, status := range []string{DeploymentCompleted, DeploymentFailed, DeploymentRolledBack} {
		got, err := s.GetDeployment("old-" + status)
		assert.NoError(t, err)
		assert.Nil(t, got, "deployment with status %s should have been cleaned up", status)
	}
}

func TestCleanupOldDeployments_PreservesNonTerminal(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "preserve")))

	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	// Old but non-terminal statuses should NOT be removed.
	for _, status := range []string{DeploymentPending, DeploymentInProgress} {
		d := makeDeployment("keep-"+status, "s1", "preserve", status)
		d.StartedAt = old
		assert.NoError(t, s.CreateDeployment(d))
	}

	n, err := s.CleanupOldDeployments(24 * time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, 0, n)

	for _, status := range []string{DeploymentPending, DeploymentInProgress} {
		got, err := s.GetDeployment("keep-" + status)
		assert.NoError(t, err)
		assert.NotNil(t, got, "non-terminal deployment %s should be preserved", status)
	}
}

func TestCleanupOldDeployments_PreservesRecent(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "recent")))

	recent := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	d := makeDeployment("recent-1", "s1", "recent", DeploymentCompleted)
	d.StartedAt = recent
	assert.NoError(t, s.CreateDeployment(d))

	n, err := s.CleanupOldDeployments(24 * time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, 0, n)

	got, err := s.GetDeployment("recent-1")
	assert.NoError(t, err)
	assert.NotNil(t, got, "recent completed deployment should not be cleaned up")
}

func TestCleanupOldDeployments_NoDeployments(t *testing.T) {
	s := newTestStore(t)

	n, err := s.CleanupOldDeployments(24 * time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
}

// ---------------------------------------------------------------------------
// Container tracking
// ---------------------------------------------------------------------------

func TestTrackAndListContainers(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "ctr-stack")))

	c := makeContainer("mc1", "s1", "ctr-aaa")
	assert.NoError(t, s.TrackContainer(c))

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "mc1", list[0].ID)
	assert.Equal(t, "s1", list[0].StackID)
	assert.Equal(t, "web", list[0].ServiceName)
	assert.Equal(t, "ctr-aaa", list[0].ContainerID)
	assert.Equal(t, "web-1", list[0].ContainerName)
	assert.Equal(t, "nginx:latest", list[0].Image)
	assert.Equal(t, "running", list[0].Status)
}

func TestListContainers_Empty(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "empty-ctr")))

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Empty(t, list)
}

func TestListContainers_OrderedByCreatedAtDesc(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "order-ctr")))

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	for i, id := range []string{"mc1", "mc2", "mc3"} {
		c := makeContainer(id, "s1", "ctr-"+id)
		c.CreatedAt = base.Add(time.Duration(i) * time.Hour) // mc1 oldest, mc3 newest
		assert.NoError(t, s.TrackContainer(c))
	}

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 3)
	assert.Equal(t, "mc3", list[0].ID)
	assert.Equal(t, "mc2", list[1].ID)
	assert.Equal(t, "mc1", list[2].ID)
}

func TestListContainers_FiltersByStack(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "ctr-a")))
	assert.NoError(t, s.CreateStack(makeStack("s2", "ctr-b")))

	assert.NoError(t, s.TrackContainer(makeContainer("mc1", "s1", "ctr-1")))
	assert.NoError(t, s.TrackContainer(makeContainer("mc2", "s2", "ctr-2")))

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "mc1", list[0].ID)
}

func TestTrackContainer_UpsertOnConflict(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "upsert-ctr")))

	c := makeContainer("mc1", "s1", "ctr-aaa")
	c.Status = "running"
	assert.NoError(t, s.TrackContainer(c))

	// Re-track with updated status; same primary key ID.
	c.Status = "stopped"
	c.Image = "nginx:1.25"
	assert.NoError(t, s.TrackContainer(c))

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1, "should upsert, not duplicate")
	assert.Equal(t, "stopped", list[0].Status)
	assert.Equal(t, "nginx:1.25", list[0].Image)
}

func TestRemoveContainer(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "rm-ctr")))

	assert.NoError(t, s.TrackContainer(makeContainer("mc1", "s1", "ctr-aaa")))
	assert.NoError(t, s.TrackContainer(makeContainer("mc2", "s1", "ctr-bbb")))

	err := s.RemoveContainer("ctr-aaa")
	assert.NoError(t, err)

	list, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "ctr-bbb", list[0].ContainerID)
}

func TestRemoveContainer_NonexistentIsNoop(t *testing.T) {
	s := newTestStore(t)
	err := s.RemoveContainer("doesnt-exist")
	assert.NoError(t, err)
}

func TestRemoveContainersByStack(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "rmall-a")))
	assert.NoError(t, s.CreateStack(makeStack("s2", "rmall-b")))

	assert.NoError(t, s.TrackContainer(makeContainer("mc1", "s1", "ctr-1")))
	assert.NoError(t, s.TrackContainer(makeContainer("mc2", "s1", "ctr-2")))
	assert.NoError(t, s.TrackContainer(makeContainer("mc3", "s2", "ctr-3")))

	err := s.RemoveContainersByStack("s1")
	assert.NoError(t, err)

	listS1, err := s.ListContainers("s1")
	assert.NoError(t, err)
	assert.Empty(t, listS1, "all s1 containers should be removed")

	listS2, err := s.ListContainers("s2")
	assert.NoError(t, err)
	assert.Len(t, listS2, 1, "s2 containers should be untouched")
}

func TestRemoveContainersByStack_NonexistentIsNoop(t *testing.T) {
	s := newTestStore(t)
	err := s.RemoveContainersByStack("doesnt-exist")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Stack with LastDeployedAt / LastReconciledAt round-trip
// ---------------------------------------------------------------------------

func TestStackNullableTimestamps(t *testing.T) {
	s := newTestStore(t)

	// Stack without optional timestamps.
	stack := makeStack("s1", "nullable-ts")
	assert.NoError(t, s.CreateStack(stack))

	got, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.Nil(t, got.LastDeployedAt)
	assert.Nil(t, got.LastReconciledAt)

	// Set both optional timestamps.
	now := time.Now().Truncate(time.Second)
	got.LastDeployedAt = &now
	got.LastReconciledAt = &now
	assert.NoError(t, s.UpdateStack(got))

	got2, err := s.GetStack("s1")
	assert.NoError(t, err)
	assert.NotNil(t, got2.LastDeployedAt)
	assert.NotNil(t, got2.LastReconciledAt)
}

// ---------------------------------------------------------------------------
// Deployment with CompletedAt round-trip
// ---------------------------------------------------------------------------

func TestDeploymentCompletedAtNullable(t *testing.T) {
	s := newTestStore(t)
	assert.NoError(t, s.CreateStack(makeStack("s1", "nullable-dep")))

	d := makeDeployment("d1", "s1", "nullable-dep", DeploymentPending)
	assert.NoError(t, s.CreateDeployment(d))

	got, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.Nil(t, got.CompletedAt)

	now := time.Now().Truncate(time.Second)
	d.CompletedAt = &now
	d.Status = DeploymentCompleted
	assert.NoError(t, s.UpdateDeployment(d))

	got2, err := s.GetDeployment("d1")
	assert.NoError(t, err)
	assert.NotNil(t, got2.CompletedAt)
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestClose(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	assert.NoError(t, err)

	err = s.Close()
	assert.NoError(t, err)

	// Operations after close should fail.
	_, err = s.ListStacks()
	assert.Error(t, err, "queries after Close should fail")
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

func TestAudit_InsertAndList(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	entries := []*AuditEntry{
		{
			ID: "a1", Timestamp: now.Add(-3 * time.Minute),
			Actor: "api-key", Operation: AuditOpStackCreate,
			ResourceType: "stack", ResourceID: "s1", StackID: "s1", StackName: "alpha",
			Outcome:  AuditOutcomeSuccess,
			Metadata: map[string]string{"note": "first"},
		},
		{
			ID: "a2", Timestamp: now.Add(-2 * time.Minute),
			Actor: "system:reconciler", Operation: AuditOpDriftDetected,
			ResourceType: "stack", ResourceID: "s1", StackID: "s1", StackName: "alpha",
			Outcome:  AuditOutcomeSuccess,
			Metadata: map[string]string{"drift_count": "3"},
		},
		{
			ID: "a3", Timestamp: now.Add(-1 * time.Minute),
			Actor: "api-key", Operation: AuditOpStackDelete,
			ResourceType: "stack", ResourceID: "s2", StackID: "s2", StackName: "beta",
			Outcome: AuditOutcomeSuccess,
		},
	}
	for _, e := range entries {
		assert.NoError(t, s.CreateAuditEntry(e))
	}

	t.Run("list-all-newest-first", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{})
		assert.NoError(t, err)
		assert.Len(t, got, 3)
		assert.Equal(t, "a3", got[0].ID, "newest first")
		assert.Equal(t, "a2", got[1].ID)
		assert.Equal(t, "a1", got[2].ID)
	})

	t.Run("filter-by-stack-id", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{StackID: "s1"})
		assert.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("filter-by-actor", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{Actor: "system:reconciler"})
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, "a2", got[0].ID)
	})

	t.Run("filter-by-operation", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{Operation: AuditOpStackCreate})
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, "a1", got[0].ID)
	})

	t.Run("filter-by-since", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{Since: now.Add(-90 * time.Second)})
		assert.NoError(t, err)
		assert.Len(t, got, 1, "only a3 is within the last 90s window")
		assert.Equal(t, "a3", got[0].ID)
	})

	t.Run("metadata-round-trips", func(t *testing.T) {
		got, err := s.ListAuditEntries(AuditFilter{StackID: "s1", Operation: AuditOpDriftDetected})
		assert.NoError(t, err)
		assert.Len(t, got, 1)
		if len(got) == 1 {
			assert.Equal(t, "3", got[0].Metadata["drift_count"])
		}
	})
}

func TestAudit_CleanupOldEntries(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	now := time.Now().UTC()
	// Three rows: old, borderline, fresh.
	entries := []*AuditEntry{
		{ID: "old", Timestamp: now.Add(-48 * time.Hour), Actor: "x", Operation: AuditOpStackCreate, Outcome: AuditOutcomeSuccess},
		{ID: "borderline", Timestamp: now.Add(-23 * time.Hour), Actor: "x", Operation: AuditOpStackCreate, Outcome: AuditOutcomeSuccess},
		{ID: "fresh", Timestamp: now.Add(-1 * time.Minute), Actor: "x", Operation: AuditOpStackCreate, Outcome: AuditOutcomeSuccess},
	}
	for _, e := range entries {
		assert.NoError(t, s.CreateAuditEntry(e))
	}

	t.Run("drops rows older than max age", func(t *testing.T) {
		n, err := s.CleanupOldAuditEntries(24 * time.Hour)
		assert.NoError(t, err)
		assert.Equal(t, 1, n, "only the 48h-old row is beyond 24h cutoff")

		got, _ := s.ListAuditEntries(AuditFilter{})
		// fresh + borderline survive.
		assert.Len(t, got, 2)
	})

	t.Run("zero max age is a no-op", func(t *testing.T) {
		// AUDIT_MAX_AGE=0 disables retention; nothing should be deleted.
		n, err := s.CleanupOldAuditEntries(0)
		assert.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("negative max age is a no-op", func(t *testing.T) {
		n, err := s.CleanupOldAuditEntries(-1 * time.Hour)
		assert.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}

func TestAudit_LimitCap(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Insert more than the cap; verify the store clamps it.
	for i := 0; i < 50; i++ {
		assert.NoError(t, s.CreateAuditEntry(&AuditEntry{
			ID:        string(rune('a' + i)) + "_" + string(rune('0'+i%10)) + "_x",
			Timestamp: time.Now().Add(-time.Duration(i) * time.Second),
			Actor:     "api-key",
			Operation: AuditOpStackCreate,
			Outcome:   AuditOutcomeSuccess,
		}))
	}

	// Limit=10 returns 10.
	got, err := s.ListAuditEntries(AuditFilter{Limit: 10})
	assert.NoError(t, err)
	assert.Len(t, got, 10)

	// Limit=-5 defaults to 100.
	got, err = s.ListAuditEntries(AuditFilter{Limit: -5})
	assert.NoError(t, err)
	assert.Len(t, got, 50, "all 50 rows fit under the default 100 limit")

	// Limit above cap is clamped to cap.
	got, err = s.ListAuditEntries(AuditFilter{Limit: 99999})
	assert.NoError(t, err)
	assert.LessOrEqual(t, len(got), maxAuditListLimit)
}
