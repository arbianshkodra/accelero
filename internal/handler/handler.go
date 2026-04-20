package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/store"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/gorilla/mux"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	maxPayloadSize = 1 << 20 // 1 MB

	// readyzTimeout caps the Docker + DB checks so a wedged dependency
	// doesn't tarpit the probe endpoint. K8s probes are configured with
	// their own timeoutSeconds (usually 1–5s); 2s fits inside the usual
	// defaults.
	readyzTimeout = 2 * time.Second
)

// Deployer is the interface the stack deployer must satisfy.
type Deployer interface {
	Deploy(ctx context.Context, stack *store.Stack, trigger string) (*store.Deployment, error)
	// CleanupStackData removes the stack's cloned-repo workdir from disk.
	// Called by DeleteStack after the DB record is gone so orphan clones
	// don't accumulate forever.
	CleanupStackData(stackID string) error
}

// DockerClient is the subset of the moby client used by the handler's
// container-introspection endpoints. A narrow interface lets tests drop
// in a fake without wiring a real daemon.
type DockerClient interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerLogs(ctx context.Context, id string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerStats(ctx context.Context, id string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error)
}

// Handler holds all dependencies for the HTTP API.
type Handler struct {
	Store      store.Store
	Deployer   Deployer
	Reconciler *reconciler.Reconciler

	// Docker backs the read-only container introspection endpoints
	// (/stacks/{id}/containers, .../containers/{cid}, .../logs). Nil
	// disables those endpoints (they return 503) — useful in tests or
	// in minimal deploys that don't expose runtime introspection.
	Docker DockerClient

	// DockerPing is called by /readyz to verify Docker daemon connectivity.
	// nil disables the Docker check — useful in tests, or in the unlikely
	// deployment where Accelero proxies to another host and wouldn't want
	// its own readiness dependent on local Docker.
	DockerPing func(ctx context.Context) error

	// shuttingDown is flipped by SetShuttingDown before graceful shutdown
	// tears down the HTTP server.  /readyz reports 503 once set so load
	// balancers drain this pod before the server actually stops accepting
	// connections.
	shuttingDown atomic.Bool
}

// SetShuttingDown flips /readyz to draining mode. Safe to call from any
// goroutine — typically the signal handler in main.go immediately before
// server.Shutdown.
func (h *Handler) SetShuttingDown() {
	h.shuttingDown.Store(true)
}

// RegisterRoutes mounts all API endpoints onto the given router.
// The authMiddleware is applied to all endpoints that require authentication.
// The /health endpoint is registered without auth.
//
// RequestID middleware is applied to the base router so every handler,
// including /health, receives a request_id on its context and the
// response carries an X-Request-ID header.
func (h *Handler) RegisterRoutes(r *mux.Router, authMiddleware mux.MiddlewareFunc) {
	r.Use(middleware.RequestID)

	// Unauthenticated probes.
	//   /health  — original, kept for backward compatibility
	//   /healthz — Kubernetes-style liveness ("process responds at all")
	//   /readyz  — Kubernetes-style readiness (DB + Docker + not draining)
	r.HandleFunc("/health", h.Health).Methods("GET")
	r.HandleFunc("/healthz", h.Health).Methods("GET")
	r.HandleFunc("/readyz", h.Readyz).Methods("GET")

	// Authenticated API routes
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(authMiddleware)

	api.HandleFunc("/stacks", h.CreateStack).Methods("POST")
	api.HandleFunc("/stacks", h.ListStacks).Methods("GET")
	api.HandleFunc("/stacks/{id}", h.GetStack).Methods("GET")
	api.HandleFunc("/stacks/{id}", h.UpdateStack).Methods("PUT")
	api.HandleFunc("/stacks/{id}", h.DeleteStack).Methods("DELETE")

	api.HandleFunc("/stacks/{id}/deploy", h.DeployStack).Methods("POST")
	api.HandleFunc("/stacks/{id}/deployments", h.ListDeployments).Methods("GET")
	api.HandleFunc("/stacks/{id}/drift", h.CheckDrift).Methods("GET")
	api.HandleFunc("/stacks/{id}/preview", h.PreviewDeploy).Methods("POST")

	// Read-only container introspection (Phase 3).
	api.HandleFunc("/stacks/{id}/containers", h.ListStackContainers).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}", h.GetStackContainer).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/logs", h.GetStackContainerLogs).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/stats", h.GetStackContainerStats).Methods("GET")

	api.HandleFunc("/webhook", h.LegacyWebhook).Methods("POST")

	// Authenticated root-level routes
	auth := r.PathPrefix("").Subrouter()
	auth.Use(authMiddleware)
	auth.HandleFunc("/webhook", h.LegacyWebhook).Methods("POST")
	auth.HandleFunc("/status", h.Status).Methods("GET")
}

