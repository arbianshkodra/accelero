package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func apikeyRouter(h *Handler) *mux.Router {
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	return router
}

func TestCreateAPIKey_ReturnsRawKeyOnce(t *testing.T) {
	ms := &mockStore{}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	body, _ := json.Marshal(map[string]string{"name": "ci", "role": "operator"})
	req := httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.Equal(t, "ci", out["name"])
	assert.Equal(t, "operator", out["role"])
	key, _ := out["key"].(string)
	assert.Contains(t, key, "acc_", "raw key returned once")
	// It was persisted (hashed).
	require.Len(t, ms.apiKeys, 1)
	assert.Equal(t, store.HashAPIKey(key), ms.apiKeys[0].KeyHash)
	assert.True(t, hasAuditOp(rec.entries, store.AuditOpAPIKeyCreate))
}

func TestCreateAPIKey_Validation(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}
	// Missing name.
	body, _ := json.Marshal(map[string]string{"role": "operator"})
	req := httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Bad role.
	body, _ = json.Marshal(map[string]string{"name": "x", "role": "root"})
	req = httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	rr = httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestListAPIKeys_RedactsHash(t *testing.T) {
	ms := &mockStore{apiKeys: []*store.APIKey{
		{ID: "k1", Name: "ci", Role: "viewer", KeyHash: "secret-hash"},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}
	req := httptest.NewRequest("GET", "/api/v1/apikeys", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "secret-hash", "hash must never be serialised")
	assert.Contains(t, rr.Body.String(), "ci")
}

func TestDeleteAPIKey(t *testing.T) {
	ms := &mockStore{apiKeys: []*store.APIKey{{ID: "k1", Name: "ci", Role: "viewer"}}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	// Existing → 204.
	req := httptest.NewRequest("DELETE", "/api/v1/apikeys/k1", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Missing → 404, audited as failure.
	req = httptest.NewRequest("DELETE", "/api/v1/apikeys/k1", nil)
	rr = httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.True(t, hasAuditOp(rec.entries, store.AuditOpAPIKeyDelete))
}

func TestCreateAPIKey_WithStackGrants(t *testing.T) {
	ms := &mockStore{stacks: []*store.Stack{{ID: "sid-prod", Name: "prod"}}}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	// Grant keyed by stack NAME resolves to the stack ID.
	body, _ := json.Marshal(map[string]any{
		"name": "team", "role": "none",
		"stack_grants": map[string]string{"prod": "operator"},
	})
	req := httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	require.Len(t, ms.apiKeys, 1)
	assert.Equal(t, map[string]string{"sid-prod": "operator"}, ms.apiKeys[0].StackGrants,
		"grant stored keyed by canonical stack ID")
}

func TestCreateAPIKey_RejectsUnknownStackGrant(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}
	body, _ := json.Marshal(map[string]any{
		"name": "team", "role": "viewer",
		"stack_grants": map[string]string{"ghost": "operator"},
	})
	req := httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "unknown stack")
}

func TestCreateAPIKey_RejectsBadGrantRole(t *testing.T) {
	ms := &mockStore{stacks: []*store.Stack{{ID: "sid-prod", Name: "prod"}}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}
	body, _ := json.Marshal(map[string]any{
		"name": "team", "role": "viewer",
		"stack_grants": map[string]string{"prod": "superuser"},
	})
	req := httptest.NewRequest("POST", "/api/v1/apikeys", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
