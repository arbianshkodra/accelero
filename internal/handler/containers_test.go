package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/store"
	volumepkg "github.com/arbianshkodra/accelero/internal/volume"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
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
	statsByID       map[string]container.StatsResponse
	statsStreamByID map[string][]container.StatsResponse
	statsErr        error
	restartCalls  []restartCall
	restartErr    error
	execCreateFn  func(id string, opts client.ExecCreateOptions) (client.ExecCreateResult, error)
	execAttachFn  func(id string, opts client.ExecAttachOptions) (client.ExecAttachResult, error)
	execInspectFn func(id string) (client.ExecInspectResult, error)
	eventsResult  client.EventsResult
	imageResult   client.ImageListResult
	imageErr      error
	volumeResult  client.VolumeListResult
	volumeErr     error
	networkResult client.NetworkListResult
	networkErr    error
}

type restartCall struct {
	ID      string
	Timeout *int
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

func (m *mockDocker) Events(ctx context.Context, _ client.EventsListOptions) client.EventsResult {
	return m.eventsResult
}

func (m *mockDocker) ContainerRestart(ctx context.Context, id string, opts client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	m.restartCalls = append(m.restartCalls, restartCall{ID: id, Timeout: opts.Timeout})
	return client.ContainerRestartResult{}, m.restartErr
}

func (m *mockDocker) ExecCreate(_ context.Context, id string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if m.execCreateFn != nil {
		return m.execCreateFn(id, opts)
	}
	return client.ExecCreateResult{ID: "exec-" + id}, nil
}

func (m *mockDocker) ExecAttach(_ context.Context, id string, opts client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if m.execAttachFn != nil {
		return m.execAttachFn(id, opts)
	}
	return client.ExecAttachResult{}, errNotFound("exec")
}

func (m *mockDocker) ExecInspect(_ context.Context, id string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	if m.execInspectFn != nil {
		return m.execInspectFn(id)
	}
	return client.ExecInspectResult{}, errNotFound("exec")
}

func (m *mockDocker) ImageList(ctx context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	return m.imageResult, m.imageErr
}

func (m *mockDocker) VolumeList(ctx context.Context, _ client.VolumeListOptions) (client.VolumeListResult, error) {
	return m.volumeResult, m.volumeErr
}

func (m *mockDocker) NetworkList(ctx context.Context, _ client.NetworkListOptions) (client.NetworkListResult, error) {
	return m.networkResult, m.networkErr
}

func (m *mockDocker) ContainerStats(ctx context.Context, id string, _ client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	if m.statsErr != nil {
		return client.ContainerStatsResult{}, m.statsErr
	}
	// Streaming case first: the stream-stats endpoint decodes multiple
	// concatenated JSON objects from the body. One-shot /stats only
	// decodes the first, so a populated stream slice works for both
	// use cases in tests.
	if samples, ok := m.statsStreamByID[id]; ok {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, s := range samples {
			if err := enc.Encode(s); err != nil {
				return client.ContainerStatsResult{}, err
			}
		}
		return client.ContainerStatsResult{Body: io.NopCloser(&buf)}, nil
	}
	stats, ok := m.statsByID[id]
	if !ok {
		return client.ContainerStatsResult{}, errNotFound("container")
	}
	buf, err := json.Marshal(stats)
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(buf))}, nil
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

// ---------------------------------------------------------------------------
// Stats: pure computeStatsSample math
// ---------------------------------------------------------------------------

func TestComputeCPUPercent_Basic(t *testing.T) {
	// cpuDelta = 100ns, systemDelta = 1000ns, onlineCPUs = 2
	// percent = (100/1000) * 2 * 100 = 20%
	s := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1100},
			SystemUsage: 11000,
			OnlineCPUs:  2,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1000},
			SystemUsage: 10000,
		},
	}
	assert.InDelta(t, 20.0, computeCPUPercent(s), 0.001)
}

func TestComputeCPUPercent_FallsBackToPerCPUUsageCount(t *testing.T) {
	// OnlineCPUs missing — derive from len(PercpuUsage).
	s := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 400, PercpuUsage: []uint64{100, 100, 100, 100}},
			SystemUsage: 4000,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 300},
			SystemUsage: 3000,
		},
	}
	// delta = (100/1000) * 4 * 100 = 40
	assert.InDelta(t, 40.0, computeCPUPercent(s), 0.001)
}

func TestComputeCPUPercent_IdenticalSamplesIsZero(t *testing.T) {
	// Truly idle: both samples report the same counters, delta is 0.
	// Guard against emitting NaN or bogus negative percents.
	s := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1000},
			SystemUsage: 10000,
			OnlineCPUs:  4,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1000},
			SystemUsage: 10000,
		},
	}
	assert.Equal(t, 0.0, computeCPUPercent(s))
}

func TestComputeMemoryStats_SubtractsCacheCgroupV1(t *testing.T) {
	got := computeMemoryStats(container.MemoryStats{
		Usage: 500,
		Limit: 1000,
		Stats: map[string]uint64{"cache": 100},
	})
	assert.Equal(t, uint64(400), got.UsageBytes, "cache subtracted")
	assert.Equal(t, uint64(1000), got.LimitBytes)
	assert.InDelta(t, 40.0, got.Percent, 0.001)
}

func TestComputeMemoryStats_SubtractsFileCgroupV2(t *testing.T) {
	got := computeMemoryStats(container.MemoryStats{
		Usage: 500,
		Limit: 1000,
		Stats: map[string]uint64{"file": 200},
	})
	assert.Equal(t, uint64(300), got.UsageBytes)
	assert.InDelta(t, 30.0, got.Percent, 0.001)
}