// --------------------------------------------------------------------------
// Stack CRUD
// --------------------------------------------------------------------------

func (h *Handler) CreateStack(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name              string `json:"name"`
		RepoURL           string `json:"repo_url"`
		RepoUsername      string `json:"repo_username"`
		RepoToken         string `json:"repo_token"`
		RepoBranch        string `json:"repo_branch"`
		ComposePath       string `json:"compose_path"`
		ServiceFilter     string `json:"service_filter"`
		AutoDeploy        bool   `json:"auto_deploy"`
		ReconcileInterval int    `json:"reconcile_interval_seconds"`
		DockerUsername    string `json:"docker_username"`
		DockerPassword    string `json:"docker_password"`
		DockerRegistry    string `json:"docker_registry"`
	}

	if err := readJSON(r, &input); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if input.Name == "" || input.RepoURL == "" || input.ComposePath == "" {
		writeError(w, "name, repo_url, and compose_path are required", http.StatusBadRequest)
		return
	}

	id, err := generateID()
	if err != nil {
		writeError(w, "failed to generate ID", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	stack := &store.Stack{
		ID:                id,
		Name:              input.Name,
		RepoURL:           input.RepoURL,
		RepoUsername:      input.RepoUsername,
		RepoToken:         input.RepoToken,
		RepoBranch:        input.RepoBranch,
		ComposePath:       input.ComposePath,
		ServiceFilter:     input.ServiceFilter,
		AutoDeploy:        input.AutoDeploy,
		ReconcileInterval: input.ReconcileInterval,
		Status:            store.StackStatusActive,
		DockerUsername:    input.DockerUsername,
		DockerPassword:    input.DockerPassword,
		DockerRegistry:    input.DockerRegistry,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := h.Store.CreateStack(stack); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, fmt.Sprintf("stack with name %q already exists", input.Name), http.StatusConflict)
			return
		}
		writeError(w, "failed to create stack", http.StatusInternalServerError)
		return
	}

	// Notify the reconciler so it can start a loop if needed.
	if h.Reconciler != nil {
		h.Reconciler.RefreshStack(stack.ID)
	}

	writeJSON(w, stack, http.StatusCreated)
}

func (h *Handler) ListStacks(w http.ResponseWriter, r *http.Request) {
	stacks, err := h.Store.ListStacks()
	if err != nil {
		writeError(w, "failed to list stacks", http.StatusInternalServerError)
		return
	}
	if stacks == nil {
		stacks = []*store.Stack{}
	}
	writeJSON(w, stacks, http.StatusOK)
}

func (h *Handler) GetStack(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil {
		writeError(w, "failed to get stack", http.StatusInternalServerError)
		return
	}
	if stack == nil {
		// Try by name as a convenience.
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}
	writeJSON(w, stack, http.StatusOK)
}

func (h *Handler) UpdateStack(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	var input struct {
		Name              *string `json:"name"`
		RepoURL           *string `json:"repo_url"`
		RepoUsername      *string `json:"repo_username"`
		RepoToken         *string `json:"repo_token"`
		RepoBranch        *string `json:"repo_branch"`
		ComposePath       *string `json:"compose_path"`
		ServiceFilter     *string `json:"service_filter"`
		AutoDeploy        *bool   `json:"auto_deploy"`
		ReconcileInterval *int    `json:"reconcile_interval_seconds"`
		Status            *string `json:"status"`
		DockerUsername    *string `json:"docker_username"`
		DockerPassword    *string `json:"docker_password"`
		DockerRegistry    *string `json:"docker_registry"`
	}

	if err := readJSON(r, &input); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if input.Name != nil {
		stack.Name = *input.Name
	}
	if input.RepoURL != nil {
		stack.RepoURL = *input.RepoURL
	}
	if input.RepoUsername != nil {
		stack.RepoUsername = *input.RepoUsername
	}
	if input.RepoToken != nil {
		stack.RepoToken = *input.RepoToken
	}
	if input.RepoBranch != nil {
		stack.RepoBranch = *input.RepoBranch
	}
	if input.ComposePath != nil {
		stack.ComposePath = *input.ComposePath
	}
	if input.ServiceFilter != nil {
		stack.ServiceFilter = *input.ServiceFilter
	}
	if input.AutoDeploy != nil {
		stack.AutoDeploy = *input.AutoDeploy
	}
	if input.ReconcileInterval != nil {
		stack.ReconcileInterval = *input.ReconcileInterval
	}
	if input.Status != nil {
		stack.Status = *input.Status
	}
	if input.DockerUsername != nil {
		stack.DockerUsername = *input.DockerUsername
	}
	if input.DockerPassword != nil {
		stack.DockerPassword = *input.DockerPassword
	}
	if input.DockerRegistry != nil {
		stack.DockerRegistry = *input.DockerRegistry
	}

	if err := h.Store.UpdateStack(stack); err != nil {
		writeError(w, "failed to update stack", http.StatusInternalServerError)
		return
	}

	if h.Reconciler != nil {
		h.Reconciler.RefreshStack(stack.ID)
	}

	writeJSON(w, stack, http.StatusOK)
}

