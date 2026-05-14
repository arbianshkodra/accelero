package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
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

	// plaintextStackIDs lets encryption-migration tests simulate
	// "these rows are still plaintext" without modelling the cipher.
	// UpdateStack removes the ID from the set to simulate the row
	// becoming ciphertext.
	plaintextStackIDs map[string]bool
	updateCalls       []string

	// secrets is keyed by (stack_id, name). Separate from the stacks
	// slice so tests can seed both independently.
	secrets map[string]map[string]*store.StackSecret

	// registries mirrors the secrets layout but keyed by (stack_id, server).
	registries map[string]map[string]*store.StackRegistry
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
func (m *mockStore) ListStacks() ([]*store.Stack, error) { return m.stacks, nil }
func (m *mockStore) UpdateStack(s *store.Stack) error {
	m.updateCalls = append(m.updateCalls, s.ID)
	if m.plaintextStackIDs != nil {
		delete(m.plaintextStackIDs, s.ID)
	}
	return nil
}
func (m *mockStore) DeleteStack(id string) error { return nil }
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

// --- Per-stack secrets ---

func (m *mockStore) UpsertStackSecret(s *store.StackSecret) error {
	if m.secrets == nil {
		m.secrets = map[string]map[string]*store.StackSecret{}
	}
	if _, ok := m.secrets[s.StackID]; !ok {
		m.secrets[s.StackID] = map[string]*store.StackSecret{}
	}
	now := time.Now()
	if existing, ok := m.secrets[s.StackID][s.Name]; ok {
		// Preserve CreatedAt on update — matches the real store.
		s.CreatedAt = existing.CreatedAt
	} else {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	m.secrets[s.StackID][s.Name] = &store.StackSecret{
		StackID:   s.StackID,
		Name:      s.Name,
		Value:     s.Value,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
	return nil
}

func (m *mockStore) ListStackSecrets(stackID string) ([]*store.StackSecret, error) {
	bucket := m.secrets[stackID]
	out := make([]*store.StackSecret, 0, len(bucket))
	for _, s := range bucket {
		out = append(out, s)
	}
	// Match the real store's ORDER BY name ASC so handler tests can
	// assert deterministic responses.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *mockStore) DeleteStackSecret(stackID, name string) (bool, error) {
	bucket := m.secrets[stackID]
	if _, ok := bucket[name]; !ok {
		return false, nil
	}
	delete(bucket, name)
	return true, nil
}

// --- Per-stack registry credentials ---

func (m *mockStore) UpsertStackRegistry(r *store.StackRegistry) error {
	if m.registries == nil {
		m.registries = map[string]map[string]*store.StackRegistry{}
	}
	if _, ok := m.registries[r.StackID]; !ok {
		m.registries[r.StackID] = map[string]*store.StackRegistry{}
	}
	now := time.Now()
	if existing, ok := m.registries[r.StackID][r.Server]; ok {
		r.CreatedAt = existing.CreatedAt
	} else {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	m.registries[r.StackID][r.Server] = &store.StackRegistry{
		StackID:   r.StackID,
		Server:    r.Server,
		Username:  r.Username,
		Password:  r.Password,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	return nil
}

func (m *mockStore) ListStackRegistries(stackID string) ([]*store.StackRegistry, error) {
	bucket := m.registries[stackID]
	out := make([]*store.StackRegistry, 0, len(bucket))
	for _, r := range bucket {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out, nil
}

func (m *mockStore) DeleteStackRegistry(stackID, server string) (bool, error) {
	bucket := m.registries[stackID]
	if _, ok := bucket[server]; !ok {
		return false, nil
	}
	delete(bucket, server)
	return true, nil
}

func (m *mockStore) ListStacksNeedingEncryption() ([]string, error) {
	ids := make([]string, 0, len(m.plaintextStackIDs))
	for id := range m.plaintextStackIDs {
		ids = append(ids, id)
	}
	return ids, nil
}
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
			name: "secrets_changed -> recreate",
			// A redeploy must recreate every replica with the new
			// env — Docker can't update env on a running container,
			// so any other action would be misleading in /preview.
			drift:      reconciler.DriftItem{Type: "secrets_changed", ServiceName: "(secrets)"},
			wantAction: "recreate",
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

// ---------------------------------------------------------------------------
// Admin: encrypt-existing (at-rest encryption migration)
// ---------------------------------------------------------------------------

func TestEncryptExistingStacks_DisabledReturns400(t *testing.T) {
	// Endpoint is a no-op when the server has no cipher attached.
	// Returning 200 with "migrated 0" would be a lie — the rows are
	// still plaintext. 400 forces the operator to set the key first.
	h := &Handler{
		Store:             &mockStore{},
		Deployer:          &mockDeployer{},
		EncryptionEnabled: false,
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/admin/encrypt-existing", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "encryption is disabled")
}

func TestEncryptExistingStacks_MigratesPlaintextRows(t *testing.T) {
	now := time.Now()
	ms := &mockStore{
		stacks: []*store.Stack{
			{ID: "s1", Name: "one", Status: "active", CreatedAt: now, UpdatedAt: now},
			{ID: "s2", Name: "two", Status: "active", CreatedAt: now, UpdatedAt: now},
			{ID: "s3", Name: "three", Status: "active", CreatedAt: now, UpdatedAt: now}, // already encrypted
		},
		plaintextStackIDs: map[string]bool{"s1": true, "s2": true},
	}
	rec := &captureRecorder{}
	h := &Handler{
		Store:             ms,
		Deployer:          &mockDeployer{},
		Audit:             rec,
		EncryptionEnabled: true,
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/admin/encrypt-existing", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
	assert.InDelta(t, 2.0, body["stacks_migrated"], 0)
	assert.NotContains(t, body, "stacks_failed")

	// Re-save went through UpdateStack for exactly the plaintext rows.
	// Ordering isn't guaranteed (map iteration), so compare as sets.
	assert.ElementsMatch(t, []string{"s1", "s2"}, ms.updateCalls)

	// Audit row captures the success and the migrated count.
	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOpAdminEncrypt, got.Operation)
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
	assert.Equal(t, "2", got.Metadata["stacks_migrated"])
	assert.Equal(t, "0", got.Metadata["stacks_failed"])
}

func TestEncryptExistingStacks_Idempotent(t *testing.T) {
	// Nothing to migrate → zero work, no failures, audit row with 0/0.
	// Matches what a second call after a successful migration looks like.
	ms := &mockStore{plaintextStackIDs: map[string]bool{}}
	rec := &captureRecorder{}
	h := &Handler{
		Store:             ms,
		Deployer:          &mockDeployer{},
		Audit:             rec,
		EncryptionEnabled: true,
	}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/admin/encrypt-existing", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.InDelta(t, 0.0, body["stacks_migrated"], 0)
	assert.Empty(t, ms.updateCalls)

	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
	assert.Equal(t, "0", got.Metadata["stacks_migrated"])
}

// ---------------------------------------------------------------------------
// Per-stack secrets
// ---------------------------------------------------------------------------

// secretsRouter builds a minimal handler + router pre-seeded with a
// stack so the secret-endpoint tests below can focus on their own
// assertions rather than setup boilerplate.
func secretsRouter(t *testing.T) (*Handler, *mockStore, *captureRecorder, *mux.Router) {
	t.Helper()
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "app", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	return h, ms, rec, router
}

func postSecret(t *testing.T, router http.Handler, stack, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "value": value})
	req := httptest.NewRequest("POST", "/api/v1/stacks/"+stack+"/secrets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSetStackSecret_FirstWriteReturns201(t *testing.T) {
	_, ms, rec, router := secretsRouter(t)

	rr := postSecret(t, router, "app", "DATABASE_URL", "postgres://a")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "DATABASE_URL", body["name"])
	assert.Equal(t, true, body["created"])

	// Store has the row with plaintext value (encryption is a store
	// concern; the mock doesn't simulate it).
	secs, _ := ms.ListStackSecrets("s1")
	require.Len(t, secs, 1)
	assert.Equal(t, "postgres://a", secs[0].Value)

	// Audit row carries the name only, never the value.
	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOpStackSecretSet, got.Operation)
	assert.Equal(t, "DATABASE_URL", got.ResourceID)
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
	assert.Equal(t, "false", got.Metadata["rewrote_existing"])
	for _, v := range got.Metadata {
		assert.NotContains(t, v, "postgres://a", "value must never appear in audit metadata")
	}
}

func TestSetStackSecret_RewriteReturns200(t *testing.T) {
	// Setting the same key twice is the "rotate" flow — upsert rather
	// than 409. Status code flips from 201 (first) to 200 (update)
	// so clients can tell the difference; audit metadata records it
	// too.
	_, _, rec, router := secretsRouter(t)

	require.Equal(t, http.StatusCreated, postSecret(t, router, "app", "K", "v1").Code)

	rr := postSecret(t, router, "app", "K", "v2")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, false, body["created"])

	require.Len(t, rec.entries, 2)
	assert.Equal(t, "true", rec.entries[1].Metadata["rewrote_existing"])
}

func TestSetStackSecret_RejectsInvalidNames(t *testing.T) {
	// Enforce the env-var regex at the API boundary. This matters
	// because secrets are going to be materialised into container env
	// vars at deploy time — accepting "lower" or "with-dash" now would
	// surface as a confusing deploy error later.
	_, _, _, router := secretsRouter(t)

	cases := []string{
		"lowercase",    // not uppercase
		"MIXEDCase",    // not uppercase
		"9LEADING",     // leading digit
		"WITH-DASH",    // dash not allowed
		"SPACE CHAR",   // space not allowed
		"",             // empty
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			rr := postSecret(t, router, "app", name, "v")
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
		})
	}
}