func TestComputeMemoryStats_NoLimitMeansZeroPercent(t *testing.T) {
	got := computeMemoryStats(container.MemoryStats{Usage: 500})
	assert.Equal(t, uint64(500), got.UsageBytes)
	assert.Equal(t, 0.0, got.Percent)
}

func TestComputeBlockIOStats_SumsAcrossDevices(t *testing.T) {
	got := computeBlockIOStats(container.BlkioStats{IoServiceBytesRecursive: []container.BlkioStatEntry{
		{Major: 8, Minor: 0, Op: "Read", Value: 100},
		{Major: 8, Minor: 0, Op: "Write", Value: 200},
		{Major: 8, Minor: 16, Op: "Read", Value: 50},
		{Major: 8, Minor: 0, Op: "Sync", Value: 999}, // ignored
	}})
	assert.Equal(t, uint64(150), got.ReadBytes)
	assert.Equal(t, uint64(200), got.WriteBytes)
}

// ---------------------------------------------------------------------------
// Stats: endpoint wiring
// ---------------------------------------------------------------------------

func TestGetStackContainerStats_HappyPath(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	stats := container.StatsResponse{
		ID:     "cid",
		Name:   "/web_0_123",
		Read:   time.Unix(1_700_000_000, 0),
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1100},
			SystemUsage: 11000,
			OnlineCPUs:  2,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1000},
			SystemUsage: 10000,
		},
		MemoryStats: container.MemoryStats{
			Usage: 500,
			Limit: 1000,
			Stats: map[string]uint64{"cache": 100},
		},
		Networks: map[string]container.NetworkStats{
			"eth0": {RxBytes: 1024, TxBytes: 2048},
		},
		BlkioStats: container.BlkioStats{IoServiceBytesRecursive: []container.BlkioStatEntry{
			{Op: "Read", Value: 512},
			{Op: "Write", Value: 1024},
		}},
		PidsStats: container.PidsStats{Current: 7},
	}
	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID:   "cid",
				Name: "/web_0_123",
				Config: &container.Config{Labels: map[string]string{
					"managed-by":     "accelero",
					"accelero-stack": "demo",
				}},
			}},
		},
		statsByID: map[string]container.StatsResponse{"cid": stats},
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/stacks/demo/containers/cid/stats", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got ContainerStatsSample
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "cid", got.ContainerID)
	assert.Equal(t, "web_0_123", got.Name)
	assert.InDelta(t, 20.0, got.CPU.Percent, 0.001)
	assert.Equal(t, uint32(2), got.CPU.OnlineCPUs)
	assert.Equal(t, uint64(400), got.Memory.UsageBytes)
	assert.InDelta(t, 40.0, got.Memory.Percent, 0.001)
	assert.Equal(t, uint64(1024), got.Networks["eth0"].RxBytes)
	assert.Equal(t, uint64(2048), got.Networks["eth0"].TxBytes)
	assert.Equal(t, uint64(512), got.BlockIO.ReadBytes)
	assert.Equal(t, uint64(1024), got.BlockIO.WriteBytes)
	assert.Equal(t, uint64(7), got.PIDs)
}

func TestGetStackContainerStats_ForeignStack404(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
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

	req := httptest.NewRequest("GET", "/api/v1/stacks/ours/containers/cid/stats", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// ---------------------------------------------------------------------------
// Events: pure projection
// ---------------------------------------------------------------------------

func TestProjectEvent_Container(t *testing.T) {
	msg := events.Message{
		Type:     events.Type("container"),
		Action:   events.Action("start"),
		TimeNano: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC).UnixNano(),
		Actor: events.Actor{
			ID: "cid-full",
			Attributes: map[string]string{
				"name":             "web_0_123",
				"image":            "nginx:1.27.1-alpine",
				"accelero-service": "web",
				"accelero-replica": "0",
				"accelero-stack":   "demo",
				"managed-by":       "accelero",
			},
		},
	}
	got := projectEvent(msg)

	assert.Equal(t, "container", got.Type)
	assert.Equal(t, "start", got.Action)
	assert.Equal(t, "cid-full", got.ActorID)
	assert.Equal(t, "web_0_123", got.Name)
	assert.Equal(t, "nginx:1.27.1-alpine", got.Image)
	assert.Equal(t, "web", got.Service)
	require.NotNil(t, got.Replica)
	assert.Equal(t, 0, *got.Replica)
	assert.Equal(t, "2026-04-20T12:00:00Z", got.Time.UTC().Format(time.RFC3339))
	// Attributes pass through verbatim for client dig-ins.
	assert.Equal(t, "accelero", got.Attributes["managed-by"])
}

func TestProjectEvent_FallsBackToTimeSecondsWhenNanoMissing(t *testing.T) {
	msg := events.Message{
		Type:   events.Type("container"),
		Action: events.Action("die"),
		Time:   1_700_000_000,
		Actor:  events.Actor{ID: "cid"},
	}
	got := projectEvent(msg)
	assert.Equal(t, int64(1_700_000_000), got.Time.Unix())
}

func TestProjectEvent_UnlabelledContainer(t *testing.T) {
	// Legacy container with no accelero-service / replica labels —
	// shouldn't blow up, just skip the optional fields.
	msg := events.Message{
		Type:   events.Type("container"),
		Action: events.Action("destroy"),
		Actor: events.Actor{
			ID:         "cid",
			Attributes: map[string]string{"name": "orphan"},
		},
	}
	got := projectEvent(msg)
	assert.Equal(t, "orphan", got.Name)
	assert.Empty(t, got.Service)
	assert.Nil(t, got.Replica)
}

// ---------------------------------------------------------------------------
// Events: endpoint wiring
// ---------------------------------------------------------------------------

// TestStreamStackEvents_EndToEndSSE exercises the real streaming path:
// a channel of Docker events gets projected, serialized, and framed
// as SSE; the handler returns once the source closes. We don't use
// httptest.ResponseRecorder because SSE flushes require a real
// http.Flusher — stand up a test server instead.
func TestStreamStackEvents_EndToEndSSE(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}

	msgCh := make(chan events.Message, 4)
	errCh := make(chan error, 1)
	msgCh <- events.Message{
		Type:     events.Type("container"),
		Action:   events.Action("start"),
		TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{
			ID: "cid1",
			Attributes: map[string]string{
				"name":             "web_0_1",
				"accelero-service": "web",
				"accelero-replica": "0",
			},
		},
	}
	msgCh <- events.Message{
		Type:     events.Type("container"),
		Action:   events.Action("die"),
		TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{
			ID: "cid1",
			Attributes: map[string]string{
				"name":             "web_0_1",
				"accelero-service": "web",
				"accelero-replica": "0",
			},
		},
	}
	close(msgCh) // source closes → handler exits the loop

	docker := &mockDocker{eventsResult: client.EventsResult{Messages: msgCh, Err: errCh}}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/stacks/demo/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	// Two events, each framed as "data: {...}\n\n".
	assert.Equal(t, 2, strings.Count(text, "data: "),
		"expected two SSE data frames; body was:\n%s", text)
	assert.Contains(t, text, `"action":"start"`)
	assert.Contains(t, text, `"action":"die"`)
	assert.Contains(t, text, `"service":"web"`)
	assert.Contains(t, text, `"replica":0`)
}