func (h *Handler) DeleteStack(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	if err := h.Store.DeleteStack(id); err != nil {
		writeError(w, "failed to delete stack", http.StatusInternalServerError)
		return
	}

	if h.Reconciler != nil {
		h.Reconciler.RefreshStack(id)
	}

	// Remove the on-disk clone workdir.  Best-effort: a failure here is
	// logged but doesn't roll back the DB delete — the stack is already
	// gone logically, leftover files are a GC concern not a user concern.
	if h.Deployer != nil {
		if err := h.Deployer.CleanupStackData(stack.ID); err != nil {
			logctx.FromContext(r.Context()).WithError(err).
				Warnf("failed to remove stack data dir for %s", stack.ID)
		}
	}

	writeJSON(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// --------------------------------------------------------------------------
// Deployment actions
// --------------------------------------------------------------------------

func (h *Handler) DeployStack(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		// Also try by name.
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	if stack.Status == store.StackStatusDeploying {
		writeError(w, "deployment already in progress", http.StatusConflict)
		return
	}

	// Capture the request's logger so the background goroutine keeps the
	// request_id on every subsequent log line.
	reqLogger := logctx.FromContext(r.Context())

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ctx = logctx.WithLogger(ctx, reqLogger)
		if _, err := h.Deployer.Deploy(ctx, stack, store.TriggerManual); err != nil {
			logctx.FromContext(ctx).WithError(err).Errorf("Manual deployment failed for stack %s", stack.Name)
		}
	}()

	writeJSON(w, map[string]string{
		"status":   "accepted",
		"stack_id": stack.ID,
		"message":  "Deployment started. Check deployments for progress.",
	}, http.StatusAccepted)
}

func (h *Handler) ListDeployments(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	deployments, err := h.Store.ListDeployments(stack.ID, 50)
	if err != nil {
		writeError(w, "failed to list deployments", http.StatusInternalServerError)
		return
	}
	if deployments == nil {
		deployments = []*store.Deployment{}
	}
	writeJSON(w, deployments, http.StatusOK)
}

func (h *Handler) CheckDrift(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	if h.Reconciler == nil {
		writeError(w, "reconciler not available", http.StatusServiceUnavailable)
		return
	}

	report, err := h.Reconciler.CheckDrift(r.Context(), stack)
	if err != nil {
		writeError(w, fmt.Sprintf("drift check failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, report, http.StatusOK)
}

// PreviewAction describes what Accelero would do to a single service if
// deploy were triggered right now.
type PreviewAction struct {
	ServiceName string `json:"service_name"`
	Action      string `json:"action"` // create, recreate, restart, no_change
	Reason      string `json:"reason,omitempty"`
	Expected    string `json:"expected,omitempty"`
	Actual      string `json:"actual,omitempty"`
}

// PreviewReport is what POST /stacks/{id}/preview returns.  It is a thin
// layer over the reconciler's drift report: each drift item becomes an
// "action" the deploy would take, and clean services (no drift) are
// reported as no_change so operators can see the full plan.
type PreviewReport struct {
	StackID    string          `json:"stack_id"`
	StackName  string          `json:"stack_name"`
	CheckedAt  time.Time       `json:"checked_at"`
	HasChanges bool            `json:"has_changes"`
	Actions    []PreviewAction `json:"actions"`
}

// PreviewDeploy returns what the next deploy would change without actually
// touching the Docker host.  It uses the same clone + interpolate + parse
// pipeline as a real deploy, then maps drift items to per-service actions.
func (h *Handler) PreviewDeploy(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil || stack == nil {
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	if h.Reconciler == nil {
		writeError(w, "reconciler not available", http.StatusServiceUnavailable)
		return
	}

	driftReport, err := h.Reconciler.CheckDrift(r.Context(), stack)
	if err != nil {
		writeError(w, fmt.Sprintf("preview failed: %v", err), http.StatusInternalServerError)
		return
	}

	preview := &PreviewReport{
		StackID:    driftReport.StackID,
		StackName:  driftReport.StackName,
		CheckedAt:  driftReport.CheckedAt,
		HasChanges: driftReport.HasDrift,
		Actions:    make([]PreviewAction, 0, len(driftReport.Drifts)),
	}

	for _, d := range driftReport.Drifts {
		preview.Actions = append(preview.Actions, driftToAction(d))
	}

	writeJSON(w, preview, http.StatusOK)
}

// driftToAction maps a drift item to the deploy action it would trigger.
func driftToAction(d reconciler.DriftItem) PreviewAction {
	a := PreviewAction{
		ServiceName: d.ServiceName,
		Expected:    d.Expected,
		Actual:      d.Actual,
		Reason:      d.Message,
	}
	switch d.Type {
	case "missing":
		a.Action = "create"
	case "image_mismatch":
		a.Action = "recreate"
	case "stopped", "unhealthy":
		a.Action = "restart"
	case "extra":
		a.Action = "remove"
	case "missing_external":
		a.Action = "error" // external volume isn't Accelero's to create
	default:
		a.Action = "inspect" // unknown drift type — surface for human review
	}
	return a
}

// --------------------------------------------------------------------------
// Legacy webhook — triggers the "default" stack deploy
// --------------------------------------------------------------------------

func (h *Handler) LegacyWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		writeError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadSize)
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeError(w, "invalid or empty request body", http.StatusBadRequest)
		return
	}

	// Determine which stack to deploy. Check for a "stack" field in the payload.
	var payload struct {
		Stack string `json:"stack"`
	}
	_ = json.Unmarshal(body, &payload)

	var stack *store.Stack
	if payload.Stack != "" {
		stack, _ = h.Store.GetStackByName(payload.Stack)
		if stack == nil {
			stack, _ = h.Store.GetStack(payload.Stack)
		}
	}
	if stack == nil {
		// Fall back to the "default" stack.
		stack, _ = h.Store.GetStackByName("default")
	}
	if stack == nil {
		// Fall back to the first stack.
		stacks, _ := h.Store.ListStacks()
		if len(stacks) > 0 {
			stack = stacks[0]
		}
	}

	if stack == nil {
		writeError(w, "no stacks configured", http.StatusNotFound)
		return
	}

	reqLogger := logctx.FromContext(r.Context())

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ctx = logctx.WithLogger(ctx, reqLogger)
		if _, err := h.Deployer.Deploy(ctx, stack, store.TriggerWebhook); err != nil {
			logctx.FromContext(ctx).WithError(err).Errorf("Webhook deployment failed for stack %s", stack.Name)
		}
	}()

	writeJSON(w, map[string]string{
		"status":   "accepted",
		"stack_id": stack.ID,
		"message":  "Deployment queued.",
	}, http.StatusAccepted)
}

// --------------------------------------------------------------------------
// Health & Status
// --------------------------------------------------------------------------

// Health is a liveness probe: returns 200 whenever the HTTP server can
// accept a request. It intentionally does not touch dependencies — use
// /readyz for that. Same body is served at /healthz (K8s convention).
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
}

