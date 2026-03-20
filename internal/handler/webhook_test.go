package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
)

// mockStore implements store.Store for testing.
type mockStore struct {
	stacks      []*store.Stack
	deployments []*store.Deployment
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
func (m *mockStore) Close() error                                                 { return nil }

// mockDeployer implements Deployer for testing.
type mockDeployer struct {
	deployCalled bool
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