func TestStreamStackEvents_UnknownStack404(t *testing.T) {
	h := newTestHandler(&mockStore{}, &mockDocker{})
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/stacks/nope/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStreamStackEvents_BadSinceRejected(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := newTestHandler(ms, &mockDocker{})
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/stacks/demo/events?since=yesterday")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestStreamStackEvents_DockerUnconfigured503(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}} // no Docker
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/stacks/demo/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Resource browsers: images
// ---------------------------------------------------------------------------

func TestListManagedImages_JoinsContainerUsageWithImageList(t *testing.T) {
	replica0 := 0
	replica1 := 1

	docker := &mockDocker{
		listResult: client.ContainerListResult{Items: []container.Summary{
			{
				ID:      "c-a-0",
				ImageID: "sha256:img-web",
				Labels: map[string]string{
					"managed-by":       "accelero",
					"accelero-stack":   "alpha",
					"accelero-service": "web",
					"accelero-replica": "0",
				},
			},
			{
				ID:      "c-a-1",
				ImageID: "sha256:img-web",
				Labels: map[string]string{
					"managed-by":       "accelero",
					"accelero-stack":   "alpha",
					"accelero-service": "web",
					"accelero-replica": "1",
				},
			},
			{
				ID:      "c-b-0",
				ImageID: "sha256:img-api",
				Labels: map[string]string{
					"managed-by":       "accelero",
					"accelero-stack":   "beta",
					"accelero-service": "api",
				},
			},
		}},
		imageResult: client.ImageListResult{Items: []image.Summary{
			{
				ID:       "sha256:img-web",
				RepoTags: []string{"nginx:1.27.1-alpine"},
				Size:     9_000_000,
				Created:  1_700_000_000,
			},
			{
				ID:       "sha256:img-api",
				RepoTags: []string{"myapp:v2"},
				Size:     120_000_000,
				Created:  1_700_000_500,
			},
			{
				// Unrelated local image — must not appear in the response.
				ID:       "sha256:img-orphan",
				RepoTags: []string{"orphan:latest"},
				Size:     1_000,
				Created:  1_700_000_900,
			},
		}},
	}

	h := newTestHandler(&mockStore{}, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/images", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got []ManagedImage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2, "only referenced images should be returned")

	byTag := map[string]ManagedImage{}
	for _, m := range got {
		byTag[m.RepoTags[0]] = m
	}
	assert.InDelta(t, 9_000_000, byTag["nginx:1.27.1-alpine"].SizeBytes, 0)
	require.Len(t, byTag["nginx:1.27.1-alpine"].UsedBy, 2)
	assert.Equal(t, "alpha", byTag["nginx:1.27.1-alpine"].UsedBy[0].Stack)
	require.NotNil(t, byTag["nginx:1.27.1-alpine"].UsedBy[0].Replica)
	assert.Equal(t, replica0, *byTag["nginx:1.27.1-alpine"].UsedBy[0].Replica)
	assert.Equal(t, replica1, *byTag["nginx:1.27.1-alpine"].UsedBy[1].Replica)

	require.Len(t, byTag["myapp:v2"].UsedBy, 1)
	assert.Equal(t, "beta", byTag["myapp:v2"].UsedBy[0].Stack)
}

func TestListManagedImages_NoContainers_EmptyArray(t *testing.T) {
	// When there are no managed containers, we should return [], not
	// null — clients iterating the response shouldn't have to care.
	h := newTestHandler(&mockStore{}, &mockDocker{})
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/images", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "[]\n", rr.Body.String())
}

func TestListManagedImages_DockerUnconfigured503(t *testing.T) {
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/images", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// ---------------------------------------------------------------------------
// Resource browsers: volumes
// ---------------------------------------------------------------------------

func TestListManagedVolumes_ProjectsAndSorts(t *testing.T) {
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: []volume.Volume{
		{
			Name:       "accelero_beta_pg_data",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/accelero_beta_pg_data/_data",
			CreatedAt:  "2026-04-20T10:00:00Z",
			Labels: map[string]string{
				"managed-by":     "accelero",
				"accelero-stack": "beta",
			},
		},
		{
			Name:       "accelero_alpha_cache",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/accelero_alpha_cache/_data",
			CreatedAt:  "2026-04-20T09:00:00Z",
			Labels: map[string]string{
				"managed-by":     "accelero",
				"accelero-stack": "alpha",
			},
		},
	}}}

	h := newTestHandler(&mockStore{}, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/volumes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got []ManagedVolume
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	// Sorted by name.
	assert.Equal(t, "accelero_alpha_cache", got[0].Name)
	assert.Equal(t, "alpha", got[0].Stack)
	assert.Equal(t, "accelero_beta_pg_data", got[1].Name)
	assert.Equal(t, "beta", got[1].Stack)
}

func TestListManagedVolumes_Empty(t *testing.T) {
	h := newTestHandler(&mockStore{}, &mockDocker{})
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/volumes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "[]\n", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Resource browsers: networks
// ---------------------------------------------------------------------------

func TestListManagedNetworks_ProjectsAndSorts(t *testing.T) {
	created := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	docker := &mockDocker{networkResult: client.NetworkListResult{Items: []network.Summary{
		{Network: network.Network{
			ID:      "net-beta-id",
			Name:    "accelero_beta_app",
			Driver:  "bridge",
			Scope:   "local",
			Created: created,
			Labels:  map[string]string{"managed-by": "accelero", "accelero-stack": "beta"},
			Options: map[string]string{"com.docker.network.bridge.name": "beta-br"},
		}},
		{Network: network.Network{
			ID:      "net-alpha-id",
			Name:    "accelero_alpha_app",
			Driver:  "bridge",
			Scope:   "local",
			Created: created,
			Labels:  map[string]string{"managed-by": "accelero", "accelero-stack": "alpha"},
		}},
	}}}

	h := newTestHandler(&mockStore{}, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/networks", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got []ManagedNetwork
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	// Sorted by name.
	assert.Equal(t, "accelero_alpha_app", got[0].Name)
	assert.Equal(t, "alpha", got[0].Stack)
	assert.Equal(t, "bridge", got[0].Driver)
	assert.Equal(t, "accelero_beta_app", got[1].Name)
	assert.Equal(t, "beta", got[1].Stack)
	assert.Equal(t, "beta-br", got[1].Options["com.docker.network.bridge.name"])
}

func TestListManagedNetworks_DockerError_500(t *testing.T) {
	docker := &mockDocker{networkErr: errReadyzTest("docker is down")}
	h := newTestHandler(&mockStore{}, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/networks", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// ---------------------------------------------------------------------------
// WebSocket log follow
// ---------------------------------------------------------------------------

// dialWSLogs upgrades a WebSocket connection to the log-stream endpoint
// under a test server URL. Returns the conn and the upgrade response.
func dialWSLogs(t *testing.T, serverURL, path string) (*websocket.Conn, *http.Response) {
	t.Helper()
	// httptest.NewServer gives http://... — rewrite the scheme for the ws dial.
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + path
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "ws dial: %v", err)
	return conn, resp
}

// frameMulti wraps multiple payloads as a single concatenated stdcopy
// stream.  mock's ContainerLogs returns the full bytes at once; each
// frame is "[header 8 bytes][payload]" so a single Write can yield
// many lines once the handler demuxes.
func frameMulti(payloads ...string) []byte {
	var b bytes.Buffer
	for _, p := range payloads {
		b.Write(frameStdoutFrame([]byte(p)))
	}
	return b.Bytes()
}

func TestStreamStackContainerLogs_NonTTYDemuxedLines(t *testing.T) {
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
		logsByID:     map[string]string{"cid": "line-a\nline-b\nline-c\n"},
		logsIsFramed: true,
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, _ := dialWSLogs(t, srv.URL, "/api/v1/stacks/demo/containers/cid/logs/stream")
	defer conn.Close()

	got := make([]string, 0, 3)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < 3; i++ {
		msgType, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, websocket.TextMessage, msgType)
		got = append(got, string(msg))
	}
	assert.Equal(t, []string{"line-a", "line-b", "line-c"}, got)

	// Next read should be a clean close (daemon stream ended).
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseNormalClosure, closeErr.Code,
		"expected server to send a normal-closure close frame, got %v", err)
}

func TestStreamStackContainerLogs_TTYPassThrough(t *testing.T) {
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
		logsByID:     map[string]string{"cid": "tty-line-1\ntty-line-2\n"},
		logsIsFramed: false, // TTY: no stdcopy framing, raw stream
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, _ := dialWSLogs(t, srv.URL, "/api/v1/stacks/demo/containers/cid/logs/stream")
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg1, err := conn.ReadMessage()
	require.NoError(t, err)
	_, msg2, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "tty-line-1", string(msg1))
	assert.Equal(t, "tty-line-2", string(msg2))
}

func TestStreamStackContainerLogs_ForeignStack404(t *testing.T) {
	// Container exists but is labelled for a different stack. The
	// handler refuses the upgrade with a 404 — the WS dial fails.
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
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

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/ours/containers/cid/logs/stream"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err, "expected dial failure")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStreamStackContainerLogs_DockerUnconfigured503(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}} // no Docker
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/logs/stream"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// Keep the stdcopy multi-frame helper compiled in case future tests want it.
var _ = frameMulti

// ---------------------------------------------------------------------------
// Container restart
// ---------------------------------------------------------------------------

func TestRestartStackContainer_HappyPath(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{Labels: map[string]string{
					"managed-by":       "accelero",
					"accelero-stack":   "demo",
					"accelero-service": "web",
					"accelero-replica": "0",
				}},
			}},
		},
	}
	recorder := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Docker: docker, Audit: recorder}

	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/stacks/demo/containers/cid/restart?t=5", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)
	require.Len(t, docker.restartCalls, 1)
	assert.Equal(t, "cid", docker.restartCalls[0].ID)
	require.NotNil(t, docker.restartCalls[0].Timeout)
	assert.Equal(t, 5, *docker.restartCalls[0].Timeout)

	// Audit row captured the operator action with service/replica context.
	require.Len(t, recorder.entries, 1)
	got := recorder.entries[0]
	assert.Equal(t, "container.restart", got.Operation)
	assert.Equal(t, "api-key", got.Actor)
	assert.Equal(t, "cid", got.ResourceID)
	assert.Equal(t, store.AuditOutcomeSuccess, got.Outcome)
	assert.Equal(t, "web", got.Metadata["service"])
	assert.Equal(t, "0", got.Metadata["replica"])
	assert.Equal(t, "5", got.Metadata["timeout_seconds"])
}