// Readyz is a readiness probe.  It returns 200 only when Accelero is
// actually ready to serve traffic: the database answers, the Docker
// daemon is reachable, and we aren't draining for shutdown.  Returns
// 503 with a per-check breakdown otherwise so an operator (or k8s)
// can see which dependency is the problem.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	checks := map[string]string{}
	ok := true

	if h.shuttingDown.Load() {
		checks["shutdown"] = "draining"
		ok = false
	} else {
		checks["shutdown"] = "ok"
	}

	if h.Store != nil {
		if err := h.Store.Ping(ctx); err != nil {
			checks["database"] = "unreachable: " + err.Error()
			ok = false
		} else {
			checks["database"] = "ok"
		}
	}

	if h.DockerPing != nil {
		if err := h.DockerPing(ctx); err != nil {
			checks["docker"] = "unreachable: " + err.Error()
			ok = false
		} else {
			checks["docker"] = "ok"
		}
	}

	status := http.StatusOK
	body := map[string]any{"status": "ok", "checks": checks}
	if !ok {
		status = http.StatusServiceUnavailable
		body["status"] = "not_ready"
	}
	writeJSON(w, body, status)
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	stacks, _ := h.Store.ListStacks()
	if stacks == nil {
		stacks = []*store.Stack{}
	}

	type stackSummary struct {
		ID             string     `json:"id"`
		Name           string     `json:"name"`
		Status         string     `json:"status"`
		LastDeployedAt *time.Time `json:"last_deployed_at,omitempty"`
		GitCommit      string     `json:"git_commit,omitempty"`
	}

	summaries := make([]stackSummary, len(stacks))
	for i, s := range stacks {
		summaries[i] = stackSummary{
			ID:             s.ID,
			Name:           s.Name,
			Status:         s.Status,
			LastDeployedAt: s.LastDeployedAt,
			GitCommit:      s.GitCommit,
		}
	}

	writeJSON(w, map[string]interface{}{
		"stacks": summaries,
		"total":  len(summaries),
	}, http.StatusOK)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func readJSON(r *http.Request, v interface{}) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxPayloadSize)
	return json.NewDecoder(r.Body).Decode(v)
}

