package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockStore implements store.Store for testing.
type mockStore struct {
	stacks       []*store.Stack
	deployments  []*store.Deployment
	auditEntries []*store.AuditEntry
}

// newHandler wires a Handler with the three test doubles we need. Kept
// separate from the existing tests to avoid fighting their literals.
func newTestHandler(store *mockStore, docker DockerClient) *Handler {
	return &Handler{
		Store:    store,
		Deployer: &mockDeployer{},
		Docker:   docker,
	}
}

func (m *mockStore) CreateStack(s *store.Stack) error               { m.stacks = append(m.stacks, s); return nil }
func (m *mockStore) GetStack(id string) (*store.Stack, error) {
	for _, s := range m.stacks {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, nil
}
func (m *mockStore) GetStackByName(name string) (*store.Stack, error) {
	for _, s := range m.stacks {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, nil
}
func (m *mockStore) ListStacks() ([]*store.Stack, error)                          { return m.stacks, nil }
func (m *mockStore) UpdateStack(s *store.Stack) error                             { return nil }
func (m *mockStore) DeleteStack(id string) error                                  { return nil }
func (m *mockStore) CreateDeployment(d *store.Deployment) error                   { m.deployments = append(m.deployments, d); return nil }
func (m *mockStore) GetDeployment(id string) (*store.Deployment, error)           { return nil, nil }
func (m *mockStore) ListDeployments(stackID string, limit int) ([]*store.Deployment, error) {
	return m.deployments, nil
}
func (m *mockStore) UpdateDeployment(d *store.Deployment) error                   { return nil }
func (m *mockStore) CleanupOldDeployments(maxAge time.Duration) (int, error)      { return 0, nil }
func (m *mockStore) TrackContainer(c *store.ManagedContainer) error               { return nil }
func (m *mockStore) ListContainers(stackID string) ([]*store.ManagedContainer, error) { return nil, nil }
func (m *mockStore) RemoveContainer(containerID string) error                     { return nil }
func (m *mockStore) RemoveContainersByStack(stackID string) error                 { return nil }
func (m *mockStore) CreateAuditEntry(e *store.AuditEntry) error {
	m.auditEntries = append(m.auditEntries, e)
	return nil
}
func (m *mockStore) ListAuditEntries(_ store.AuditFilter) ([]*store.AuditEntry, error) {
	return m.auditEntries, nil
}
func (m *mockStore) CleanupOldAuditEntries(_ time.Duration) (int, error) { return 0, nil }
func (m *mockStore) Ping(ctx context.Context) error { return nil }
func (m *mockStore) Close() error                   { return nil }

// mockDeployer implements Deployer for testing.
type mockDeployer struct {
	deployCalled      bool
	cleanupCalledWith []string
}

func (d *mockDeployer) Deploy(ctx context.Context, stack *store.Stack, trigger string) (*store.Deployment, error) {
	d.deployCalled = true
	return &store.Deployment{
		ID:        "test-deploy-id",
		StackID:   stack.ID,
		StackName: stack.Name,
		Status:    store.DeploymentCompleted,
		Trigger:   trigger,
		StartedAt: time.Now(),
	}, nil
}

func (d *mockDeployer) CleanupStackData(stackID string) error {
	d.cleanupCalledWith = append(d.cleanupCalledWith, stackID)
	return nil
}

func TestCreateStack(t *testing.T) {
	ms := &mockStore{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	body, _ := json.Marshal(map[string]interface{}{
		"name":         "test-stack",
		"repo_url":     "https://github.com/example/repo",
		"compose_path": "docker-compose.yaml",
	})

	req := httptest.NewRequest("POST", "/api/v1/stacks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)

	var result store.Stack
	err := json.Unmarshal(rr.Body.Bytes(), &result)
	assert.NoError(t, err)
	assert.Equal(t, "test-stack", result.Name)
	assert.NotEmpty(t, result.ID)
}

func TestListStacks(t *testing.T) {
	now := time.Now()
	ms := &mockStore{
		stacks: []*store.Stack{
			{ID: "1", Name: "stack-a", Status: "active", CreatedAt: now, UpdatedAt: now},
			{ID: "2", Name: "stack-b", Status: "active", CreatedAt: now, UpdatedAt: now},
		},
	}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	req := httptest.NewRequest("GET", "/api/v1/stacks", nil)
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var result []*store.Stack
	err := json.Unmarshal(rr.Body.Bytes(), &result)
	assert.NoError(t, err)
	assert.Len(t, result, 2)
}

func TestLegacyWebhook(t *testing.T) {
	now := time.Now()
	ms := &mockStore{
		stacks: []*store.Stack{
			{ID: "default-id", Name: "default", Status: "active", CreatedAt: now, UpdatedAt: now},
		},
	}
	deployer := &mockDeployer{}
	h := &Handler{Store: ms, Deployer: deployer}

	body := []byte(`{"event": "push"}`)
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)

	var result map[string]string
	err := json.Unmarshal(rr.Body.Bytes(), &result)
	assert.NoError(t, err)
	assert.Equal(t, "accepted", result["status"])
	assert.Equal(t, "default-id", result["stack_id"])
}

func TestHealth(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}

	req := httptest.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// ---------------------------------------------------------------------------
// /healthz + /readyz
// ---------------------------------------------------------------------------

func TestHealthz_AliasesHealth(t *testing.T) {
	// /healthz is a K8s-style alias — same body, same status, never
	// touches dependencies (so mock store/pinger state is irrelevant).
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
}

func TestReadyz_AllHealthy(t *testing.T) {
	h := &Handler{
		Store:      &mockStore{},
		Deployer:   &mockDeployer{},
		DockerPing: func(ctx context.Context) error { return nil },
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/readyz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])

	checks, _ := body["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["database"])
	assert.Equal(t, "ok", checks["docker"])
	assert.Equal(t, "ok", checks["shutdown"])
}

func TestReadyz_DockerDown(t *testing.T) {
	h := &Handler{
		Store:      &mockStore{},
		Deployer:   &mockDeployer{},
		DockerPing: func(ctx context.Context) error { return errSimulatedDocker },
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/readyz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "not_ready", body["status"])

	checks, _ := body["checks"].(map[string]any)
	dockerStatus, _ := checks["docker"].(string)
	assert.Contains(t, dockerStatus, "unreachable")
	assert.Equal(t, "ok", checks["database"])
}

func TestReadyz_ShuttingDown(t *testing.T) {
	h := &Handler{
		Store:      &mockStore{},
		Deployer:   &mockDeployer{},
		DockerPing: func(ctx context.Context) error { return nil },
	}
	h.SetShuttingDown()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/readyz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "not_ready", body["status"])

	checks, _ := body["checks"].(map[string]any)
	assert.Equal(t, "draining", checks["shutdown"])
}

func TestReadyz_DockerCheckOptional(t *testing.T) {
	// DockerPing == nil means "skip the docker check" — useful in tests
	// or in hypothetical remote-daemon deployments.
	h := &Handler{
		Store:    &mockStore{},
		Deployer: &mockDeployer{},
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/readyz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	checks, _ := body["checks"].(map[string]any)
	_, present := checks["docker"]
	assert.False(t, present, "docker check should be absent when DockerPing is nil")
}

// errSimulatedDocker stands in for a Docker daemon ping failure in /readyz tests.
var errSimulatedDocker = errReadyzTest("docker daemon not reachable")

type errReadyzTest string

func (e errReadyzTest) Error() string { return string(e) }

func TestCreateStackValidation(t *testing.T) {
	ms := &mockStore{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	// Missing required fields.
	body, _ := json.Marshal(map[string]string{"name": "test"})
	req := httptest.NewRequest("POST", "/api/v1/stacks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ---------------------------------------------------------------------------
// DeleteStack — accepts ID or name, and triggers cleanup
// ---------------------------------------------------------------------------

func TestDeleteStack_ByID(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "abc123", Name: "my-app", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	md := &mockDeployer{}
	h := &Handler{Store: ms, Deployer: md}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/abc123", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"abc123"}, md.cleanupCalledWith,
		"cleanup called with the canonical stack ID")
}

func TestDeleteStack_ByName(t *testing.T) {
	// Same convention as GetStack/UpdateStack: the URL {id} segment
	// can be either the ID or the stack name. A regression in this
	// handler silently left stacks undeletable by name — test guards it.
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "abc123", Name: "my-app", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	md := &mockDeployer{}
	h := &Handler{Store: ms, Deployer: md}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/my-app", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	// Cleanup must receive the *ID*, not the name — downstream code
	// (data dir layout, labels) is keyed on IDs.
	assert.Equal(t, []string{"abc123"}, md.cleanupCalledWith)
}

func TestDeleteStack_NotFound(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/does-not-exist", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// ---------------------------------------------------------------------------
// Preview endpoint
// ---------------------------------------------------------------------------

func TestDriftToAction(t *testing.T) {
	cases := []struct {
		name       string
		drift      reconciler.DriftItem
		wantAction string
	}{
		{
			name:       "missing service -> create",
			drift:      reconciler.DriftItem{Type: "missing", ServiceName: "web", Expected: "nginx:1.27"},
			wantAction: "create",
		},
		{
			name:       "image mismatch -> recreate",
			drift:      reconciler.DriftItem{Type: "image_mismatch", ServiceName: "api", Expected: "api:v2", Actual: "api:v1"},
			wantAction: "recreate",
		},
		{
			name:       "stopped -> restart",
			drift:      reconciler.DriftItem{Type: "stopped", ServiceName: "db"},
			wantAction: "restart",
		},
		{
			name:       "unhealthy -> restart",
			drift:      reconciler.DriftItem{Type: "unhealthy", ServiceName: "worker"},
			wantAction: "restart",
		},
		{
			name:       "extra -> remove",
			drift:      reconciler.DriftItem{Type: "extra", ServiceName: "orphan"},
			wantAction: "remove",
		},
		{
			name:       "missing_external -> error",
			drift:      reconciler.DriftItem{Type: "missing_external", ServiceName: "(volume)"},
			wantAction: "error",
		},
		{
			name:       "unknown drift type -> inspect",
			drift:      reconciler.DriftItem{Type: "something_new", ServiceName: "web"},
			wantAction: "inspect",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driftToAction(tc.drift)
			assert.Equal(t, tc.wantAction, got.Action)
			assert.Equal(t, tc.drift.ServiceName, got.ServiceName)
			assert.Equal(t, tc.drift.Expected, got.Expected)
			assert.Equal(t, tc.drift.Actual, got.Actual)
		})
	}
}

func TestPreviewDeploy_ReconcilerUnavailable(t *testing.T) {
	// Handler returns 503 when no reconciler is wired — exercises the route
	// registration and verifies the happy-path branches haven't been
	// accidentally demoted.
	now := time.Now()
	ms := &mockStore{
		stacks: []*store.Stack{
			{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
		},
	}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}} // no Reconciler

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/stacks/demo/preview", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestPreviewDeploy_StackNotFound(t *testing.T) {
	ms := &mockStore{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/stacks/nope/preview", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// captureRecorder is a tiny audit.Recorder used by handler tests to
// assert that write endpoints emit the right audit entries. We don't
// pull in the real StoreRecorder here because the handler tests already
// use mockStore, which implements the audit store API directly.
type captureRecorder struct {
	entries []store.AuditEntry
}

func (r *captureRecorder) Record(_ context.Context, e store.AuditEntry) error {
	r.entries = append(r.entries, e)
	return nil
}

func TestCreateStack_EmitsAuditEntry(t *testing.T) {
	ms := &mockStore{}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	body, _ := json.Marshal(map[string]interface{}{
		"name":         "audited-stack",
		"repo_url":     "https://example/repo",
		"compose_path": "docker-compose.yaml",
	})
	req := httptest.NewRequest("POST", "/api/v1/stacks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)
	require.Len(t, rec.entries, 1, "one audit entry per create")
	got := rec.entries[0]
	assert.Equal(t, "stack.create", got.Operation)
	assert.Equal(t, "api-key", got.Actor)
	assert.Equal(t, "stack", got.ResourceType)
	assert.Equal(t, "audited-stack", got.StackName)
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
	assert.NotEmpty(t, got.StackID)
}

func TestDeleteStack_EmitsAuditEntry(t *testing.T) {
	// DeleteStack records even when the caller passed a *name*, and the
	// audit row carries the canonical *ID* in ResourceID/StackID so that
	// a later "what happened to stack X" query by ID still finds it.
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "real-id", Name: "named-stack", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/named-stack", nil)
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, "stack.delete", got.Operation)
	assert.Equal(t, "real-id", got.ResourceID, "canonical ID lands in audit, not the name")
	assert.Equal(t, "real-id", got.StackID)
	assert.Equal(t, "named-stack", got.StackName)
}

func TestDeployStack_EmitsInProgressAuditEntry(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "app", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	req := httptest.NewRequest("POST", "/api/v1/stacks/app/deploy", nil)
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)
	require.Len(t, rec.entries, 1, "handler records 'deploy.start'; deployer records completion")
	got := rec.entries[0]
	assert.Equal(t, "deploy.start", got.Operation)
	assert.Equal(t, store.AuditOutcomeInProgress, got.Outcome)
	assert.Equal(t, "manual", got.Metadata["trigger"])
}

func TestListAuditEntries_Endpoint(t *testing.T) {
	// End-to-end check through the real GET /api/v1/audit handler —
	// asserts the store-returned rows come back as JSON.
	now := time.Now().UTC()
	ms := &mockStore{
		auditEntries: []*store.AuditEntry{
			{
				ID:        "entry-1",
				Timestamp: now,
				Actor:     "api-key",
				Operation: store.AuditOpStackCreate,
				StackID:   "s1",
				StackName: "app",
				Outcome:   store.AuditOutcomeSuccess,
			},
		},
	}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	req := httptest.NewRequest("GET", "/api/v1/audit?operation=stack.create", nil)
	rr := httptest.NewRecorder()

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out []*store.AuditEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "stack.create", out[0].Operation)
	assert.Equal(t, "app", out[0].StackName)
}

func TestListAuditEntries_BadSinceRejected(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/audit?since=yesterday", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