func TestRestartStackContainer_DefaultTimeoutUnset(t *testing.T) {
	// No ?t= → Options.Timeout stays nil (Docker uses its default).
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

	req := httptest.NewRequest("POST", "/api/v1/stacks/demo/containers/cid/restart", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusAccepted, rr.Code)
	require.Len(t, docker.restartCalls, 1)
	assert.Nil(t, docker.restartCalls[0].Timeout, "no ?t= → Timeout pointer stays nil")
}

func TestRestartStackContainer_BadTimeout400(t *testing.T) {
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

	req := httptest.NewRequest("POST", "/api/v1/stacks/demo/containers/cid/restart?t=not-a-number", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Empty(t, docker.restartCalls, "handler must reject before calling Docker")
}

func TestRestartStackContainer_ForeignStack404(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
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

	req := httptest.NewRequest("POST", "/api/v1/stacks/ours/containers/cid/restart", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Empty(t, docker.restartCalls, "foreign container: no Docker call issued")
}

func TestRestartStackContainer_DockerErrorStillAudits(t *testing.T) {
	// If Docker returns an error, the handler returns 500 — but the
	// audit row is still written with Outcome=failure so the trail
	// captures the attempted operator action regardless.
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{Labels: map[string]string{
					"managed-by":     "accelero",
					"accelero-stack": "demo",
				}},
			}},
		},
		restartErr: errReadyzTest("docker daemon unhappy"),
	}
	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Docker: docker, Audit: rec}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/stacks/demo/containers/cid/restart", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Len(t, rec.entries, 1, "audit row must be written even on failure")
	assert.Equal(t, store.AuditOutcomeFailure, rec.entries[0].Outcome)
	assert.Contains(t, rec.entries[0].ErrorMessage, "docker daemon unhappy")
}