// --------------------------------------------------------------------------
// Container introspection (Phase 3)
// --------------------------------------------------------------------------

const (
	containerLabelManagedBy    = "managed-by"
	containerLabelManagedValue = "accelero"
	containerLabelStackName    = "accelero-stack"
	containerLabelServiceName  = "accelero-service"
	containerLabelReplicaIndex = "accelero-replica"

	// defaultLogTail is the number of log lines returned when the caller
	// omits ?tail=. maxLogTail guards against accidentally streaming a
	// gigabyte of history in a single GET.
	defaultLogTail = 100
	maxLogTail     = 10000

	containerOpTimeout = 10 * time.Second
)

// ContainerSummary is the API response shape for the list endpoint.
// Intentionally narrower than Docker's Summary so the contract can evolve
// independently of moby internal types.
type ContainerSummary struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Service   string            `json:"service,omitempty"`
	Replica   *int              `json:"replica,omitempty"`
	State     string            `json:"state"`
	Status    string            `json:"status"`
	Health    string            `json:"health,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Ports     []ContainerPort   `json:"ports,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// ContainerPort is the normalised port-binding view served by both the
// list and detail endpoints.
type ContainerPort struct {
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	HostPort      uint16 `json:"host_port,omitempty"`
	HostIP        string `json:"host_ip,omitempty"`
}

// ContainerDetail extends Summary with inspect-level fields for a single
// container. Env is redacted by default — see redactEnv.
type ContainerDetail struct {
	ContainerSummary
	Cmd           []string                            `json:"cmd,omitempty"`
	Entrypoint    []string                            `json:"entrypoint,omitempty"`
	Env           []string                            `json:"env,omitempty"`
	WorkingDir    string                              `json:"working_dir,omitempty"`
	User          string                              `json:"user,omitempty"`
	RestartCount  int                                 `json:"restart_count"`
	RestartPolicy string                              `json:"restart_policy,omitempty"`
	StartedAt     string                              `json:"started_at,omitempty"`
	FinishedAt    string                              `json:"finished_at,omitempty"`
	ExitCode      int                                 `json:"exit_code"`
	Networks      map[string]ContainerNetworkEndpoint `json:"networks,omitempty"`
	Mounts        []ContainerMount                    `json:"mounts,omitempty"`
}

type ContainerNetworkEndpoint struct {
	NetworkID  string   `json:"network_id,omitempty"`
	IPAddress  string   `json:"ip_address,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	MACAddress string   `json:"mac_address,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

type ContainerMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode,omitempty"`
	Type        string `json:"type,omitempty"`
	RW          bool   `json:"rw"`
}

// ListStackContainers returns every container managed by Accelero for the
// given stack. Label filters ensure we never leak unmanaged containers or
// containers from a different stack even if the caller guesses names.
func (h *Handler) ListStackContainers(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), containerOpTimeout)
	defer cancel()

	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue)).
		Add("label", fmt.Sprintf("%s=%s", containerLabelStackName, stack.Name))

	res, err := h.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("container list failed")
		writeError(w, "failed to list containers", http.StatusInternalServerError)
		return
	}

	out := make([]ContainerSummary, 0, len(res.Items))
	for _, c := range res.Items {
		out = append(out, summaryFromDocker(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		ri, rj := -1, -1
		if out[i].Replica != nil {
			ri = *out[i].Replica
		}
		if out[j].Replica != nil {
			rj = *out[j].Replica
		}
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})

	writeJSON(w, out, http.StatusOK)
}

// GetStackContainer returns inspect-level detail for a single container.
// Verifies managed-by / stack labels before returning — guesses at another
// stack's container IDs get a 404, not a data leak.
func (h *Handler) GetStackContainer(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), containerOpTimeout)
	defer cancel()

	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(ctx, w, stack.Name, cid)
	if !ok {
		return
	}

	writeJSON(w, detailFromInspect(insp), http.StatusOK)
}