func TestSetStackSecret_RejectsEmptyValue(t *testing.T) {
	// Empty value is ambiguous — route the caller to DELETE explicitly.
	_, _, _, router := secretsRouter(t)
	rr := postSecret(t, router, "app", "K", "")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "DELETE")
}

func TestSetStackSecret_UnknownStack404(t *testing.T) {
	_, _, _, router := secretsRouter(t)
	rr := postSecret(t, router, "does-not-exist", "K", "v")
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListStackSecrets_ValuesRedacted(t *testing.T) {
	// The whole point of this endpoint: values never leave the process
	// via the API. If a future JSON refactor accidentally exposes them,
	// this test fails.
	_, ms, _, router := secretsRouter(t)

	require.NoError(t, ms.UpsertStackSecret(&store.StackSecret{StackID: "s1", Name: "DATABASE_URL", Value: "postgres://SUPER_SECRET"}))
	require.NoError(t, ms.UpsertStackSecret(&store.StackSecret{StackID: "s1", Name: "API_KEY", Value: "KEY_SUPER_SECRET"}))

	req := httptest.NewRequest("GET", "/api/v1/stacks/app/secrets", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.NotContains(t, body, "SUPER_SECRET", "values must never appear in list response")
	assert.NotContains(t, body, "KEY_SUPER_SECRET")

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	// Deterministic order: ASC by name.
	assert.Equal(t, "API_KEY", got[0]["name"])
	assert.Equal(t, "DATABASE_URL", got[1]["name"])
	// No "value" key in the JSON at all.
	_, hasValue := got[0]["value"]
	assert.False(t, hasValue, "no value field in list response")
}

func TestDeleteStackSecret_Success(t *testing.T) {
	_, ms, rec, router := secretsRouter(t)
	require.NoError(t, ms.UpsertStackSecret(&store.StackSecret{StackID: "s1", Name: "K", Value: "v"}))

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/app/secrets/K", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	list, _ := ms.ListStackSecrets("s1")
	assert.Empty(t, list)

	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOpStackSecretDelete, got.Operation)
	assert.Equal(t, "K", got.ResourceID)
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
}

func TestDeleteStackSecret_Missing404AndAuditedAsFailure(t *testing.T) {
	// The audit trail must record "someone tried to delete X" even
	// when X was already gone — that's a useful signal during an
	// incident (e.g. scripted cleanup running twice).
	_, _, rec, router := secretsRouter(t)

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/app/secrets/NOPE", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOutcomeFailure, got.Outcome)
	assert.Equal(t, "not found", got.ErrorMessage)
}

// ---------------------------------------------------------------------------
// Per-stack registry credentials
// ---------------------------------------------------------------------------

func registriesRouter(t *testing.T) (*Handler, *mockStore, *captureRecorder, *mux.Router) {
	t.Helper()
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "app", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	return h, ms, rec, router
}

func postRegistry(t *testing.T, router http.Handler, stack, server, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"server": server, "username": user, "password": pw})
	req := httptest.NewRequest("POST", "/api/v1/stacks/"+stack+"/registries", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSetStackRegistry_FirstWriteReturns201(t *testing.T) {
	_, ms, rec, router := registriesRouter(t)

	rr := postRegistry(t, router, "app", "ghcr.io", "ghuser", "ghpw")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ghcr.io", body["server"])
	assert.Equal(t, "ghuser", body["username"])
	assert.Equal(t, true, body["created"])
	// Body must NOT echo the password.
	_, hasPw := body["password"]
	assert.False(t, hasPw)

	regs, _ := ms.ListStackRegistries("s1")
	require.Len(t, regs, 1)
	assert.Equal(t, "ghpw", regs[0].Password)

	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, store.AuditOpStackRegistrySet, got.Operation)
	assert.Equal(t, "ghcr.io", got.ResourceID)
	assert.Equal(t, "false", got.Metadata["rewrote_existing"])
	assert.Equal(t, "ghuser", got.Metadata["username"])
	for _, v := range got.Metadata {
		assert.NotContains(t, v, "ghpw", "password must never appear in audit metadata")
	}
}

