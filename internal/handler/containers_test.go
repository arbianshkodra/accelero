package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frameStdoutFrame wraps a payload in Docker's non-TTY multiplex format:
// an 8-byte header (stream type, 3 pad bytes, big-endian length) followed
// by the payload. Used to simulate what ContainerLogs returns so the
// handler's stdcopy.StdCopy demux path is exercised.
func frameStdoutFrame(payload []byte) []byte {
	const stdout byte = 1
	var buf bytes.Buffer
	header := make([]byte, 8)
	header[0] = stdout
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	buf.Write(header)
	buf.Write(payload)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestIsSecretKey(t *testing.T) {
	cases := map[string]bool{
		"DB_PASSWORD":        true,
		"password":           true,
		"API_TOKEN":          true,
		"ghcr_TOKEN":         true,
		"STRIPE_SECRET":      true,
		"PRIVATE_KEY":        true,
		"FOO_CREDENTIAL":     true,
		"APIKEY":             true,
		"API_KEY":            true,
		"PATH":               false,
		"HOME":               false,
		"NGINX_VERSION":      false,
		"LD_LIBRARY_PATH":    false,
	}
	for key, want := range cases {
		assert.Equalf(t, want, isSecretKey(key), "key=%s", key)
	}
}

func TestRedactEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"DB_PASSWORD=s3cret",
		"API_TOKEN=abc.def",
		"NO_EQUALS_SIGN",
		"STRIPE_SECRET=sk_live_xyz",
	}
	got := redactEnv(in)

	assert.Equal(t, []string{
		"PATH=/usr/bin",
		"DB_PASSWORD=***",
		"API_TOKEN=***",
		"NO_EQUALS_SIGN",
		"STRIPE_SECRET=***",
	}, got)

	assert.Nil(t, redactEnv(nil))
}

func TestContainerBelongsToStack(t *testing.T) {
	labelled := container.InspectResponse{
		Config: &container.Config{Labels: map[string]string{
			"managed-by":      "accelero",
			"accelero-stack":  "ours",
		}},
	}
	assert.True(t, containerBelongsToStack(labelled, "ours"))
	assert.False(t, containerBelongsToStack(labelled, "theirs"))

	noLabels := container.InspectResponse{Config: &container.Config{}}
	assert.False(t, containerBelongsToStack(noLabels, "ours"))

	unmanaged := container.InspectResponse{
		Config: &container.Config{Labels: map[string]string{
			"managed-by":     "portainer",
			"accelero-stack": "ours",
		}},
	}
	assert.False(t, containerBelongsToStack(unmanaged, "ours"))

	nilConfig := container.InspectResponse{}
	assert.False(t, containerBelongsToStack(nilConfig, "ours"))
}

// ---------------------------------------------------------------------------
// Mock Docker client
// ---------------------------------------------------------------------------

type mockDocker struct {
	listResult    client.ContainerListResult
	listErr       error
	inspectByID   map[string]client.ContainerInspectResult
	inspectErr    error
	logsByID      map[string]string
	logsIsFramed  bool // true → wrap body in stdcopy frames
	logsErr       error
}

func (m *mockDocker) ContainerList(ctx context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	return m.listResult, m.listErr
}

func (m *mockDocker) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if m.inspectErr != nil {
		return client.ContainerInspectResult{}, m.inspectErr
	}
	if res, ok := m.inspectByID[id]; ok {
		return res, nil
	}
	return client.ContainerInspectResult{}, errNotFound("container")
}