// GetStackContainerLogs streams the logs of a single container as
// text/plain. Multiplexed stdout/stderr from non-TTY containers is
// demuxed transparently before the body is written to the response.
//
// Query params:
//   - tail      (int)      default 100, capped at maxLogTail
//   - since     (Go duration, e.g. "5m")
//   - timestamps(bool)     include Docker's RFC3339Nano timestamps
func (h *Handler) GetStackContainerLogs(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(ctx, w, stack.Name, cid)
	if !ok {
		return
	}

	tailStr := strconv.Itoa(defaultLogTail)
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, "tail must be a non-negative integer", http.StatusBadRequest)
			return
		}
		if n > maxLogTail {
			n = maxLogTail
		}
		tailStr = strconv.Itoa(n)
	}

	var since string
	if raw := r.URL.Query().Get("since"); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, "since must be a Go duration (e.g. 5m, 30s)", http.StatusBadRequest)
			return
		}
		// Docker accepts an RFC3339 / Unix-seconds timestamp for Since;
		// computing from now() is more intuitive than the raw duration.
		since = strconv.FormatInt(time.Now().Add(-dur).Unix(), 10)
	}

	timestamps := r.URL.Query().Get("timestamps") == "true"

	stream, err := h.Docker.ContainerLogs(ctx, insp.Container.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailStr,
		Since:      since,
		Timestamps: timestamps,
		Follow:     false,
	})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("container logs failed")
		writeError(w, "failed to read container logs", http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// TTY containers produce a single raw stream; non-TTY containers use
	// Docker's framed multiplex which we demux into the response body.
	if insp.Container.Config != nil && insp.Container.Config.Tty {
		if _, err := io.Copy(w, stream); err != nil {
			logctx.FromContext(ctx).WithError(err).Warn("truncated tty log stream")
		}
		return
	}

	if _, err := stdcopy.StdCopy(w, w, stream); err != nil {
		logctx.FromContext(ctx).WithError(err).Warn("truncated demuxed log stream")
	}
}

// ContainerStatsSample is a projected view of Docker's raw StatsResponse
// with the derived percentages already computed. Returning a digested
// shape keeps callers from having to re-implement Docker's CPU-delta
// math in every client.
type ContainerStatsSample struct {
	ContainerID string                        `json:"container_id"`
	Name        string                        `json:"name"`
	ReadAt      time.Time                     `json:"read_at"`
	CPU         ContainerCPUStats             `json:"cpu"`
	Memory      ContainerMemoryStats          `json:"memory"`
	Networks    map[string]ContainerNetStats  `json:"networks,omitempty"`
	BlockIO     ContainerBlockIOStats         `json:"block_io"`
	PIDs        uint64                        `json:"pids"`
}

type ContainerCPUStats struct {
	Percent     float64 `json:"percent"`
	OnlineCPUs  uint32  `json:"online_cpus,omitempty"`
	TotalUsage  uint64  `json:"total_usage_ns"`
	SystemUsage uint64  `json:"system_usage_ns,omitempty"`
}

type ContainerMemoryStats struct {
	UsageBytes uint64  `json:"usage_bytes"`
	LimitBytes uint64  `json:"limit_bytes"`
	Percent    float64 `json:"percent"`
}

type ContainerNetStats struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

type ContainerBlockIOStats struct {
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// GetStackContainerStats returns a single computed stats sample. The
// endpoint deliberately does not stream — the use case is "what is this
// container doing right now?" from a polling dashboard. WebSocket-based
// streaming lives in a separate endpoint (same pattern as logs follow).
//
// The sample is taken with IncludePreviousSample=true so the daemon
// gathers two consecutive readings and we can compute a valid CPU
// percentage. The call therefore costs ~1s of daemon time, bounded by
// the request's context deadline.
func (h *Handler) GetStackContainerStats(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	// The daemon sleeps ~1s for the previous-sample gather; give the call
	// enough headroom for a slow host plus network.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(ctx, w, stack.Name, cid)
	if !ok {
		return
	}

	res, err := h.Docker.ContainerStats(ctx, insp.Container.ID, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("container stats failed")
		writeError(w, "failed to read container stats", http.StatusInternalServerError)
		return
	}
	defer res.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		logctx.FromContext(ctx).WithError(err).Error("decode container stats")
		writeError(w, "failed to decode container stats", http.StatusInternalServerError)
		return
	}

	writeJSON(w, computeStatsSample(raw, insp.Container.ID, insp.Container.Name), http.StatusOK)
}

// computeStatsSample digests a raw StatsResponse into our API shape.
// Kept pure (no ctx, no I/O) so the math is unit-testable.
func computeStatsSample(s container.StatsResponse, idFallback, nameFallback string) ContainerStatsSample {
	id := s.ID
	if id == "" {
		id = idFallback
	}
	name := strings.TrimPrefix(s.Name, "/")
	if name == "" {
		name = strings.TrimPrefix(nameFallback, "/")
	}

	out := ContainerStatsSample{
		ContainerID: id,
		Name:        name,
		ReadAt:      s.Read,
		CPU: ContainerCPUStats{
			Percent:     computeCPUPercent(s),
			OnlineCPUs:  s.CPUStats.OnlineCPUs,
			TotalUsage:  s.CPUStats.CPUUsage.TotalUsage,
			SystemUsage: s.CPUStats.SystemUsage,
		},
		Memory:  computeMemoryStats(s.MemoryStats),
		BlockIO: computeBlockIOStats(s.BlkioStats),
		PIDs:    s.PidsStats.Current,
	}

	if len(s.Networks) > 0 {
		out.Networks = make(map[string]ContainerNetStats, len(s.Networks))
		for iface, net := range s.Networks {
			out.Networks[iface] = ContainerNetStats{
				RxBytes: net.RxBytes,
				TxBytes: net.TxBytes,
			}
		}
	}
	return out
}