func TestSetStackRegistry_RewriteReturns200(t *testing.T) {
	_, _, rec, router := registriesRouter(t)
	require.Equal(t, http.StatusCreated, postRegistry(t, router, "app", "ghcr.io", "u1", "p1").Code)

	rr := postRegistry(t, router, "app", "ghcr.io", "u2", "p2")
	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, false, body["created"])

	require.Len(t, rec.entries, 2)
	assert.Equal(t, "true", rec.entries[1].Metadata["rewrote_existing"])
}

func TestSetStackRegistry_RejectsBadInputs(t *testing.T) {
	_, _, _, router := registriesRouter(t)

	cases := []struct {
		name             string
		server, u, p     string
		wantStatus       int
		wantContainsBody string
	}{
		{"empty server", "", "u", "p", http.StatusBadRequest, "server is required"},
		{"http URL", "http://ghcr.io", "u", "p", http.StatusBadRequest, "drop the http"},
		{"https URL", "https://ghcr.io", "u", "p", http.StatusBadRequest, "drop the http"},
		{"slash in server", "ghcr.io/path", "u", "p", http.StatusBadRequest, "no slashes"},
		{"empty username", "ghcr.io", "", "p", http.StatusBadRequest, "username is required"},
		{"empty password", "ghcr.io", "u", "", http.StatusBadRequest, "password is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postRegistry(t, router, "app", tc.server, tc.u, tc.p)
			assert.Equal(t, tc.wantStatus, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.wantContainsBody)
		})
	}
}