// ---------------------------------------------------------------------------
// WebSocket stats stream
// ---------------------------------------------------------------------------

func TestStreamStackContainerStats_EmitsOneMessagePerSample(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}

	// Two samples — the second has CPU/memory deltas against PreCPUStats
	// so the projected percent works out to a non-zero value, and the
	// client reader can assert on that transition.
	samples := []container.StatsResponse{
		{
			ID:   "cid",
			Name: "/web_0_1",
			Read: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
			CPUStats: container.CPUStats{
				CPUUsage:    container.CPUUsage{TotalUsage: 1000},
				SystemUsage: 10000,
				OnlineCPUs:  2,
			},
			// Same values as CPUStats so delta is 0 → "idle" first reading.
			// Matches real Docker streams: the daemon populates PreCPUStats
			// with an initial sample before the first message is sent.
			PreCPUStats: container.CPUStats{
				CPUUsage:    container.CPUUsage{TotalUsage: 1000},
				SystemUsage: 10000,
			},
			MemoryStats: container.MemoryStats{Usage: 500, Limit: 1000},
		},
		{
			ID:   "cid",
			Name: "/web_0_1",
			Read: time.Date(2026, 4, 20, 12, 0, 1, 0, time.UTC),
			CPUStats: container.CPUStats{
				CPUUsage:    container.CPUUsage{TotalUsage: 1100},
				SystemUsage: 11000,
				OnlineCPUs:  2,
			},
			PreCPUStats: container.CPUStats{
				CPUUsage:    container.CPUUsage{TotalUsage: 1000},
				SystemUsage: 10000,
			},
			MemoryStats: container.MemoryStats{Usage: 600, Limit: 1000},
		},
	}

	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{Labels: map[string]string{
					"managed-by":     "accelero",
					"accelero-stack": "demo",
				}},
			}},
		},
		statsStreamByID: map[string][]container.StatsResponse{"cid": samples},
	}

	h := newTestHandler(ms, docker)
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, _ := dialWSLogs(t, srv.URL, "/api/v1/stacks/demo/containers/cid/stats/stream")
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	var first ContainerStatsSample
	_, msg1, err := conn.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(msg1, &first))
	assert.Equal(t, "web_0_1", first.Name)
	// No prior sample on server side yet → 0%.
	assert.Equal(t, 0.0, first.CPU.Percent)
	assert.Equal(t, uint64(500), first.Memory.UsageBytes)

	var second ContainerStatsSample
	_, msg2, err := conn.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(msg2, &second))
	assert.InDelta(t, 20.0, second.CPU.Percent, 0.001,
		"second sample has PreCPUStats filled; formula yields 20%%")
	assert.Equal(t, uint64(600), second.Memory.UsageBytes)

	// Source ended → normal closure.
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseNormalClosure, closeErr.Code)
}