// computeCPUPercent mirrors the formula Docker's CLI `docker stats` uses:
//
//	(cpuDelta / systemDelta) * onlineCPUs * 100
//
// A missing previous sample (both reads are effectively the same moment)
// yields 0%, which is accurate for "we didn't sample long enough to know".
func computeCPUPercent(s container.StatsResponse) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || systemDelta <= 0 {
		return 0
	}
	onlineCPUs := float64(s.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	return (cpuDelta / systemDelta) * onlineCPUs * 100
}

// computeMemoryStats subtracts kernel page cache from the raw usage
// figure before reporting. Docker's CLI does the same to give operators
// the number that actually matters for OOM risk — page cache is
// reclaimable memory the kernel hands back under pressure, so including
// it in "usage" is misleading in most monitoring contexts.
//
// cgroup v1 reports cache under key "cache"; cgroup v2 reports it under
// "file". We subtract whichever is present (they're mutually exclusive).
func computeMemoryStats(m container.MemoryStats) ContainerMemoryStats {
	usage := m.Usage
	if cache, ok := m.Stats["cache"]; ok && usage > cache {
		usage -= cache
	} else if file, ok := m.Stats["file"]; ok && usage > file {
		usage -= file
	}

	out := ContainerMemoryStats{
		UsageBytes: usage,
		LimitBytes: m.Limit,
	}
	if m.Limit > 0 {
		out.Percent = float64(usage) / float64(m.Limit) * 100
	}
	return out
}

// computeBlockIOStats sums the per-device read/write byte counters.
// Docker reports them as repeated rows per (Major,Minor,Op) tuple; we
// collapse to a single pair to avoid exposing arbitrary device numbers.
func computeBlockIOStats(b container.BlkioStats) ContainerBlockIOStats {
	var out ContainerBlockIOStats
	for _, e := range b.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			out.ReadBytes += e.Value
		case "write":
			out.WriteBytes += e.Value
		}
	}
	return out
}

// resolveStack is shared between all the /stacks/{id}/* endpoints. It
// writes the response on error and returns (nil, false).
func (h *Handler) resolveStack(w http.ResponseWriter, r *http.Request) (*store.Stack, bool) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil {
		writeError(w, "failed to get stack", http.StatusInternalServerError)
		return nil, false
	}
	if stack == nil {
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return nil, false
	}
	return stack, true
}

// resolveStackContainer inspects a container and verifies it is managed by
// Accelero and belongs to the given stack. Any verification failure maps to
// a 404 so callers probing foreign IDs cannot distinguish "doesn't exist"
// from "exists but isn't yours". Writes the error on failure.
func (h *Handler) resolveStackContainer(
	ctx context.Context,
	w http.ResponseWriter,
	stackName, containerID string,
) (client.ContainerInspectResult, bool) {
	var empty client.ContainerInspectResult
	insp, err := h.Docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			writeError(w, "container not found", http.StatusNotFound)
			return empty, false
		}
		logctx.FromContext(ctx).WithError(err).Error("container inspect failed")
		writeError(w, "failed to inspect container", http.StatusInternalServerError)
		return empty, false
	}
	if !containerBelongsToStack(insp.Container, stackName) {
		writeError(w, "container not found", http.StatusNotFound)
		return empty, false
	}
	return insp, true
}

// containerBelongsToStack checks the management labels on a container.
// Any missing or mismatched label means the container is not Accelero's
// (or belongs to a different stack) — the caller should present a 404.
func containerBelongsToStack(c container.InspectResponse, stackName string) bool {
	if c.Config == nil {
		return false
	}
	if c.Config.Labels[containerLabelManagedBy] != containerLabelManagedValue {
		return false
	}
	if c.Config.Labels[containerLabelStackName] != stackName {
		return false
	}
	return true
}