func (m *mockDocker) ContainerLogs(ctx context.Context, id string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	if m.logsErr != nil {
		return nil, m.logsErr
	}
	body, ok := m.logsByID[id]
	if !ok {
		return nil, errNotFound("container")
	}
	if m.logsIsFramed {
		return io.NopCloser(bytes.NewReader(frameStdoutFrame([]byte(body)))), nil
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// errNotFound makes the mock return a not-found the handler can recognise via
// cerrdefs.IsNotFound. The mock errors must implement the right interface —
// simplest way is wrapping an errdefs-typed error. Borrow from the same
// package the deployer uses.
func errNotFound(what string) error { return notFoundErr(what) }

type notFoundErr string

func (e notFoundErr) Error() string  { return string(e) + " not found" }
func (notFoundErr) NotFound()        {}

// ---------------------------------------------------------------------------
// List endpoint
// ---------------------------------------------------------------------------

func TestListStackContainers_ReturnsOnlyStackContainers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	// Docker filtered-list would do the label filter server-side; the mock
	// just returns whatever we put in listResult, simulating that the
	// filter already happened.
	replica0 := 0
	replica1 := 1
	docker := &mockDocker{listResult: client.ContainerListResult{Items: []container.Summary{
		{
			ID:      "cid-0",
			Names:   []string{"/web_0_123"},
			Image:   "nginx:1.27.1",
			Created: 1_700_000_000,
			State:   "running",
			Status:  "Up 2 minutes",
			Labels: map[string]string{
				"managed-by":       "accelero",
				"accelero-stack":   "demo",
				"accelero-service": "web",
				"accelero-replica": "0",
			},
			Ports: []container.PortSummary{
				{PrivatePort: 80, PublicPort: 8080, Type: "tcp", IP: netip.MustParseAddr("127.0.0.1")},
			},
			Health: &container.HealthSummary{Status: container.Healthy},
		},
		{
			ID:      "cid-1",
			Names:   []string{"/web_1_456"},
			Image:   "nginx:1.27.1",
			Created: 1_700_000_010,
			State:   "running",
			Status:  "Up 1 minute",
			Labels: map[string]string{
				"managed-by":       "accelero",
				"accelero-stack":   "demo",
				"accelero-service": "web",
				"accelero-replica": "1",
			},
		},
	}}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got []ContainerSummary
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)

	// Sorted by service then replica.
	assert.Equal(t, "web", got[0].Service)
	assert.NotNil(t, got[0].Replica)
	assert.Equal(t, 0, *got[0].Replica)
	assert.Equal(t, replica0, *got[0].Replica)
	assert.Equal(t, "healthy", got[0].Health)
	assert.Len(t, got[0].Ports, 1)
	assert.Equal(t, uint16(80), got[0].Ports[0].ContainerPort)
	assert.Equal(t, uint16(8080), got[0].Ports[0].HostPort)
	assert.Equal(t, "127.0.0.1", got[0].Ports[0].HostIP)

	assert.Equal(t, replica1, *got[1].Replica)
}

func TestListStackContainers_UnknownStackReturns404(t *testing.T) {
	h := newTestHandler(&mockStore{}, &mockDocker{})
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/nope/containers", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListStackContainers_DockerUnconfiguredReturns503(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}} // no Docker
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// ---------------------------------------------------------------------------
// Detail endpoint
// ---------------------------------------------------------------------------

func TestGetStackContainer_ForeignContainerReturns404(t *testing.T) {
	// Container exists and is managed by Accelero, but belongs to a
	// different stack. Must 404, not leak metadata.
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: "s2", Name: "theirs", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid-foreign": {Container: container.InspectResponse{
			ID:   "cid-foreign",
			Name: "/web_0_999",
			Config: &container.Config{Labels: map[string]string{
				"managed-by":     "accelero",
				"accelero-stack": "theirs",
			}},
		}},
	}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/ours/containers/cid-foreign", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, "foreign-stack container must 404, got %d: %s", rr.Code, rr.Body.String())
}

func TestGetStackContainer_RedactsSecretEnv(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid-ok": {Container: container.InspectResponse{
			ID:      "cid-ok",
			Name:    "/web_0_123",
			Created: "2026-04-20T12:00:00Z",
			State:   &container.State{Status: "running"},
			Config: &container.Config{
				Env: []string{
					"PATH=/usr/bin",
					"DB_PASSWORD=hunter2",
					"LOG_LEVEL=info",
				},
				Labels: map[string]string{
					"managed-by":       "accelero",
					"accelero-stack":   "demo",
					"accelero-service": "web",
				},
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"bridge": {Aliases: []string{"web"}},
				},
			},
		}},
	}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid-ok", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got ContainerDetail
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "web", got.Service)
	assert.Contains(t, got.Env, "PATH=/usr/bin")
	assert.Contains(t, got.Env, "DB_PASSWORD=***")
	assert.Contains(t, got.Env, "LOG_LEVEL=info")
	assert.Contains(t, got.Networks, "bridge")
}

// ---------------------------------------------------------------------------
// Logs endpoint
// ---------------------------------------------------------------------------

func TestGetStackContainerLogs_DemuxedNonTTY(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{
					Tty: false,
					Labels: map[string]string{
						"managed-by":     "accelero",
						"accelero-stack": "demo",
					},
				},
			}},
		},
		logsByID:     map[string]string{"cid": "line1\nline2\n"},
		logsIsFramed: true,
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid/logs?tail=50", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/plain")
	// Demux strips the Docker frame header and leaves plain text.
	assert.Equal(t, "line1\nline2\n", rr.Body.String())
}

func TestGetStackContainerLogs_TTYPassthrough(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{
					Tty: true,
					Labels: map[string]string{
						"managed-by":     "accelero",
						"accelero-stack": "demo",
					},
				},
			}},
		},
		logsByID:     map[string]string{"cid": "tty-output\n"},
		logsIsFramed: false,
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid/logs", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "tty-output\n", rr.Body.String())
}

func TestGetStackContainerLogs_BadTailRejected(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
			Config: &container.Config{Labels: map[string]string{
				"managed-by":     "accelero",
				"accelero-stack": "demo",
			}},
		}},
	}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid/logs?tail=not-a-number", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetStackContainerLogs_BadSinceRejected(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
			Config: &container.Config{Labels: map[string]string{
				"managed-by":     "accelero",
				"accelero-stack": "demo",
			}},
		}},
	}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid/logs?since=yesterday", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