func TestStreamStackContainerStats_ForeignStack404(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
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

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/ours/containers/cid/stats/stream"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStreamStackContainerStats_DockerUnconfigured503(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}} // no Docker
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/stats/stream"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Volume browser endpoint
// ---------------------------------------------------------------------------

// stubBrowser is a handler-package test double for volumepkg.Browser. It
// doesn't need to exercise the Docker helper lifecycle — handler tests
// only care that the right volume name + path reach the browser and
// the response is faithfully returned to the HTTP client.
type stubBrowser struct {
	listCalls  []stubBrowseCall
	readCalls  []stubBrowseCall
	writeCalls []stubWriteCall
	listFn     func(volume, p string) ([]volumepkg.FileEntry, error)
	readFn     func(volume, p string, max int64) (volumepkg.ReadResult, error)
	writeFn    func(volume, p string, mode uint32, body []byte) error
}

type stubBrowseCall struct {
	Volume string
	Path   string
	Max    int64
}

type stubWriteCall struct {
	Volume string
	Path   string
	Mode   uint32
	Body   []byte
}

func (s *stubBrowser) ListPath(_ context.Context, v, p string) ([]volumepkg.FileEntry, error) {
	s.listCalls = append(s.listCalls, stubBrowseCall{Volume: v, Path: p})
	if s.listFn != nil {
		return s.listFn(v, p)
	}
	return nil, nil
}

func (s *stubBrowser) ReadFile(_ context.Context, v, p string, max int64) (volumepkg.ReadResult, error) {
	s.readCalls = append(s.readCalls, stubBrowseCall{Volume: v, Path: p, Max: max})
	if s.readFn != nil {
		return s.readFn(v, p, max)
	}
	return volumepkg.ReadResult{}, nil
}

func (s *stubBrowser) WriteFile(_ context.Context, v, p string, mode uint32, content io.Reader, _ int64) error {
	body, _ := io.ReadAll(content)
	s.writeCalls = append(s.writeCalls, stubWriteCall{Volume: v, Path: p, Mode: mode, Body: body})
	if s.writeFn != nil {
		return s.writeFn(v, p, mode, body)
	}
	return nil
}

// volumeTestFixture constructs a handler wired to mocks that know about
// one managed volume ("mine"). Reduces boilerplate across the browse tests.
func volumeTestFixture(t *testing.T, browser volumepkg.Browser) (*Handler, *mux.Router) {
	t.Helper()
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: []volume.Volume{
		{Name: "mine", Driver: "local", Labels: map[string]string{
			"managed-by":     "accelero",
			"accelero-stack": "ours",
		}},
	}}}
	rec := &captureRecorder{}
	h := &Handler{
		Store:         &mockStore{},
		Deployer:      &mockDeployer{},
		Docker:        docker,
		VolumeBrowser: browser,
		Audit:         rec,
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	return h, router
}

func TestBrowseVolume_ListHappyPath(t *testing.T) {
	browser := &stubBrowser{
		listFn: func(vol, p string) ([]volumepkg.FileEntry, error) {
			assert.Equal(t, "mine", vol)
			assert.Equal(t, "/config", p)
			return []volumepkg.FileEntry{
				{Name: "app.yml", Path: "/config/app.yml", SizeBytes: 42, Mode: "-rw-r--r--"},
				{Name: "secrets", Path: "/config/secrets", IsDir: true, Mode: "drwx------"},
			}, nil
		},
	}
	h, router := volumeTestFixture(t, browser)

	req := httptest.NewRequest("GET", "/api/v1/volumes/mine/browse?path=/config", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got []volumepkg.FileEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "app.yml", got[0].Name)
	assert.Equal(t, int64(42), got[0].SizeBytes)
	assert.True(t, got[1].IsDir)

	// Audit — one volume.browse row with outcome=success.
	rec := h.Audit.(*captureRecorder)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, "volume.browse", rec.entries[0].Operation)
	assert.Equal(t, "success", rec.entries[0].Outcome)
	assert.Equal(t, "mine", rec.entries[0].ResourceID)
	assert.Equal(t, "/config", rec.entries[0].Metadata["path"])
}

func TestBrowseVolume_UnmanagedVolume404(t *testing.T) {
	// Volume exists per Docker but isn't managed by accelero → 404,
	// and the browser is NEVER called.
	browser := &stubBrowser{}
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: nil}}
	h := &Handler{
		Store:         &mockStore{},
		Deployer:      &mockDeployer{},
		Docker:        docker,
		VolumeBrowser: browser,
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/volumes/unmanaged/browse", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Empty(t, browser.listCalls, "browser must not be invoked for unmanaged volumes")
}

