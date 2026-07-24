package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postHost(h *Handler, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/hosts", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	return rr
}

func TestCreateDockerHost_Validation(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}

	// Missing name/endpoint.
	rr := postHost(h, map[string]any{"name": "edge"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	rr = postHost(h, map[string]any{"endpoint": "tcp://x:2376"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Unsupported scheme (ssh:// is a documented follow-up, not supported yet).
	rr = postHost(h, map[string]any{"name": "edge", "endpoint": "ssh://host"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "unix:// or tcp://")

	// Partial TLS triple is rejected before any connection attempt.
	rr = postHost(h, map[string]any{
		"name": "edge", "endpoint": "tcp://x:2376", "tls_cert": "cert-only",
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "together")
}

func TestCreateDockerHost_DuplicateName(t *testing.T) {
	ms := &mockStore{dockerHosts: []*store.DockerHost{{ID: "h1", Name: "edge", Endpoint: "tcp://a:2376"}}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}
	rr := postHost(h, map[string]any{"name": "edge", "endpoint": "tcp://b:2376"})
	assert.Equal(t, http.StatusConflict, rr.Code)
}

func TestCreateDockerHost_UnreachableIsRejectedAndAudited(t *testing.T) {
	rec := &captureRecorder{}
	ms := &mockStore{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	// Port 1 on localhost: nothing listens, so the pre-store ping fails.
	rr := postHost(h, map[string]any{"name": "dead", "endpoint": "tcp://127.0.0.1:1"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "not reachable")
	assert.Empty(t, ms.dockerHosts, "an unreachable host must not be persisted")
	assert.True(t, hasAuditOp(rec.entries, store.AuditOpHostCreate), "failed registration is audited")
}

func TestListDockerHosts_RedactsTLSMaterial(t *testing.T) {
	ms := &mockStore{dockerHosts: []*store.DockerHost{{
		ID: "h1", Name: "edge", Endpoint: "tcp://a:2376",
		TLSCA: "ca-pem", TLSCert: "cert-pem", TLSKey: "super-secret-key",
	}}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	req := httptest.NewRequest("GET", "/api/v1/hosts", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "super-secret-key", "TLS key must never be serialised")
	assert.NotContains(t, body, "cert-pem")
	assert.NotContains(t, body, "ca-pem")
	assert.Contains(t, body, `"tls_enabled":true`, "clients learn TLS is on without seeing material")
	assert.Contains(t, body, "edge")
}

func TestDeleteDockerHost(t *testing.T) {
	rec := &captureRecorder{}
	ms := &mockStore{dockerHosts: []*store.DockerHost{{ID: "h1", Name: "edge"}}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Audit: rec}

	req := httptest.NewRequest("DELETE", "/api/v1/hosts/h1", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Second delete → 404, audited as failure.
	req = httptest.NewRequest("DELETE", "/api/v1/hosts/h1", nil)
	rr = httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.True(t, hasAuditOp(rec.entries, store.AuditOpHostDelete))
}

func TestDeleteDockerHost_RefusesWhileInUse(t *testing.T) {
	ms := &mockStore{
		dockerHosts: []*store.DockerHost{{ID: "h1", Name: "edge"}},
		stacks:      []*store.Stack{{ID: "s1", Name: "web", HostID: "h1"}},
	}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	req := httptest.NewRequest("DELETE", "/api/v1/hosts/h1", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "still targeted by 1 stack")
	assert.Len(t, ms.dockerHosts, 1, "host survives a refused delete")
}

func TestCreateStack_HostIDResolvedByName(t *testing.T) {
	ms := &mockStore{dockerHosts: []*store.DockerHost{{ID: "h1", Name: "edge"}}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}

	body, _ := json.Marshal(map[string]any{
		"name": "web", "repo_url": "https://x/y.git", "compose_path": "dc.yaml",
		"host_id": "edge", // by name
	})
	req := httptest.NewRequest("POST", "/api/v1/stacks", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	var got store.Stack
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "h1", got.HostID, "host name resolves to canonical id")
}

func TestCreateStack_UnknownHostRejected(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}
	body, _ := json.Marshal(map[string]any{
		"name": "web", "repo_url": "https://x/y.git", "compose_path": "dc.yaml",
		"host_id": "ghost",
	})
	req := httptest.NewRequest("POST", "/api/v1/stacks", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "unknown host")
}