func TestListStackRegistries_PasswordsRedacted(t *testing.T) {
	_, ms, _, router := registriesRouter(t)

	require.NoError(t, ms.UpsertStackRegistry(&store.StackRegistry{StackID: "s1", Server: "ghcr.io", Username: "ghuser", Password: "REGISTRY_VERY_SECRET"}))
	require.NoError(t, ms.UpsertStackRegistry(&store.StackRegistry{StackID: "s1", Server: "docker.io", Username: "dh", Password: "DH_VERY_SECRET"}))

	req := httptest.NewRequest("GET", "/api/v1/stacks/app/registries", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "REGISTRY_VERY_SECRET")
	assert.NotContains(t, body, "DH_VERY_SECRET")

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	// ASC by server.
	assert.Equal(t, "docker.io", got[0]["server"])
	assert.Equal(t, "ghcr.io", got[1]["server"])
	// Username present, password absent.
	assert.Equal(t, "dh", got[0]["username"])
	_, hasPw := got[0]["password"]
	assert.False(t, hasPw)
}

func TestDeleteStackRegistry_Success(t *testing.T) {
	_, ms, rec, router := registriesRouter(t)
	require.NoError(t, ms.UpsertStackRegistry(&store.StackRegistry{StackID: "s1", Server: "ghcr.io", Username: "u", Password: "p"}))

	req := httptest.NewRequest("DELETE", "/api/v1/stacks/app/registries/ghcr.io", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	list, _ := ms.ListStackRegistries("s1")
	assert.Empty(t, list)

	require.Len(t, rec.entries, 1)
	assert.Equal(t, store.AuditOpStackRegistryDelete, rec.entries[0].Operation)
	assert.Equal(t, store.AuditOutcomeSuccess, rec.entries[0].Outcome)
}

func TestDeleteStackRegistry_MissingAuditedAsFailure(t *testing.T) {
	_, _, rec, router := registriesRouter(t)
	req := httptest.NewRequest("DELETE", "/api/v1/stacks/app/registries/ghcr.io", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	require.Len(t, rec.entries, 1)
	assert.Equal(t, store.AuditOutcomeFailure, rec.entries[0].Outcome)
	assert.Equal(t, "not found", rec.entries[0].ErrorMessage)
}
