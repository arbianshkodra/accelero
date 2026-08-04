package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/dockerhost"
	"github.com/arbianshkodra/accelero/internal/store"
	volumepkg "github.com/arbianshkodra/accelero/internal/volume"
	"github.com/gorilla/mux"
)

// hostResponse is the client-facing shape of a Docker host. TLS material is
// never returned — only whether TLS is configured.
type hostResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Endpoint   string    `json:"endpoint"`
	TLSEnabled bool      `json:"tls_enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func toHostResponse(h *store.DockerHost) hostResponse {
	return hostResponse{
		ID: h.ID, Name: h.Name, Endpoint: h.Endpoint,
		TLSEnabled: h.TLSEnabled(), CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
	}
}

// CreateDockerHost registers a Docker host that stacks can target. The endpoint
// is pinged before the host is stored, so an operator finds out immediately
// rather than at first deploy. Admin-only (route policy). Audited as host.create.
func (h *Handler) CreateDockerHost(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		TLSCA    string `json:"tls_ca"`
		TLSCert  string `json:"tls_cert"`
		TLSKey   string `json:"tls_key"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Endpoint = strings.TrimSpace(input.Endpoint)
	if input.Name == "" || input.Endpoint == "" {
		writeError(w, "name and endpoint are required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(input.Endpoint, "unix://") && !strings.HasPrefix(input.Endpoint, "tcp://") {
		writeError(w, "endpoint must start with unix:// or tcp://", http.StatusBadRequest)
		return
	}
	// All three TLS fields go together — a partial triple is a misconfiguration
	// that would otherwise surface as a confusing handshake failure later.
	tlsCount := 0
	for _, v := range []string{input.TLSCA, input.TLSCert, input.TLSKey} {
		if strings.TrimSpace(v) != "" {
			tlsCount++
		}
	}
	if tlsCount != 0 && tlsCount != 3 {
		writeError(w, "tls_ca, tls_cert and tls_key must be provided together", http.StatusBadRequest)
		return
	}

	if existing, _ := h.Store.GetDockerHostByName(input.Name); existing != nil {
		writeError(w, "a host with that name already exists", http.StatusConflict)
		return
	}

	// Verify reachability before persisting.
	probe := &dockerhost.Host{
		Name: input.Name, Endpoint: input.Endpoint,
		TLSCA: input.TLSCA, TLSCert: input.TLSCert, TLSKey: input.TLSKey,
	}
	if err := dockerhost.Ping(r.Context(), probe); err != nil {
		entry := audit.FromRequest(r, store.AuditOpHostCreate)
		entry.ResourceType = "host"
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = err.Error()
		entry.Metadata = map[string]string{"name": input.Name, "endpoint": input.Endpoint}
		_ = h.auditOr().Record(r.Context(), entry)

		writeError(w, "host is not reachable: "+err.Error(), http.StatusBadRequest)
		return
	}

	id, err := generateID()
	if err != nil {
		writeError(w, "failed to generate ID", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	host := &store.DockerHost{
		ID: id, Name: input.Name, Endpoint: input.Endpoint,
		TLSCA: input.TLSCA, TLSCert: input.TLSCert, TLSKey: input.TLSKey,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := h.Store.CreateDockerHost(host); err != nil {
		writeError(w, "failed to create host", http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpHostCreate)
	entry.ResourceType = "host"
	entry.ResourceID = id
	entry.Metadata = map[string]string{
		"name": input.Name, "endpoint": input.Endpoint,
		"tls": fmt.Sprintf("%t", host.TLSEnabled()),
	}
	_ = h.auditOr().Record(r.Context(), entry)

	writeJSON(w, toHostResponse(host), http.StatusCreated)
}

// HostClient pairs a Docker host with its client, for endpoints that span
// every host (the root resource browsers). The default host has an empty ID and
// the name "default".
type HostClient struct {
	ID     string
	Name   string
	Docker DockerClient
}

// DefaultHostName labels results that came from the DOCKER_SOCK daemon.
const DefaultHostName = "default"

// dockerForStack returns the Docker client for the stack's host. A stack on the
// default host (or a server without multi-host wiring) gets h.Docker. An
// unreachable/unknown host is a 502 — the request can't be served, and that's
// an infrastructure failure rather than a client error.
func (h *Handler) dockerForStack(w http.ResponseWriter, stack *store.Stack) (DockerClient, bool) {
	if stack.HostID == "" || h.DockerForHost == nil {
		return h.Docker, true
	}
	dkr, err := h.DockerForHost(stack.HostID)
	if err != nil {
		writeError(w, "stack's docker host is unavailable: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	return dkr, true
}

// hostClients returns every host to fan a root-level browse across: the default
// host first, then each registered host. Without multi-host wiring this is just
// the default host, so callers need no special case.
func (h *Handler) hostClients() ([]HostClient, error) {
	if h.DockerHosts == nil {
		return []HostClient{{ID: "", Name: DefaultHostName, Docker: h.Docker}}, nil
	}
	return h.DockerHosts()
}

// volumeHost bundles everything the volume endpoints need for one host: the
// Docker client (to verify the volume is accelero-managed *there*) and the
// Browser (whose helper container runs on that host).
type volumeHost struct {
	ID     string
	Name   string
	Docker DockerClient
	Browse volumepkg.Browser
}

// resolveVolumeHost picks the host a volume request targets from the optional
// `?host=` query parameter (a host name or id; omitted means the default host).
// It's the mirror of the `host` field returned by GET /volumes, so a client can
// feed a listing result straight back in.
//
// Explicit rather than inferred: volume names are only unique per host, so
// guessing which daemon the caller meant could operate on the wrong data.
func (h *Handler) resolveVolumeHost(w http.ResponseWriter, r *http.Request) (volumeHost, bool) {
	token := strings.TrimSpace(r.URL.Query().Get("host"))

	// Default host — either not specified, or explicitly named "default".
	if token == "" || token == DefaultHostName {
		return volumeHost{Name: DefaultHostName, Docker: h.Docker, Browse: h.VolumeBrowser}, true
	}

	if h.VolumeBrowserForHost == nil || h.DockerForHost == nil {
		writeError(w, "multi-host volume access is not configured on this server", http.StatusServiceUnavailable)
		return volumeHost{}, false
	}

	host, err := h.Store.GetDockerHost(token)
	if err != nil || host == nil {
		host, _ = h.Store.GetDockerHostByName(token)
	}
	if host == nil {
		writeError(w, "unknown host: "+token, http.StatusBadRequest)
		return volumeHost{}, false
	}

	dkr, err := h.DockerForHost(host.ID)
	if err != nil {
		writeError(w, "docker host is unavailable: "+err.Error(), http.StatusBadGateway)
		return volumeHost{}, false
	}
	browser, err := h.VolumeBrowserForHost(host.ID)
	if err != nil {
		writeError(w, "docker host is unavailable: "+err.Error(), http.StatusBadGateway)
		return volumeHost{}, false
	}
	return volumeHost{ID: host.ID, Name: host.Name, Docker: dkr, Browse: browser}, true
}

// resolveHostID maps a caller-supplied host token (id or name) to a canonical
// host ID. An empty token means the default host and resolves to "". Writes a
// 400 and returns ok=false for an unknown host.
func (h *Handler) resolveHostID(w http.ResponseWriter, token string) (string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", true
	}
	if host, err := h.Store.GetDockerHost(token); err == nil && host != nil {
		return host.ID, true
	}
	if host, _ := h.Store.GetDockerHostByName(token); host != nil {
		return host.ID, true
	}
	writeError(w, "unknown host: "+token, http.StatusBadRequest)
	return "", false
}

// ListDockerHosts returns registered hosts (TLS material redacted). Admin-only.
func (h *Handler) ListDockerHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := h.Store.ListDockerHosts()
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]hostResponse, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, toHostResponse(host))
	}
	writeJSON(w, out, http.StatusOK)
}

// DeleteDockerHost removes a host. Refuses (409) while any stack still targets
// it — silently orphaning those stacks onto the default host would be worse.
// Admin-only. Audited as host.delete in both outcomes.
func (h *Handler) DeleteDockerHost(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	inUse, err := h.Store.CountStacksOnHost(id)
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if inUse > 0 {
		writeError(w, fmt.Sprintf("host still targeted by %d stack(s); move them first", inUse), http.StatusConflict)
		return
	}

	deleted, err := h.Store.DeleteDockerHost(id)
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpHostDelete)
	entry.ResourceType = "host"
	entry.ResourceID = id
	if !deleted {
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = "not found"
	}
	_ = h.auditOr().Record(r.Context(), entry)

	if !deleted {
		writeError(w, "host not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