func TestBrowseVolume_DefaultPathIsRoot(t *testing.T) {
	browser := &stubBrowser{
		listFn: func(_, p string) ([]volumepkg.FileEntry, error) {
			assert.Equal(t, "/", p, "omitted ?path= defaults to /")
			return []volumepkg.FileEntry{}, nil
		},
	}
	_, router := volumeTestFixture(t, browser)
	req := httptest.NewRequest("GET", "/api/v1/volumes/mine/browse", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestBrowseVolume_BrowserErrorAuditedAsFailure(t *testing.T) {
	browser := &stubBrowser{
		listFn: func(_, _ string) ([]volumepkg.FileEntry, error) {
			return nil, errReadyzTest("tar parse bad magic")
		},
	}
	h, router := volumeTestFixture(t, browser)

	req := httptest.NewRequest("GET", "/api/v1/volumes/mine/browse?path=/bad", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rec := h.Audit.(*captureRecorder)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, "failure", rec.entries[0].Outcome)
	assert.Contains(t, rec.entries[0].ErrorMessage, "tar parse bad magic")
}

func TestBrowseVolume_DownloadStreamsBody(t *testing.T) {
	contents := []byte("hello from the volume")
	browser := &stubBrowser{
		readFn: func(vol, p string, max int64) (volumepkg.ReadResult, error) {
			assert.Equal(t, "mine", vol)
			assert.Equal(t, "/greet.txt", p)
			assert.Equal(t, int64(maxVolumeFileDownload), max)
			return volumepkg.ReadResult{
				Content:   io.NopCloser(bytes.NewReader(contents)),
				Name:      "greet.txt",
				Path:      "/greet.txt",
				SizeBytes: int64(len(contents)),
				Mode:      "-rw-r--r--",
			}, nil
		},
	}
	h, router := volumeTestFixture(t, browser)

	req := httptest.NewRequest("GET", "/api/v1/volumes/mine/browse?path=/greet.txt&download=true", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/octet-stream", rr.Header().Get("Content-Type"))
	assert.Contains(t, rr.Header().Get("Content-Disposition"), `filename="greet.txt"`)
	assert.Equal(t, string(contents), rr.Body.String())

	rec := h.Audit.(*captureRecorder)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, "volume.read", rec.entries[0].Operation)
	assert.Equal(t, "success", rec.entries[0].Outcome)
	assert.Equal(t, "21", rec.entries[0].Metadata["size_bytes"])
}

func TestBrowseVolume_BrowserUnconfigured503(t *testing.T) {
	// Volume browsing requires both Docker (for the label check) and the
	// browser. Nil browser → 503 even if Docker is available.
	h := &Handler{
		Store:    &mockStore{},
		Deployer: &mockDeployer{},
		Docker:   &mockDocker{},
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("GET", "/api/v1/volumes/mine/browse", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// ---------------------------------------------------------------------------
// Volume file write (POST /files)
// ---------------------------------------------------------------------------

// volumeWriteFixture wires a handler with AllowVolumeWrites=true and one
// managed volume named "mine". Tests that want the write path disabled
// build their own handler without the flag set.
func volumeWriteFixture(t *testing.T, browser volumepkg.Browser) (*Handler, *mux.Router) {
	t.Helper()
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: []volume.Volume{
		{Name: "mine", Driver: "local", Labels: map[string]string{
			"managed-by":     "accelero",
			"accelero-stack": "ours",
		}},
	}}}
	rec := &captureRecorder{}
	h := &Handler{
		Store:             &mockStore{},
		Deployer:          &mockDeployer{},
		Docker:            docker,
		VolumeBrowser:     browser,
		Audit:             rec,
		AllowVolumeWrites: true,
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)
	return h, router
}

func TestWriteVolumeFile_HappyPath(t *testing.T) {
	browser := &stubBrowser{}
	h, router := volumeWriteFixture(t, browser)

	body := bytes.NewReader([]byte("new contents\n"))
	req := httptest.NewRequest("POST", "/api/v1/volumes/mine/files?path=/config/app.yml&mode=0640", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Browser received the right inputs including the parsed mode.
	require.Len(t, browser.writeCalls, 1)
	assert.Equal(t, "mine", browser.writeCalls[0].Volume)
	assert.Equal(t, "/config/app.yml", browser.writeCalls[0].Path)
	assert.Equal(t, uint32(0o640), browser.writeCalls[0].Mode)
	assert.Equal(t, "new contents\n", string(browser.writeCalls[0].Body))

	// Audit success row present with path, size, mode.
	rec := h.Audit.(*captureRecorder)
	require.Len(t, rec.entries, 1)
	got := rec.entries[0]
	assert.Equal(t, "volume.write", got.Operation)
	assert.Equal(t, "success", got.Outcome)
	assert.Equal(t, "/config/app.yml", got.Metadata["path"])
	assert.Equal(t, "13", got.Metadata["size_bytes"])
	assert.Equal(t, "0640", got.Metadata["mode"])
}

func TestWriteVolumeFile_DisabledByDefault(t *testing.T) {
	// The flag defaults to false; request should 403 without the
	// browser ever being invoked.
	browser := &stubBrowser{}
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: []volume.Volume{
		{Name: "mine", Labels: map[string]string{"managed-by": "accelero"}},
	}}}
	h := &Handler{
		Store:         &mockStore{},
		Deployer:      &mockDeployer{},
		Docker:        docker,
		VolumeBrowser: browser,
		// AllowVolumeWrites: false (default)
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/volumes/mine/files?path=/x", bytes.NewReader([]byte("data")))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Empty(t, browser.writeCalls, "disabled endpoint must not invoke the browser")
}

func TestWriteVolumeFile_MissingPath400(t *testing.T) {
	browser := &stubBrowser{}
	_, router := volumeWriteFixture(t, browser)

	req := httptest.NewRequest("POST", "/api/v1/volumes/mine/files", bytes.NewReader([]byte("data")))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Empty(t, browser.writeCalls)
}

func TestWriteVolumeFile_UnmanagedVolume404(t *testing.T) {
	browser := &stubBrowser{}
	docker := &mockDocker{volumeResult: client.VolumeListResult{Items: nil}}
	h := &Handler{
		Store:             &mockStore{},
		Deployer:          &mockDeployer{},
		Docker:            docker,
		VolumeBrowser:     browser,
		AllowVolumeWrites: true,
	}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	req := httptest.NewRequest("POST", "/api/v1/volumes/nope/files?path=/x", bytes.NewReader([]byte("data")))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Empty(t, browser.writeCalls)
}

func TestWriteVolumeFile_BrowserFailureIsAudited(t *testing.T) {
	browser := &stubBrowser{writeFn: func(_, _ string, _ uint32, _ []byte) error {
		return errReadyzTest("volume is full")
	}}
	h, router := volumeWriteFixture(t, browser)

	req := httptest.NewRequest("POST", "/api/v1/volumes/mine/files?path=/x", bytes.NewReader([]byte("data")))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rec := h.Audit.(*captureRecorder)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, "failure", rec.entries[0].Outcome)
	assert.Contains(t, rec.entries[0].ErrorMessage, "volume is full")
}

func TestWriteVolumeFile_OversizedRejected(t *testing.T) {
	// Body larger than maxVolumeFileUpload is rejected before the
	// browser is invoked. Use a payload 1 byte over the cap so the
	// test doesn't actually allocate 10MB for no reason.
	browser := &stubBrowser{}
	_, router := volumeWriteFixture(t, browser)

	payload := bytes.Repeat([]byte{'X'}, maxVolumeFileUpload+1)
	req := httptest.NewRequest("POST", "/api/v1/volumes/mine/files?path=/x", bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	assert.Empty(t, browser.writeCalls)
}

// ---------------------------------------------------------------------------
// WebSocket exec
// ---------------------------------------------------------------------------

// execFixture wires a handler with one managed container ready to exec.
// The docker exec side is driven via net.Pipe so tests can act as the
// container, writing to stdout and reading stdin as a real daemon would.
func execFixture(t *testing.T) (*Handler, *mockDocker, *captureRecorder, net.Conn, *mux.Router) {
	t.Helper()
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}

	// Pipe: one side given to the handler as the exec connection,
	// the other returned to the test so it can play "docker".
	serverConn, testConn := net.Pipe()

	docker := &mockDocker{
		inspectByID: map[string]client.ContainerInspectResult{
			"cid": {Container: container.InspectResponse{
				ID: "cid",
				Config: &container.Config{Labels: map[string]string{
					"managed-by":     "accelero",
					"accelero-stack": "demo",
				}},
			}},
		},
		execCreateFn: func(id string, _ client.ExecCreateOptions) (client.ExecCreateResult, error) {
			return client.ExecCreateResult{ID: "exec-" + id}, nil
		},
		execAttachFn: func(_ string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
			return client.ExecAttachResult{
				HijackedResponse: client.NewHijackedResponse(serverConn, "application/vnd.docker.raw-stream"),
			}, nil
		},
		execInspectFn: func(_ string) (client.ExecInspectResult, error) {
			return client.ExecInspectResult{ExitCode: 0}, nil
		},
	}

	rec := &captureRecorder{}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}, Docker: docker, Audit: rec}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	return h, docker, rec, testConn, router
}

