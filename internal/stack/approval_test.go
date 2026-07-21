package stack

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGateDeployer builds a Deployer backed by a real SQLite store but no Docker
// client. Only the approval-gate paths (requestApproval / RejectDeployment /
// ExpirePendingApprovals) are exercised — none of them touch Docker, so a nil
// client is fine.
func newGateDeployer(t *testing.T) (*Deployer, store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewSQLiteStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return NewDeployer(nil, s, dir), s
}

func seedStack(t *testing.T, s store.Store, id string, requiresApproval bool) *store.Stack {
	t.Helper()
	now := time.Now()
	st := &store.Stack{
		ID: id, Name: id, RepoURL: "https://example.com/r.git",
		ComposePath: "docker-compose.yml", Status: store.StackStatusActive,
		RequiresApproval: requiresApproval, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.CreateStack(st))
	return st
}

func TestDeploy_RequiresApproval_HoldsInsteadOfDeploying(t *testing.T) {
	d, s := newGateDeployer(t)
	st := seedStack(t, s, "web", true)

	dep, err := d.Deploy(context.Background(), st, store.TriggerManual)
	require.NoError(t, err)
	require.NotNil(t, dep)
	assert.Equal(t, store.DeploymentPendingApproval, dep.Status,
		"a requires_approval stack must hold the deploy, not execute it")

	// The stack must NOT have been flipped to deploying.
	got, err := s.GetStack("web")
	require.NoError(t, err)
	assert.Equal(t, store.StackStatusActive, got.Status)
}

func TestDeploy_RequiresApproval_DedupesPending(t *testing.T) {
	d, s := newGateDeployer(t)
	st := seedStack(t, s, "web", true)

	first, err := d.Deploy(context.Background(), st, store.TriggerManual)
	require.NoError(t, err)
	// A second trigger (e.g. the reconcile loop) must return the SAME pending
	// approval, not create a new one.
	second, err := d.Deploy(context.Background(), st, store.TriggerReconcile)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "repeated triggers should not spawn duplicate approvals")

	all, err := s.ListPendingApprovals()
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

func TestRejectDeployment(t *testing.T) {
	d, s := newGateDeployer(t)
	st := seedStack(t, s, "web", true)
	dep, err := d.Deploy(context.Background(), st, store.TriggerManual)
	require.NoError(t, err)

	rejected, err := d.RejectDeployment(context.Background(), dep.ID, "not now")
	require.NoError(t, err)
	assert.Equal(t, store.DeploymentRejected, rejected.Status)
	assert.Equal(t, "not now", rejected.ErrorMessage)
	require.NotNil(t, rejected.CompletedAt)

	// It's no longer pending, so a fresh trigger creates a NEW approval.
	_, err = d.RejectDeployment(context.Background(), dep.ID, "again")
	require.Error(t, err, "rejecting a non-pending deployment must error")

	all, err := s.ListPendingApprovals()
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestApproveDeployment_RejectsNonPending(t *testing.T) {
	d, s := newGateDeployer(t)
	st := seedStack(t, s, "web", true)
	dep, err := d.Deploy(context.Background(), st, store.TriggerManual)
	require.NoError(t, err)
	_, err = d.RejectDeployment(context.Background(), dep.ID, "")
	require.NoError(t, err)

	// Approving an already-rejected deployment must fail loudly.
	_, err = d.ApproveDeployment(context.Background(), dep.ID)
	assert.Error(t, err)

	// Approving an unknown id must fail.
	_, err = d.ApproveDeployment(context.Background(), "does-not-exist")
	assert.Error(t, err)
}

func TestExpirePendingApprovals(t *testing.T) {
	d, s := newGateDeployer(t)
	seedStack(t, s, "web", true)

	// Seed an old pending approval directly (UpdateDeployment doesn't
	// persist StartedAt, so we can't backdate a Deploy()-created one).
	old := &store.Deployment{
		ID: "d-old", StackID: "web", StackName: "web",
		Status: store.DeploymentPendingApproval, Trigger: store.TriggerManual,
		StartedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, s.CreateDeployment(old))
	// And a fresh one that must survive the sweep.
	fresh := &store.Deployment{
		ID: "d-fresh", StackID: "web", StackName: "web",
		Status: store.DeploymentPendingApproval, Trigger: store.TriggerManual,
		StartedAt: time.Now(),
	}
	require.NoError(t, s.CreateDeployment(fresh))

	// maxAge=0 disables expiry — nothing happens.
	n, err := d.ExpirePendingApprovals(context.Background(), 0)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// With a 1h timeout only the 2h-old approval is rejected.
	n, err = d.ExpirePendingApprovals(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	gotOld, err := s.GetDeployment("d-old")
	require.NoError(t, err)
	assert.Equal(t, store.DeploymentRejected, gotOld.Status)
	assert.Contains(t, gotOld.ErrorMessage, "timed out")

	gotFresh, err := s.GetDeployment("d-fresh")
	require.NoError(t, err)
	assert.Equal(t, store.DeploymentPendingApproval, gotFresh.Status)
}