// summaryFromDocker projects Docker's Summary onto our API shape.
func summaryFromDocker(c container.Summary) ContainerSummary {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	replica := parseReplicaLabel(c.Labels[containerLabelReplicaIndex])

	summary := ContainerSummary{
		ID:        c.ID,
		Name:      name,
		Image:     c.Image,
		Service:   c.Labels[containerLabelServiceName],
		Replica:   replica,
		State:     string(c.State),
		Status:    c.Status,
		CreatedAt: time.Unix(c.Created, 0).UTC(),
		Labels:    c.Labels,
	}
	if c.Health != nil {
		summary.Health = string(c.Health.Status)
	}
	for _, p := range c.Ports {
		cp := ContainerPort{
			ContainerPort: p.PrivatePort,
			Protocol:      p.Type,
			HostPort:      p.PublicPort,
		}
		if p.IP.IsValid() {
			cp.HostIP = p.IP.String()
		}
		summary.Ports = append(summary.Ports, cp)
	}
	return summary
}

// detailFromInspect builds the richer detail view from an inspect response.
// Env is redacted — see redactEnv for the policy.
func detailFromInspect(r client.ContainerInspectResult) ContainerDetail {
	c := r.Container
	name := strings.TrimPrefix(c.Name, "/")
	replica := -1
	if c.Config != nil {
		if idx := parseReplicaLabel(c.Config.Labels[containerLabelReplicaIndex]); idx != nil {
			replica = *idx
		}
	}

	detail := ContainerDetail{
		ContainerSummary: ContainerSummary{
			ID:     c.ID,
			Name:   name,
			Image:  c.Image,
			Labels: labelsFromConfig(c.Config),
		},
	}
	if replica >= 0 {
		r := replica
		detail.Replica = &r
	}
	if created, err := time.Parse(time.RFC3339Nano, c.Created); err == nil {
		detail.CreatedAt = created.UTC()
	}
	if c.State != nil {
		detail.State = string(c.State.Status)
		detail.Status = fmt.Sprintf("state=%s", c.State.Status)
		if c.State.Health != nil {
			detail.Health = string(c.State.Health.Status)
		}
		detail.StartedAt = c.State.StartedAt
		detail.FinishedAt = c.State.FinishedAt
		detail.ExitCode = c.State.ExitCode
	}
	detail.RestartCount = c.RestartCount
	if c.HostConfig != nil && c.HostConfig.RestartPolicy.Name != "" {
		detail.RestartPolicy = string(c.HostConfig.RestartPolicy.Name)
	}
	if c.Config != nil {
		detail.Cmd = []string(c.Config.Cmd)
		detail.Entrypoint = []string(c.Config.Entrypoint)
		detail.Env = redactEnv(c.Config.Env)
		detail.WorkingDir = c.Config.WorkingDir
		detail.User = c.Config.User
		detail.Service = c.Config.Labels[containerLabelServiceName]
	}

	if c.NetworkSettings != nil {
		detail.Networks = make(map[string]ContainerNetworkEndpoint, len(c.NetworkSettings.Networks))
		for netName, ep := range c.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			endpoint := ContainerNetworkEndpoint{
				NetworkID: ep.NetworkID,
				Aliases:   ep.Aliases,
			}
			if ep.IPAddress.IsValid() {
				endpoint.IPAddress = ep.IPAddress.String()
			}
			if ep.Gateway.IsValid() {
				endpoint.Gateway = ep.Gateway.String()
			}
			if len(ep.MacAddress) > 0 {
				endpoint.MACAddress = ep.MacAddress.String()
			}
			detail.Networks[netName] = endpoint
		}
	}

	for _, m := range c.Mounts {
		detail.Mounts = append(detail.Mounts, ContainerMount{
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			Type:        string(m.Type),
			RW:          m.RW,
		})
	}

	return detail
}

func labelsFromConfig(cfg *container.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	return cfg.Labels
}

// parseReplicaLabel returns the replica index, or nil when the label is
// missing or malformed (legacy containers pre-dating deploy.replicas).
func parseReplicaLabel(raw string) *int {
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &n
}

// redactEnv masks values whose key looks like a secret. The goal isn't
// perfect — anything in a container's env is technically visible to anyone
// who can exec into the host — but surfacing `API_TOKEN=...` in a JSON
// response is strictly worse than hiding it and letting the operator drop
// down to `docker inspect` when they really need the value.
func redactEnv(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			out = append(out, entry)
			continue
		}
		key := entry[:idx]
		if isSecretKey(key) {
			out = append(out, key+"=***")
			continue
		}
		out = append(out, entry)
	}
	return out
}

// isSecretKey recognises the conventional "this is a secret" substrings.
// Case-insensitive match on the *key*, not the value.
func isSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, needle := range []string{"PASSWORD", "TOKEN", "SECRET", "APIKEY", "API_KEY", "PRIVATE", "CREDENTIAL"} {
		if strings.Contains(upper, needle) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, data interface{}, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logrus.WithError(err).Error("Failed to write JSON response")
	}
}

func writeError(w http.ResponseWriter, msg string, status int) {
	writeJSON(w, map[string]string{"error": msg}, status)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
