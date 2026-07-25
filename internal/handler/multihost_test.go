package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managedContainer builds a container summary carrying the accelero labels for
// the given stack.
func managedContainer(id, stackName, service string) container.Summary {
	return container.Summary{
		ID:      id,
		Names:   []string{"/" + service},
		Image:   "nginx:1.27",
		ImageID: "sha256:" + id,
		State:   "running",
		Labels: map[string]string{
			containerLabelManagedBy:    containerLabelManagedValue,
			containerLabelStackName:    stackName,
			containerLabelServiceName:  service,
			containerLabelReplicaIndex: "1",
		},
	}
}

// TestListStackContainers_UsesStackHost is the core of phase 2: a stack with a
// host_id must be introspected on THAT host, never the default one.
func TestListStackContainers_UsesStackHost(t *testing.T) {
	defaultDocker := &mockDocker{
		listResult: client.ContainerListResult{
			Items: []container.Summary{managedContainer("aaa", "web", "wrong-host")},
		},
	}
	remoteDocker := &mockDocker{
		listResult: client.ContainerListResult{
			Items: []container.Summary{managedContainer("bbb", "web", "right-host")},
		},
	}

	ms := &mockStore{stacks: []*store.Stack{{ID: "s1", Name: "web", HostID: "h1"}}}
	h := &Handler{
		Store:    ms,
		Deployer: &mockDeployer{},
		Docker:   defaultDocker,
		DockerForHost: func(hostID string) (DockerClient, error) {
			if hostID == "h1" {
				return remoteDocker, nil
			}
			return nil, errors.New("unexpected host " + hostID)
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/stacks/web/containers", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "right-host", "must read the stack's host")
	assert.NotContains(t, body, "wrong-host", "must NOT read the default host")
}

func TestListStackContainers_DefaultHostWhenNoHostID(t *testing.T) {
	defaultDocker := &mockDocker{
		listResult: client.ContainerListResult{
			Items: []container.Summary{managedContainer("aaa", "web", "on-default")},
		},
	}
	ms := &mockStore{stacks: []*store.Stack{{ID: "s1", Name: "web"}}} // no HostID
	h := &Handler{
		Store: ms, Deployer: &mockDeployer{}, Docker: defaultDocker,
		DockerForHost: func(string) (DockerClient, error) {
			t.Fatal("must not resolve a host for a default-host stack")
			return nil, nil
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/stacks/web/containers", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "on-default")
}

// An unreachable host is an infrastructure failure, not a client error.
func TestListStackContainers_UnreachableHostIs502(t *testing.T) {
	ms := &mockStore{stacks: []*store.Stack{{ID: "s1", Name: "web", HostID: "h1"}}}
	h := &Handler{
		Store: ms, Deployer: &mockDeployer{}, Docker: &mockDocker{},
		DockerForHost: func(string) (DockerClient, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/stacks/web/containers", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadGateway, rr.Code)
	assert.Contains(t, rr.Body.String(), "docker host is unavailable")
}

// --- root browser fan-out ---------------------------------------------------

func twoHostHandler(def, remote *mockDocker) *Handler {
	return &Handler{
		Store: &mockStore{}, Deployer: &mockDeployer{}, Docker: def,
		DockerHosts: func() ([]HostClient, error) {
			return []HostClient{
				{ID: "", Name: DefaultHostName, Docker: def},
				{ID: "h1", Name: "edge-1", Docker: remote},
			}, nil
		},
	}
}

func TestListManagedVolumes_FansOutAcrossHosts(t *testing.T) {
	def := &mockDocker{volumeResult: client.VolumeListResult{
		Items: []volume.Volume{{Name: "vol-default", Driver: "local"}},
	}}
	remote := &mockDocker{volumeResult: client.VolumeListResult{
		Items: []volume.Volume{{Name: "vol-remote", Driver: "local"}},
	}}
	h := twoHostHandler(def, remote)

	req := httptest.NewRequest("GET", "/api/v1/volumes", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got []ManagedVolume
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	byName := map[string]string{}
	for _, v := range got {
		byName[v.Name] = v.Host
	}
	assert.Equal(t, DefaultHostName, byName["vol-default"])
	assert.Equal(t, "edge-1", byName["vol-remote"], "remote volumes are attributed to their host")
}

func TestListManagedNetworks_FansOutAcrossHosts(t *testing.T) {
	def := &mockDocker{networkResult: client.NetworkListResult{
		Items: []network.Summary{{Network: network.Network{ID: "n1", Name: "net-default", Driver: "bridge"}}},
	}}
	remote := &mockDocker{networkResult: client.NetworkListResult{
		Items: []network.Summary{{Network: network.Network{ID: "n2", Name: "net-remote", Driver: "bridge"}}},
	}}
	h := twoHostHandler(def, remote)

	req := httptest.NewRequest("GET", "/api/v1/networks", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got []ManagedNetwork
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 2)
	hosts := map[string]string{}
	for _, n := range got {
		hosts[n.Name] = n.Host
	}
	assert.Equal(t, DefaultHostName, hosts["net-default"])
	assert.Equal(t, "edge-1", hosts["net-remote"])
}

// One dead host must not blank the whole listing.
func TestListManagedVolumes_PartialHostFailureStillReturns(t *testing.T) {
	def := &mockDocker{volumeResult: client.VolumeListResult{
		Items: []volume.Volume{{Name: "vol-default", Driver: "local"}},
	}}
	broken := &mockDocker{volumeErr: errors.New("host down")}
	h := twoHostHandler(def, broken)

	req := httptest.NewRequest("GET", "/api/v1/volumes", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got []ManagedVolume
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "vol-default", got[0].Name)
}

// If every host fails there's nothing truthful to return.
func TestListManagedVolumes_AllHostsFailingIs500(t *testing.T) {
	def := &mockDocker{volumeErr: errors.New("down")}
	remote := &mockDocker{volumeErr: errors.New("down")}
	h := twoHostHandler(def, remote)

	req := httptest.NewRequest("GET", "/api/v1/volumes", nil)
	rr := httptest.NewRecorder()
	apikeyRouter(h).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Contains(t, rr.Body.String(), "any host")
}

// Without multi-host wiring, hostClients collapses to the default host so every
// browser behaves exactly as it did pre-multi-host.
func TestHostClients_DefaultOnlyWhenUnwired(t *testing.T) {
	def := &mockDocker{}
	h := &Handler{Store: &mockStore{}, Deployer: &mockDeployer{}, Docker: def}

	hosts, err := h.hostClients()
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	assert.Equal(t, "", hosts[0].ID)
	assert.Equal(t, DefaultHostName, hosts[0].Name)
	assert.Same(t, def, hosts[0].Docker)
}