func TestExecStackContainer_Bidirectional(t *testing.T) {
	_, _, rec, testConn, router := execFixture(t)
	defer testConn.Close()

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/exec?cmd=sh"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	// "Docker" side: write bytes that should arrive on the WS as output,
	// and read bytes that the WS will send as stdin.
	errCh := make(chan error, 2)
	go func() {
		_, err := testConn.Write([]byte("hi from exec\n"))
		errCh <- err
	}()

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	msgType, payload, err := ws.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, msgType)
	assert.Equal(t, "hi from exec\n", string(payload))

	// Client → server stdin.
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("input-from-ws\n")))
	// Read it back from the "docker" side.
	buf := make([]byte, 64)
	testConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := testConn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "input-from-ws\n", string(buf[:n]))

	// Close the docker-side conn → handler's reader returns, WS closes
	// with a CloseNormalClosure frame containing the exit code.
	testConn.Close()

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = ws.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, websocket.CloseNormalClosure, ce.Code)
	assert.Contains(t, ce.Text, "exit_code=0")

	// Audit trail captured both start and end with matching resource IDs.
	require.GreaterOrEqual(t, len(rec.entries), 2)
	var gotStart, gotEnd bool
	for _, e := range rec.entries {
		switch e.Operation {
		case "container.exec_start":
			gotStart = true
			assert.Equal(t, "in_progress", e.Outcome)
			assert.Equal(t, "sh", e.Metadata["cmd"])
			assert.Equal(t, "true", e.Metadata["tty"])
		case "container.exec_end":
			gotEnd = true
			assert.Equal(t, "success", e.Outcome)
			assert.Equal(t, "0", e.Metadata["exit_code"])
			assert.Contains(t, e.Metadata, "duration_seconds")
		}
	}
	assert.True(t, gotStart, "exec_start audit row missing")
	assert.True(t, gotEnd, "exec_end audit row missing")
}

func TestExecStackContainer_MissingCmd400(t *testing.T) {
	_, _, _, _, router := execFixture(t)
	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/exec"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestExecStackContainer_NonTTYRejected400(t *testing.T) {
	// MVP is TTY-only — non-TTY requires stdcopy demux on the output
	// side to avoid returning Docker's 8-byte frame headers as if they
	// were data. Rejecting up front is better than returning garbage.
	_, _, _, _, router := execFixture(t)
	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/exec?cmd=ls&tty=false"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestExecStackContainer_ForeignStack404(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "ours", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	docker := &mockDocker{inspectByID: map[string]client.ContainerInspectResult{
		"cid": {Container: container.InspectResponse{
			ID: "cid",
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

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/ours/containers/cid/exec?cmd=sh"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestExecStackContainer_DockerUnconfigured503(t *testing.T) {
	now := time.Now()
	ms := &mockStore{stacks: []*store.Stack{
		{ID: "s1", Name: "demo", Status: "active", CreatedAt: now, UpdatedAt: now},
	}}
	h := &Handler{Store: ms, Deployer: &mockDeployer{}}
	router := mux.NewRouter()
	noAuth := func(next http.Handler) http.Handler { return next }
	h.RegisterRoutes(router, noAuth)

	srv := httptest.NewServer(router)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/v1/stacks/demo/containers/cid/exec?cmd=sh"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
