package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/store"
	volumepkg "github.com/arbianshkodra/accelero/internal/volume"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
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
	ContainerRestart(ctx context.Context, id string, options client.ContainerRestartOptions) (client.ContainerRestartResult, error)
	Events(ctx context.Context, options client.EventsListOptions) client.EventsResult
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	ExecResize(ctx context.Context, execID string, options client.ExecResizeOptions) (client.ExecResizeResult, error)
}

// Handler holds all dependencies for the HTTP API.
type Handler struct {
	Store      store.Store
	Deployer   Deployer
	Reconciler *reconciler.Reconciler

	// Audit records notable write actions to the audit log. Defaults to
	// a NoopRecorder when nil so existing tests that don't wire audit
	// keep passing. Production wires audit.NewStoreRecorder(db).
	Audit audit.Recorder

	// Docker backs the read-only container introspection endpoints
	// (/stacks/{id}/containers, .../containers/{cid}, .../logs). Nil
	// disables those endpoints (they return 503) — useful in tests or
	// in minimal deploys that don't expose runtime introspection.
	Docker DockerClient

	// VolumeBrowser backs GET /volumes/{name}/browse. Separated from
	// DockerClient because the helper-container lifecycle is a bigger
	// unit of behaviour than a single Docker API call.  Nil disables
	// the browse endpoint (503).
	VolumeBrowser volumepkg.Browser

	// AllowVolumeWrites gates POST /volumes/{name}/files. Defaults to
	// false; an operator opts in via ALLOW_VOLUME_WRITES=true only on
	// hosts where the trade-off (emergency-write capability vs. one
	// well-placed bug wiping volume data) is acceptable.
	AllowVolumeWrites bool

	// EncryptionEnabled reports whether at-rest encryption is active
	// (cipher wired in cmd/main.go). The admin migration endpoint
	// requires this to be true — there's no point re-saving rows
	// through a pass-through cipher.
	EncryptionEnabled bool

	// WebhookAuth, when non-nil, replaces the default API-key auth on
	// the root-level POST /webhook route. Main wires this to the
	// HMAC-SHA256 signature middleware when WEBHOOK_SECRET is set, so
	// external senders (GitHub, CI) can sign payloads instead of
	// smuggling the API key into their webhook config. /api/v1/webhook
	// always requires the API key regardless.
	WebhookAuth mux.MiddlewareFunc

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

// auditOr returns the handler's configured Audit recorder or a
// NoopRecorder if none was wired. Call sites can always dereference
// the result without a nil check.
func (h *Handler) auditOr() audit.Recorder {
	if h.Audit == nil {
		return audit.NoopRecorder{}
	}
	return h.Audit
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

	// Per-stack secrets — encrypted at rest via the existing cipher.
	// List returns names/timestamps only; values never leave via the
	// API (they're injected into containers at deploy time, follow-up PR).
	api.HandleFunc("/stacks/{id}/secrets", h.SetStackSecret).Methods("POST")
	api.HandleFunc("/stacks/{id}/secrets", h.ListStackSecrets).Methods("GET")
	api.HandleFunc("/stacks/{id}/secrets/{name}", h.DeleteStackSecret).Methods("DELETE")

	// Read-only container introspection (Phase 3).
	api.HandleFunc("/stacks/{id}/containers", h.ListStackContainers).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}", h.GetStackContainer).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/logs", h.GetStackContainerLogs).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/logs/stream", h.StreamStackContainerLogs).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/stats", h.GetStackContainerStats).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/stats/stream", h.StreamStackContainerStats).Methods("GET")
	api.HandleFunc("/stacks/{id}/containers/{cid}/restart", h.RestartStackContainer).Methods("POST")
	api.HandleFunc("/stacks/{id}/containers/{cid}/exec", h.ExecStackContainer).Methods("GET")
	api.HandleFunc("/stacks/{id}/events", h.StreamStackEvents).Methods("GET")

	// Read-only resource browsers (Phase 3) — root-level; scoped to
	// accelero-managed resources. ?stack=<name> narrows further.
	api.HandleFunc("/images", h.ListManagedImages).Methods("GET")
	api.HandleFunc("/volumes", h.ListManagedVolumes).Methods("GET")
	api.HandleFunc("/volumes/{name}/browse", h.BrowseVolume).Methods("GET")
	api.HandleFunc("/volumes/{name}/files", h.WriteVolumeFile).Methods("POST")
	api.HandleFunc("/networks", h.ListManagedNetworks).Methods("GET")

	// Audit log — read-only; append-only at the store layer.
	api.HandleFunc("/audit", h.ListAuditEntries).Methods("GET")

	// Admin: one-shot migration that re-encrypts any plaintext
	// secrets in place. No-op when encryption is disabled.
	api.HandleFunc("/admin/encrypt-existing", h.EncryptExistingStacks).Methods("POST")

	api.HandleFunc("/webhook", h.LegacyWebhook).Methods("POST")

	// /webhook has its own auth chain. If WebhookAuth is non-nil
	// (WEBHOOK_SECRET is set), HMAC-SHA256 verification replaces the
	// API key check — external senders like GitHub won't know the
	// internal API key, but they can sign payloads with a shared
	// secret. Absent the override we fall back to the old API-key
	// behaviour so existing deployments keep working.
	webhookAuth := authMiddleware
	if h.WebhookAuth != nil {
		webhookAuth = h.WebhookAuth
	}
	webhookRouter := r.PathPrefix("").Subrouter()
	webhookRouter.Use(webhookAuth)
	webhookRouter.HandleFunc("/webhook", h.LegacyWebhook).Methods("POST")

	// /status stays behind API key auth — it's an operator surface,
	// not a webhook target.
	statusRouter := r.PathPrefix("").Subrouter()
	statusRouter.Use(authMiddleware)
	statusRouter.HandleFunc("/status", h.Status).Methods("GET")
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

	entry := audit.FromRequest(r, store.AuditOpStackCreate)
	entry.ResourceType = "stack"
	entry.ResourceID = stack.ID
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	_ = h.auditOr().Record(r.Context(), entry)

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
	if err != nil {
		writeError(w, "failed to get stack", http.StatusInternalServerError)
		return
	}
	if stack == nil {
		// Id-or-name fallback, matching the other stack-CRUD endpoints.
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
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

	entry := audit.FromRequest(r, store.AuditOpStackUpdate)
	entry.ResourceType = "stack"
	entry.ResourceID = stack.ID
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	_ = h.auditOr().Record(r.Context(), entry)

	writeJSON(w, stack, http.StatusOK)
}

func (h *Handler) DeleteStack(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	stack, err := h.Store.GetStack(id)
	if err != nil {
		writeError(w, "failed to get stack", http.StatusInternalServerError)
		return
	}
	if stack == nil {
		// Try by name — matches the other stack CRUD endpoints'
		// id-or-name convention. Resolve the canonical ID here so the
		// downstream delete / cleanup references it directly rather
		// than re-doing the name lookup.
		stack, _ = h.Store.GetStackByName(id)
	}
	if stack == nil {
		writeError(w, "stack not found", http.StatusNotFound)
		return
	}

	if err := h.Store.DeleteStack(stack.ID); err != nil {
		writeError(w, "failed to delete stack", http.StatusInternalServerError)
		return
	}

	if h.Reconciler != nil {
		h.Reconciler.RefreshStack(stack.ID)
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

	entry := audit.FromRequest(r, store.AuditOpStackDelete)
	entry.ResourceType = "stack"
	entry.ResourceID = stack.ID
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	_ = h.auditOr().Record(r.Context(), entry)

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

	// Record the "deploy requested" intent synchronously before the
	// background goroutine runs. The deployer itself records the
	// completion/failure outcome once it knows which one applies.
	startEntry := audit.FromRequest(r, store.AuditOpDeployStart)
	startEntry.ResourceType = "stack"
	startEntry.ResourceID = stack.ID
	startEntry.StackID = stack.ID
	startEntry.StackName = stack.Name
	startEntry.Outcome = store.AuditOutcomeInProgress
	startEntry.Metadata = map[string]string{"trigger": store.TriggerManual}
	_ = h.auditOr().Record(r.Context(), startEntry)

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
	case "secrets_changed":
		// Docker can't update env on a running container; the next
		// deploy will recreate every replica with the new secret set.
		a.Action = "recreate"
	default:
		a.Action = "inspect" // unknown drift type — surface for human review
	}
	return a
}

// --------------------------------------------------------------------------
// Audit log endpoint
// --------------------------------------------------------------------------

// EncryptExistingStacks is a one-shot migration that re-saves any
// stack whose sensitive fields are still stored as pre-encryption
// plaintext. The re-save goes through UpdateStack, which always
// encrypts on write when a cipher is attached — no special code
// path needed here.
//
// Requires ACCELERO_ENCRYPTION_KEY to be set; without it the re-save
// would be a no-op and the endpoint would lie about what it did.
// Idempotent: running again after everything is encrypted finds zero
// plaintext rows.
//
// Audited as admin.encrypt-existing with the migrated count in metadata.
func (h *Handler) EncryptExistingStacks(w http.ResponseWriter, r *http.Request) {
	if !h.EncryptionEnabled {
		writeError(w, "encryption is disabled on this server (set ACCELERO_ENCRYPTION_KEY to enable)", http.StatusBadRequest)
		return
	}

	ids, err := h.Store.ListStacksNeedingEncryption()
	if err != nil {
		writeError(w, "failed to list stacks needing migration", http.StatusInternalServerError)
		return
	}

	migrated := 0
	var failed []string
	for _, id := range ids {
		stack, err := h.Store.GetStack(id)
		if err != nil || stack == nil {
			failed = append(failed, id)
			continue
		}
		// UpdateStack re-encrypts via the cipher on write. That's the
		// whole migration — no special logic here.
		if err := h.Store.UpdateStack(stack); err != nil {
			logctx.FromContext(r.Context()).WithError(err).
				Warnf("admin migrate: re-save of stack %s failed", id)
			failed = append(failed, id)
			continue
		}
		migrated++
	}

	entry := audit.FromRequest(r, store.AuditOpAdminEncrypt)
	entry.ResourceType = "admin"
	entry.Metadata = map[string]string{
		"stacks_migrated": strconv.Itoa(migrated),
		"stacks_failed":   strconv.Itoa(len(failed)),
	}
	if len(failed) > 0 {
		entry.Outcome = store.AuditOutcomeFailure
	}
	_ = h.auditOr().Record(r.Context(), entry)

	body := map[string]interface{}{
		"status":          "ok",
		"stacks_migrated": migrated,
	}
	if len(failed) > 0 {
		body["stacks_failed"] = failed
	}
	writeJSON(w, body, http.StatusOK)
}

// --------------------------------------------------------------------------
// Per-stack secrets
// --------------------------------------------------------------------------

// secretNameRe matches legal POSIX-style environment-variable names.
// Secrets get materialised into container env vars at deploy time, so
// we enforce the constraint at the store boundary rather than at the
// deploy path — fail fast, and make the API predictable.
var secretNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

const (
	maxSecretNameLen  = 128
	maxSecretValueLen = 64 * 1024 // 64 KiB — comfortably larger than a PEM
)

// SetStackSecret accepts {"name": "...", "value": "..."} and upserts a
// per-stack secret. 201 on first write of a key, 200 on rewrite, 400 on
// a name that doesn't match [A-Z_][A-Z0-9_]* or exceeds the length cap.
// The value is encrypted at rest when a cipher is attached; the audit
// entry records the name but never the value.
func (h *Handler) SetStackSecret(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}

	var input struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if input.Name == "" {
		writeError(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(input.Name) > maxSecretNameLen {
		writeError(w, fmt.Sprintf("name exceeds %d characters", maxSecretNameLen), http.StatusBadRequest)
		return
	}
	if !secretNameRe.MatchString(input.Name) {
		writeError(w, "name must match [A-Z_][A-Z0-9_]* (uppercase letters, digits, underscores; no leading digit)", http.StatusBadRequest)
		return
	}
	if input.Value == "" {
		// Storing "" is ambiguous — the operator probably meant to
		// delete. Route them to DELETE explicitly instead of
		// silently dropping the row.
		writeError(w, "value must not be empty; use DELETE to remove a secret", http.StatusBadRequest)
		return
	}
	if len(input.Value) > maxSecretValueLen {
		writeError(w, fmt.Sprintf("value exceeds %d bytes", maxSecretValueLen), http.StatusRequestEntityTooLarge)
		return
	}

	// Detect first-write vs rewrite so the response code is informative
	// (201 vs 200). Cheap: we already have the stack ID and the store
	// query is indexed on (stack_id, name).
	existing, err := h.Store.ListStackSecrets(stack.ID)
	if err != nil {
		writeError(w, "failed to read stack secrets", http.StatusInternalServerError)
		return
	}
	wasPresent := false
	for _, s := range existing {
		if s.Name == input.Name {
			wasPresent = true
			break
		}
	}

	if err := h.Store.UpsertStackSecret(&store.StackSecret{
		StackID: stack.ID,
		Name:    input.Name,
		Value:   input.Value,
	}); err != nil {
		writeError(w, "failed to store stack secret", http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpStackSecretSet)
	entry.ResourceType = "stack_secret"
	entry.ResourceID = input.Name
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	entry.Metadata = map[string]string{"rewrote_existing": strconv.FormatBool(wasPresent)}
	_ = h.auditOr().Record(r.Context(), entry)

	status := http.StatusCreated
	if wasPresent {
		status = http.StatusOK
	}
	writeJSON(w, map[string]interface{}{
		"name":    input.Name,
		"created": !wasPresent,
	}, status)
}

// ListStackSecrets returns every secret for a stack with name + timestamps.
// Values are DELIBERATELY omitted — they leave the process only through
// the deploy path. Ordering matches the store (ASC by name) so clients
// can paginate/diff without extra sort logic.
func (h *Handler) ListStackSecrets(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	secs, err := h.Store.ListStackSecrets(stack.ID)
	if err != nil {
		writeError(w, "failed to list stack secrets", http.StatusInternalServerError)
		return
	}

	// Map to a response shape that excludes the value. Doing this at
	// the handler boundary — rather than trusting the `json:"-"` tag
	// on StackSecret.Value alone — means the guarantee survives a
	// future JSON-encoding refactor that re-exports the field.
	type item struct {
		Name      string    `json:"name"`
		StackID   string    `json:"stack_id"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	out := make([]item, 0, len(secs))
	for _, s := range secs {
		out = append(out, item{
			Name:      s.Name,
			StackID:   s.StackID,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
		})
	}
	writeJSON(w, out, http.StatusOK)
}

// DeleteStackSecret removes a single secret. 204 on success, 404 when
// the named secret doesn't exist. Audited regardless of outcome so the
// trail captures "someone tried to delete X" even if X was already gone.
func (h *Handler) DeleteStackSecret(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	if name == "" {
		writeError(w, "name is required", http.StatusBadRequest)
		return
	}

	removed, err := h.Store.DeleteStackSecret(stack.ID, name)
	if err != nil {
		writeError(w, "failed to delete stack secret", http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpStackSecretDelete)
	entry.ResourceType = "stack_secret"
	entry.ResourceID = name
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	if !removed {
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = "not found"
	}
	_ = h.auditOr().Record(r.Context(), entry)

	if !removed {
		writeError(w, "secret not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAuditEntries returns audit rows newest-first. Immutable by design —
// there's no write/update/delete endpoint here; the store's schema only
// supports append. Retention is time-based (handled by the cleanup
// routine, added separately) rather than per-stack cascade.
//
// Query parameters:
//   - stack:     exact match on stack name (convenience — resolves to ID first)
//   - actor:     exact match on actor
//   - operation: exact match on operation (see store.AuditOp*)
//   - since:     Go duration (e.g. "24h") — only entries newer than N ago
//   - limit:     max rows, default 100, store-capped at 1000
func (h *Handler) ListAuditEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := store.AuditFilter{
		Actor:     q.Get("actor"),
		Operation: q.Get("operation"),
	}

	// The stack query param accepts either an ID or a name. Resolve
	// to an ID when it looks like a name — saves the caller having to
	// know which they have.
	if raw := q.Get("stack"); raw != "" {
		if s, _ := h.Store.GetStack(raw); s != nil {
			filter.StackID = s.ID
		} else if s, _ := h.Store.GetStackByName(raw); s != nil {
			filter.StackID = s.ID
		} else {
			// Unknown stack — filter by the raw value against stack_name
			// so entries from a now-deleted stack still surface when
			// the caller names it.
			filter.StackName = raw
		}
	}

	if raw := q.Get("since"); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, "since must be a Go duration (e.g. 24h, 5m)", http.StatusBadRequest)
			return
		}
		filter.Since = time.Now().Add(-dur)
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, "limit must be a non-negative integer", http.StatusBadRequest)
			return
		}
		filter.Limit = n
	}

	entries, err := h.Store.ListAuditEntries(filter)
	if err != nil {
		logctx.FromContext(r.Context()).WithError(err).Error("list audit entries")
		writeError(w, "failed to list audit entries", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	writeJSON(w, entries, http.StatusOK)
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

	startEntry := audit.FromRequest(r, store.AuditOpDeployStart)
	startEntry.ResourceType = "stack"
	startEntry.ResourceID = stack.ID
	startEntry.StackID = stack.ID
	startEntry.StackName = stack.Name
	startEntry.Outcome = store.AuditOutcomeInProgress
	startEntry.Metadata = map[string]string{"trigger": store.TriggerWebhook}
	_ = h.auditOr().Record(r.Context(), startEntry)

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

// wsLogUpgrader upgrades HTTP → WebSocket for the log-follow endpoint.
// Origin check is permissive — authentication lives at the API-key
// layer, and CSRF isn't a meaningful concern for a read-only log
// stream from a server-to-server or curl-to-server caller. If this
// ever grows a browser UI that stores session cookies, switch to a
// strict same-origin check.
var wsLogUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// wsWriteTimeout is how long a single frame write is allowed to take
// before we give up on a stuck client. Applied per-message so a slow
// consumer doesn't pin the goroutine forever.
const wsWriteTimeout = 10 * time.Second

// wsPingInterval is the cadence of application-level ping frames; they
// catch half-closed TCP connections that the OS hasn't noticed yet.
// The reader goroutine enforces a corresponding read deadline of 2x.
const wsPingInterval = 30 * time.Second

// StreamStackContainerLogs upgrades the connection to WebSocket and
// streams container logs line-by-line in real time.  Matches the
// query-param surface of /logs (tail, since, timestamps) so a client
// can "paginate" into a live tail: open /logs?tail=500 first for
// history, then /logs/stream for new lines.
//
// Each log line arrives as one WebSocket text message. Non-TTY
// containers have their multiplexed stdout/stderr demuxed on our
// side so callers see clean text.
//
// Authentication is the same X-API-KEY the rest of the API uses; the
// upgrade request is a regular HTTP GET, so middleware still applies.
// Browsers that can't set custom headers on new WebSocket() will need
// a proxy or a short-lived-token flow (not shipped yet).
func (h *Handler) StreamStackContainerLogs(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	// Validate the upgrade before we touch the client connection so
	// callers that fail auth/membership get a regular JSON error,
	// not a half-completed handshake.
	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(r.Context(), w, stack.Name, cid)
	if !ok {
		return
	}

	// Query-param parsing before upgrade — same as /logs.
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
		since = strconv.FormatInt(time.Now().Add(-dur).Unix(), 10)
	}
	timestamps := r.URL.Query().Get("timestamps") == "true"

	conn, err := wsLogUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade() has already written its own error response to w; just log.
		logctx.FromContext(r.Context()).WithError(err).Debug("websocket upgrade failed")
		return
	}
	defer conn.Close()

	// Scope the docker stream to the websocket's lifetime. A client
	// disconnect cancels this, which tears down the docker logs stream
	// inside the client library — daemon releases within ~1s.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := h.Docker.ContainerLogs(ctx, insp.Container.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailStr,
		Since:      since,
		Timestamps: timestamps,
		Follow:     true,
	})
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "container logs: "+err.Error()),
			time.Now().Add(wsWriteTimeout))
		return
	}
	defer stream.Close()

	// Monitor the socket for client-initiated close so we can cancel
	// the upstream Docker stream.  We don't actually expect messages
	// from the client (read-only stream), but without a reader, pings
	// and close frames wouldn't be observed.
	go func() {
		conn.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
			return nil
		})
		for {
			if _, _, err := conn.NextReader(); err != nil {
				cancel()
				return
			}
		}
	}()

	// Ticker for pings — catches half-closed TCPs the kernel hasn't
	// detected yet.
	pings := time.NewTicker(wsPingInterval)
	defer pings.Stop()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-pings.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout))
			}
		}
	}()

	// Demux → line-framed → WriteMessage.
	writer := &wsLogWriter{conn: conn}
	if insp.Container.Config != nil && insp.Container.Config.Tty {
		_, _ = io.Copy(writer, stream)
	} else {
		_, _ = stdcopy.StdCopy(writer, writer, stream)
	}

	// stdcopy finished: the container exited or the daemon dropped us.
	// Send a normal close frame so the client knows the stream ended
	// cleanly rather than timing out.
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(wsWriteTimeout))

	cancel()
	<-pingDone
}

// wsLogWriter adapts an io.Writer interface (what stdcopy expects) to
// gorilla/websocket's frame-based API. Each Write becomes one or more
// text messages, split on newlines so each log line is its own frame.
type wsLogWriter struct {
	conn *websocket.Conn
	buf  []byte // carries partial trailing lines across Write calls
}

func (w *wsLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if len(line) == 0 {
			continue
		}
		_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := w.conn.WriteMessage(websocket.TextMessage, line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
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

// RestartStackContainer restarts a single managed container.  This is
// the first mutating introspection endpoint: it bypasses the GitOps
// flow (no compose change, no deploy record) but is always audited so
// the trail captures "someone did something imperative here."
//
// The restart is best-effort: Docker stops the container, waits up to
// the grace period for it to exit cleanly, then starts it again. The
// daemon returns success as soon as it has issued the commands — we
// return 202 Accepted to match that semantics (the container may still
// be transitioning when the response lands).
//
// Query parameters:
//   - t: stop grace period in seconds (optional). -1 waits indefinitely;
//     0 skips graceful shutdown entirely; default is Docker's 10s.
//
// On an unhealthy container, restart is useful for "give it another
// kick" debug scenarios. It is NOT a path for state changes — to change
// image tags, replica counts, or config, commit to git and redeploy.
func (h *Handler) RestartStackContainer(w http.ResponseWriter, r *http.Request) {
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

	opts := client.ContainerRestartOptions{}
	if raw := r.URL.Query().Get("t"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, "t must be an integer (seconds); -1 waits forever, 0 kills immediately", http.StatusBadRequest)
			return
		}
		opts.Timeout = &n
	}

	// Fire off the restart. Record the audit row regardless of outcome
	// so operators see "someone tried to restart this" even on failures.
	_, restartErr := h.Docker.ContainerRestart(ctx, insp.Container.ID, opts)

	entry := audit.FromRequest(r, store.AuditOpContainerRestart)
	entry.ResourceType = "container"
	entry.ResourceID = insp.Container.ID
	entry.StackID = stack.ID
	entry.StackName = stack.Name
	entry.Metadata = map[string]string{}
	if insp.Container.Config != nil {
		if svc := insp.Container.Config.Labels[containerLabelServiceName]; svc != "" {
			entry.Metadata["service"] = svc
		}
		if rep := insp.Container.Config.Labels[containerLabelReplicaIndex]; rep != "" {
			entry.Metadata["replica"] = rep
		}
	}
	if opts.Timeout != nil {
		entry.Metadata["timeout_seconds"] = strconv.Itoa(*opts.Timeout)
	}
	if restartErr != nil {
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = restartErr.Error()
	}
	_ = h.auditOr().Record(r.Context(), entry)

	if restartErr != nil {
		logctx.FromContext(ctx).WithError(restartErr).Error("container restart failed")
		writeError(w, "failed to restart container", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{
		"status":       "accepted",
		"container_id": insp.Container.ID,
	}, http.StatusAccepted)
}

// ExecStackContainer upgrades the request to a WebSocket and runs a
// command inside the target container with stdin/stdout/stderr
// wired back to the client. The headline debug op — the one an
// operator reaches for when logs + stats + restart didn't answer
// the question.
//
// Query params:
//   - cmd     — repeatable; the command + args. At least one required.
//     Example: ?cmd=sh    or    ?cmd=sh&cmd=-c&cmd=ls+-la
//   - tty     — bool, default true. Interactive shells use TTY=true;
//     the MVP only supports TTY mode so output is a single raw stream
//     and no stdcopy demux is needed either direction.
//   - user    — optional, runs as this user inside the container.
//   - workdir — optional, starts in this directory.
//
// WebSocket protocol (MVP, TTY):
//   - Client → server: binary messages containing stdin bytes.
//   - Server → client: binary messages containing raw output bytes.
//   - On exit: server sends a CloseNormalClosure frame with the exit
//     code encoded in the reason text (e.g. "exit_code=0").
//
// This is a mutation of live container state. Both exec_start and
// exec_end are audited with actor=api-key, request_id for log
// correlation, and metadata (cmd, tty, user, exit_code, duration).
func (h *Handler) ExecStackContainer(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	// Parse params before touching the socket so failures are clean
	// JSON errors, not half-complete WS handshakes.
	cmd := r.URL.Query()["cmd"]
	if len(cmd) == 0 {
		writeError(w, "cmd query param is required (pass it multiple times for additional args)", http.StatusBadRequest)
		return
	}
	useTTY := r.URL.Query().Get("tty") != "false" // default true
	user := r.URL.Query().Get("user")
	workdir := r.URL.Query().Get("workdir")

	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(r.Context(), w, stack.Name, cid)
	if !ok {
		return
	}

	// Create the exec instance BEFORE upgrading — a failure here is a
	// clean 500/JSON response, not a half-open WS. Requires a timeout
	// context that outlives only the setup phase.
	setupCtx, setupCancel := context.WithTimeout(r.Context(), 10*time.Second)
	createRes, err := h.Docker.ExecCreate(setupCtx, insp.Container.ID, client.ExecCreateOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          useTTY,
		Cmd:          cmd,
		User:         user,
		WorkingDir:   workdir,
	})
	setupCancel()
	if err != nil {
		logctx.FromContext(r.Context()).WithError(err).Error("exec create failed")
		writeError(w, "failed to create exec session", http.StatusInternalServerError)
		return
	}

	startEntry := audit.FromRequest(r, store.AuditOpContainerExecStart)
	startEntry.ResourceType = "container"
	startEntry.ResourceID = insp.Container.ID
	startEntry.StackID = stack.ID
	startEntry.StackName = stack.Name
	startEntry.Outcome = store.AuditOutcomeInProgress
	startEntry.Metadata = map[string]string{
		"exec_id": createRes.ID,
		"cmd":     strings.Join(cmd, " "),
		"tty":     strconv.FormatBool(useTTY),
	}
	if user != "" {
		startEntry.Metadata["user"] = user
	}
	if workdir != "" {
		startEntry.Metadata["workdir"] = workdir
	}
	_ = h.auditOr().Record(r.Context(), startEntry)

	conn, err := wsLogUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logctx.FromContext(r.Context()).WithError(err).Debug("websocket upgrade failed")
		return
	}
	defer conn.Close()

	// Attach runs until the exec finishes (container exit, command
	// completion, or we tear it down). ExecAttach returns a raw
	// net.Conn; the returned HijackedResponse owns the underlying
	// connection and must be closed.
	attachCtx, attachCancel := context.WithCancel(r.Context())
	defer attachCancel()

	attachRes, err := h.Docker.ExecAttach(attachCtx, createRes.ID, client.ExecAttachOptions{
		TTY: useTTY,
	})
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "exec attach: "+err.Error()),
			time.Now().Add(wsWriteTimeout))
		h.recordExecEnd(r, startEntry, 0, time.Now(), err)
		return
	}
	defer attachRes.Conn.Close()

	// Resize callback — only meaningful in TTY mode. For non-TTY we
	// bind it to a no-op so control frames that arrive anyway (clients
	// don't always know whether they got a TTY) are silently ignored.
	resize := func(uint, uint) {}
	if useTTY {
		execID := createRes.ID
		resize = func(rows, cols uint) {
			if rows == 0 || cols == 0 {
				return
			}
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer rcancel()
			if _, err := h.Docker.ExecResize(rctx, execID, client.ExecResizeOptions{
				Height: rows, Width: cols,
			}); err != nil {
				logctx.FromContext(r.Context()).WithError(err).Debug("exec resize failed")
			}
		}
	}

	execStart := time.Now()
	exitCode := execPipe(attachCtx, conn, attachRes, useTTY, resize)

	// ExecInspect to get the authoritative exit code when possible;
	// falls back to the pipe's best guess (0 for clean close, non-zero
	// on errors). Exit code wedged at -1 means we couldn't determine.
	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer inspectCancel()
	if ins, err := h.Docker.ExecInspect(inspectCtx, createRes.ID, client.ExecInspectOptions{}); err == nil {
		exitCode = ins.ExitCode
	}

	// Record the audit end BEFORE writing the close frame. Policy: by
	// the time a client observes session termination, the trail must
	// be durable. Otherwise a well-timed crash or slow recorder would
	// leave a "started, unknown outcome" audit gap.
	h.recordExecEnd(r, startEntry, exitCode, execStart, nil)

	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("exit_code=%d", exitCode)),
		time.Now().Add(wsWriteTimeout))
}

// execPipe shuttles bytes between the WebSocket and the exec
// connection until either side closes. Returns a best-guess exit
// code (0 on clean close, -1 if the stream was torn down abnormally);
// the caller prefers the ExecInspect value when available.
//
// Message typing on the WS:
//   - BinaryMessage from the client → stdin written to the exec conn.
//   - TextMessage  from the client → JSON control frame. Today we
//     understand {"type":"resize","rows":N,"cols":N}; unknown types
//     are silently ignored for forward compatibility.
//   - BinaryMessage from the server → container output. In TTY mode
//     the daemon's output is a raw stream, copied as-is; in non-TTY
//     mode it's stdcopy-framed (stdout/stderr multiplexed with
//     8-byte headers) and we demux both streams into the same WS
//     output so the client sees clean text.
func execPipe(ctx context.Context, ws *websocket.Conn, attach client.ExecAttachResult, useTTY bool, onResize func(rows, cols uint)) int {
	done := make(chan struct{}, 2)

	// WS → exec: stdin (+ control frames)
	go func() {
		defer func() { done <- struct{}{} }()
		ws.SetPongHandler(func(string) error {
			ws.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
			return nil
		})
		ws.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				// Close write side of the exec conn so Docker sees EOF
				// on stdin and the command can terminate cleanly (many
				// tools exit on stdin EOF).
				_ = attach.CloseWrite()
				return
			}
			switch msgType {
			case websocket.BinaryMessage:
				if _, err := attach.Conn.Write(data); err != nil {
					return
				}
			case websocket.TextMessage:
				handleExecControl(data, onResize)
			}
		}
	}()

	// exec → WS: stdout/stderr
	go func() {
		defer func() { done <- struct{}{} }()
		if useTTY {
			// TTY stream is raw — copy chunks straight through.
			buf := make([]byte, 32*1024)
			for {
				n, err := attach.Reader.Read(buf)
				if n > 0 {
					_ = ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
					if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}
		// Non-TTY: demux stdcopy frames. Both stdout and stderr
		// targets write into the same WS stream — callers get a
		// single merged view, same shape as the TTY case.
		wsw := &execWSWriter{ws: ws}
		_, _ = stdcopy.StdCopy(wsw, wsw, attach.Reader)
	}()

	pings := time.NewTicker(wsPingInterval)
	defer pings.Stop()
	for {
		select {
		case <-ctx.Done():
			return -1
		case <-done:
			return 0
		case <-pings.C:
			_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout))
		}
	}
}

// execWSWriter wraps a WebSocket conn as an io.Writer that emits one
// BinaryMessage per call. Used in non-TTY mode as the destination for
// stdcopy.StdCopy, which reads Docker's multiplexed framing and writes
// the demuxed payload into the writer in chunks.
type execWSWriter struct {
	ws *websocket.Conn
}

func (w *execWSWriter) Write(p []byte) (int, error) {
	_ = w.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := w.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// execControlFrame is the JSON shape clients send as WebSocket
// TextMessages to control an exec session. Today "resize" is the only
// type; unknown types are silently ignored so future additions don't
// break old servers or vice versa.
type execControlFrame struct {
	Type string `json:"type"`
	Rows uint   `json:"rows,omitempty"`
	Cols uint   `json:"cols,omitempty"`
}

// handleExecControl parses a control frame and dispatches recognised
// commands. Malformed JSON is silently dropped — a TTY session
// shouldn't die because a client sent a bad frame, and the audit trail
// doesn't need per-frame detail. Debug logs would be the place to
// surface parse failures if they become a support headache.
func handleExecControl(data []byte, onResize func(rows, cols uint)) {
	var frame execControlFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "resize":
		if onResize != nil {
			onResize(frame.Rows, frame.Cols)
		}
	}
}

// recordExecEnd writes the terminating audit row. Always called even
// when the exec never got past ExecAttach, so the trail captures
// attempts that failed.  startEntry is passed in so we inherit its
// ResourceID / StackID / Metadata without rebuilding.
func (h *Handler) recordExecEnd(r *http.Request, start store.AuditEntry, exitCode int, startTime time.Time, attachErr error) {
	end := audit.FromRequest(r, store.AuditOpContainerExecEnd)
	end.ResourceType = start.ResourceType
	end.ResourceID = start.ResourceID
	end.StackID = start.StackID
	end.StackName = start.StackName
	end.Metadata = map[string]string{}
	for k, v := range start.Metadata {
		end.Metadata[k] = v
	}
	end.Metadata["duration_seconds"] = fmt.Sprintf("%.3f", time.Since(startTime).Seconds())
	end.Metadata["exit_code"] = strconv.Itoa(exitCode)

	if attachErr != nil {
		end.Outcome = store.AuditOutcomeFailure
		end.ErrorMessage = attachErr.Error()
	} else if exitCode != 0 {
		end.Outcome = store.AuditOutcomeFailure
	} else {
		end.Outcome = store.AuditOutcomeSuccess
	}
	_ = h.auditOr().Record(r.Context(), end)
}

// StreamStackContainerStats upgrades to a WebSocket and emits one
// computed stats sample per Docker sampling tick (roughly 1s).  Shape
// is identical to the one-shot /stats payload so clients can reuse
// the same parser; the only difference is that the first sample may
// report cpu.percent=0 until Docker has a prior sample to diff against.
//
// This is the "I want a live graph" counterpart to /stats.  Same X-API-KEY
// auth on the upgrade GET as the rest of the API.  Ping/close semantics
// mirror /logs/stream — 30s application pings, normal-closure frame
// on daemon EOF, read goroutine cancels the upstream stream on
// client-initiated disconnect.
func (h *Handler) StreamStackContainerStats(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	cid := mux.Vars(r)["cid"]
	insp, ok := h.resolveStackContainer(r.Context(), w, stack.Name, cid)
	if !ok {
		return
	}

	conn, err := wsLogUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logctx.FromContext(r.Context()).WithError(err).Debug("websocket upgrade failed")
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Stream=true asks the daemon to emit samples on its own cadence
	// (~1s). No IncludePreviousSample — each subsequent sample already
	// carries the prior one in PreCPUStats.
	res, err := h.Docker.ContainerStats(ctx, insp.Container.ID, client.ContainerStatsOptions{
		Stream: true,
	})
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "container stats: "+err.Error()),
			time.Now().Add(wsWriteTimeout))
		return
	}
	defer res.Body.Close()

	// Monitor the socket for client close and pong replies.
	go func() {
		conn.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(wsPingInterval * 2))
			return nil
		})
		for {
			if _, _, err := conn.NextReader(); err != nil {
				cancel()
				return
			}
		}
	}()

	pings := time.NewTicker(wsPingInterval)
	defer pings.Stop()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-pings.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout))
			}
		}
	}()

	// Docker streams stats as newline-delimited JSON objects; decode one
	// per iteration, project, and frame as a WS text message.
	dec := json.NewDecoder(res.Body)
	log := logctx.FromContext(ctx)
	for {
		var raw container.StatsResponse
		if err := dec.Decode(&raw); err != nil {
			if err != io.EOF {
				log.WithError(err).Debug("stats stream decode ended")
			}
			break
		}
		sample := computeStatsSample(raw, insp.Container.ID, insp.Container.Name)
		payload, marshalErr := json.Marshal(sample)
		if marshalErr != nil {
			log.WithError(marshalErr).Warn("marshal stats sample")
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			break
		}
	}

	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(wsWriteTimeout))

	cancel()
	<-pingDone
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

// --------------------------------------------------------------------------
// Events stream
// --------------------------------------------------------------------------

// eventsKeepalive is the cadence at which the handler writes an SSE
// comment line during quiet periods. Caddy, nginx, and ELB default
// idle timeouts are typically 60–120s; 25s keeps us comfortably
// below the shortest of those without spamming.
const eventsKeepalive = 25 * time.Second

// StackEvent is the projected view of a Docker event for a stack's
// managed resources. Narrower than events.Message so our API contract
// doesn't bake in moby's internal constants.
type StackEvent struct {
	Time       time.Time         `json:"time"`
	Type       string            `json:"type"`   // "container", "network", "volume", "image"
	Action     string            `json:"action"` // "create", "start", "die", "health_status: healthy", ...
	ActorID    string            `json:"actor_id"`
	Name       string            `json:"name,omitempty"`
	Image      string            `json:"image,omitempty"`
	Service    string            `json:"service,omitempty"`
	Replica    *int              `json:"replica,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// StreamStackEvents opens a Server-Sent Events stream of Docker events
// filtered to this stack's managed resources. Uses SSE rather than
// WebSocket because this is purely server→client with no control
// messages — SSE is simpler and works through every HTTP proxy without
// special configuration.
//
// Query parameters:
//   - since     (Go duration): only emit events newer than N ago
//   - types     (CSV of Docker event types; default "container"):
//     narrow to container/network/volume/image as needed.
func (h *Handler) StreamStackEvents(w http.ResponseWriter, r *http.Request) {
	stack, ok := h.resolveStack(w, r)
	if !ok {
		return
	}
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "streaming unsupported by this server", http.StatusInternalServerError)
		return
	}

	// Parse optional `since` — Docker accepts a Unix-seconds string.
	var since string
	if raw := r.URL.Query().Get("since"); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, "since must be a Go duration (e.g. 5m, 30s)", http.StatusBadRequest)
			return
		}
		since = strconv.FormatInt(time.Now().Add(-dur).Unix(), 10)
	}

	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue)).
		Add("label", fmt.Sprintf("%s=%s", containerLabelStackName, stack.Name))

	// Default to container events only; callers who want network/volume
	// /image events opt in explicitly via ?types=.
	types := r.URL.Query().Get("types")
	if types == "" {
		types = "container"
	}
	for _, t := range strings.Split(types, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			filters = filters.Add("type", t)
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	res := h.Docker.Events(ctx, client.EventsListOptions{
		Since:   since,
		Filters: filters,
	})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell nginx not to buffer — defaults to buffering proxied upstreams.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Initial flush so the client's open-handler fires before we wait for
	// the first event.
	flusher.Flush()

	keepalive := time.NewTicker(eventsKeepalive)
	defer keepalive.Stop()

	log := logctx.FromContext(ctx)

	for {
		select {
		case <-ctx.Done():
			return

		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case err, ok := <-res.Err:
			if !ok {
				return
			}
			// Docker sends io.EOF when the stream ends cleanly (e.g. daemon
			// restart during the request). Don't log that as an error.
			if err != nil && err != io.EOF {
				log.WithError(err).Warn("docker events stream error")
			}
			return

		case msg, ok := <-res.Messages:
			if !ok {
				return
			}
			payload, marshalErr := json.Marshal(projectEvent(msg))
			if marshalErr != nil {
				log.WithError(marshalErr).Warn("marshal event")
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// projectEvent digests a Docker event into our API shape, pulling out
// the accelero-service/replica labels from the attributes map so
// clients don't have to dig.
func projectEvent(m events.Message) StackEvent {
	t := time.Unix(0, m.TimeNano)
	if m.TimeNano == 0 {
		t = time.Unix(m.Time, 0)
	}

	evt := StackEvent{
		Time:       t.UTC(),
		Type:       string(m.Type),
		Action:     string(m.Action),
		ActorID:    m.Actor.ID,
		Attributes: m.Actor.Attributes,
	}
	// Docker stashes the human name under attributes["name"] for
	// container/network events; image events use "image" instead.
	if n := m.Actor.Attributes["name"]; n != "" {
		evt.Name = n
	}
	if img := m.Actor.Attributes["image"]; img != "" {
		evt.Image = img
	}
	if svc := m.Actor.Attributes[containerLabelServiceName]; svc != "" {
		evt.Service = svc
	}
	if replicaRaw := m.Actor.Attributes[containerLabelReplicaIndex]; replicaRaw != "" {
		if idx := parseReplicaLabel(replicaRaw); idx != nil {
			evt.Replica = idx
		}
	}
	return evt
}

// --------------------------------------------------------------------------
// Resource browsers (Phase 3)
// --------------------------------------------------------------------------

// ManagedImage is a projected image entry for the /images endpoint.
// "Managed" here means "currently referenced by at least one container
// with managed-by=accelero" — Docker images themselves don't carry our
// labels, so we derive usage by walking managed containers.
type ManagedImage struct {
	ID        string        `json:"id"`
	RepoTags  []string      `json:"repo_tags,omitempty"`
	SizeBytes int64         `json:"size_bytes"`
	CreatedAt time.Time     `json:"created_at"`
	UsedBy    []ImageUsage  `json:"used_by"`
}

// ImageUsage is a back-reference from an image to the containers that
// currently pull from it. Sorted lexically so scrapes are deterministic.
type ImageUsage struct {
	Stack     string `json:"stack"`
	Service   string `json:"service,omitempty"`
	Replica   *int   `json:"replica,omitempty"`
	Container string `json:"container_id"`
}

// ManagedVolume is the projected view of a Docker volume filtered to
// accelero-managed resources.
type ManagedVolume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Stack      string            `json:"stack,omitempty"`
	MountPoint string            `json:"mount_point"`
	CreatedAt  string            `json:"created_at,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// ManagedNetwork is the projected view of a Docker network.
// Connected-container enumeration is deferred — the /stacks/{id}/containers
// endpoint already lets operators see which containers belong to each
// stack. If a view of "who's on this network" becomes load-bearing we
// can add a verbose mode that hits NetworkInspect.
type ManagedNetwork struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Driver    string            `json:"driver"`
	Scope     string            `json:"scope"`
	Stack     string            `json:"stack,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
}

// ListManagedImages returns every image currently pulled into use by an
// accelero-managed container. Query param ?stack=<name> narrows to one
// stack.  We have to walk containers first because Docker images
// themselves don't carry management labels.
func (h *Handler) ListManagedImages(w http.ResponseWriter, r *http.Request) {
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), containerOpTimeout)
	defer cancel()

	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue))
	if stackName := r.URL.Query().Get("stack"); stackName != "" {
		filters = filters.Add("label", fmt.Sprintf("%s=%s", containerLabelStackName, stackName))
	}

	containers, err := h.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("container list failed")
		writeError(w, "failed to list containers", http.StatusInternalServerError)
		return
	}

	// Map each image ID to the containers that reference it.
	usageByImage := make(map[string][]ImageUsage)
	for _, c := range containers.Items {
		use := ImageUsage{
			Stack:     c.Labels[containerLabelStackName],
			Service:   c.Labels[containerLabelServiceName],
			Replica:   parseReplicaLabel(c.Labels[containerLabelReplicaIndex]),
			Container: c.ID,
		}
		usageByImage[c.ImageID] = append(usageByImage[c.ImageID], use)
	}

	if len(usageByImage) == 0 {
		writeJSON(w, []ManagedImage{}, http.StatusOK)
		return
	}

	// Docker has no "filter by image id" option on ImageList, so we list
	// all and intersect. On a typical host this is cheap — only images
	// we actually care about get projected into the response.
	imageList, err := h.Docker.ImageList(ctx, client.ImageListOptions{All: false})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("image list failed")
		writeError(w, "failed to list images", http.StatusInternalServerError)
		return
	}

	out := make([]ManagedImage, 0, len(usageByImage))
	for _, img := range imageList.Items {
		usage, ok := usageByImage[img.ID]
		if !ok {
			continue
		}
		sort.Slice(usage, func(i, j int) bool {
			if usage[i].Stack != usage[j].Stack {
				return usage[i].Stack < usage[j].Stack
			}
			if usage[i].Service != usage[j].Service {
				return usage[i].Service < usage[j].Service
			}
			return usage[i].Container < usage[j].Container
		})
		out = append(out, ManagedImage{
			ID:        img.ID,
			RepoTags:  img.RepoTags,
			SizeBytes: img.Size,
			CreatedAt: time.Unix(img.Created, 0).UTC(),
			UsedBy:    usage,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return firstTag(out[i].RepoTags) < firstTag(out[j].RepoTags)
	})

	writeJSON(w, out, http.StatusOK)
}

// firstTag returns the first repo tag for deterministic sort, or empty.
func firstTag(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return tags[0]
}

// ListManagedVolumes returns volumes labelled managed-by=accelero.
// Scope-narrowing via ?stack=<name>. Warnings from the daemon are
// surfaced via logs — not worth polluting the response for rare
// filesystem-level issues that don't affect the listing.
func (h *Handler) ListManagedVolumes(w http.ResponseWriter, r *http.Request) {
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), containerOpTimeout)
	defer cancel()

	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue))
	if stackName := r.URL.Query().Get("stack"); stackName != "" {
		filters = filters.Add("label", fmt.Sprintf("%s=%s", containerLabelStackName, stackName))
	}

	res, err := h.Docker.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("volume list failed")
		writeError(w, "failed to list volumes", http.StatusInternalServerError)
		return
	}
	for _, warn := range res.Warnings {
		logctx.FromContext(ctx).Warnf("docker volume list warning: %s", warn)
	}

	out := make([]ManagedVolume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, ManagedVolume{
			Name:       v.Name,
			Driver:     v.Driver,
			Stack:      v.Labels[containerLabelStackName],
			MountPoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Labels:     v.Labels,
			Options:    v.Options,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	writeJSON(w, out, http.StatusOK)
}

// maxVolumeFileDownload caps how many bytes a single /browse download
// may return. Protects against accidental "download all of my 50GB
// postgres WAL" requests — operators who legitimately need bigger
// files should exec in and tar it out manually, which gives them a
// moment to think about what they're doing.
const maxVolumeFileDownload = 10 * 1024 * 1024 // 10 MB

// volumeIsManaged verifies that the named volume carries the
// managed-by=accelero label before the browser is allowed to touch
// it. Same defense-in-depth rule as every other stack-scoped endpoint:
// never return information (or spawn helpers for) resources we don't
// claim.
func (h *Handler) volumeIsManaged(ctx context.Context, name string) (bool, error) {
	if h.Docker == nil {
		return false, nil
	}
	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue)).
		Add("name", name)
	res, err := h.Docker.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		return false, err
	}
	for _, v := range res.Items {
		if v.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// BrowseVolume lists files in a managed volume or streams one file's
// contents back. This is a debug affordance for "what's actually in my
// postgres data dir" scenarios — read-only, audited, capped in size.
//
// Query parameters:
//   - path:     absolute within the volume; default "/" (volume root)
//   - download: "true" returns raw file bytes; omit/false returns JSON listing
//
// Unmanaged volumes return 404 indistinguishably from missing volumes
// so callers can't probe. The handler also blocks `..` in the path
// before the browser gets it, giving two layers of path-escape defense.
func (h *Handler) BrowseVolume(w http.ResponseWriter, r *http.Request) {
	if h.VolumeBrowser == nil || h.Docker == nil {
		writeError(w, "volume browsing is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	name := mux.Vars(r)["name"]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	managed, err := h.volumeIsManaged(ctx, name)
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("check volume managed")
		writeError(w, "failed to verify volume", http.StatusInternalServerError)
		return
	}
	if !managed {
		writeError(w, "volume not found", http.StatusNotFound)
		return
	}

	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		reqPath = "/"
	}
	download := r.URL.Query().Get("download") == "true"

	if download {
		h.downloadVolumeFile(ctx, w, r, name, reqPath)
		return
	}
	h.listVolumePath(ctx, w, r, name, reqPath)
}

func (h *Handler) listVolumePath(ctx context.Context, w http.ResponseWriter, r *http.Request, volumeName, path string) {
	entries, err := h.VolumeBrowser.ListPath(ctx, volumeName, path)
	// Audit the browse attempt regardless of outcome — the intent is
	// worth recording even if the underlying call errored.
	audit := audit.FromRequest(r, store.AuditOpVolumeBrowse)
	audit.ResourceType = "volume"
	audit.ResourceID = volumeName
	audit.Metadata = map[string]string{"path": path}
	if err != nil {
		audit.Outcome = store.AuditOutcomeFailure
		audit.ErrorMessage = err.Error()
		_ = h.auditOr().Record(r.Context(), audit)
		logctx.FromContext(ctx).WithError(err).Warn("volume browse failed")
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = h.auditOr().Record(r.Context(), audit)

	if entries == nil {
		entries = []volumepkg.FileEntry{}
	}
	writeJSON(w, entries, http.StatusOK)
}

func (h *Handler) downloadVolumeFile(ctx context.Context, w http.ResponseWriter, r *http.Request, volumeName, path string) {
	res, err := h.VolumeBrowser.ReadFile(ctx, volumeName, path, maxVolumeFileDownload)
	auditEntry := audit.FromRequest(r, store.AuditOpVolumeRead)
	auditEntry.ResourceType = "volume"
	auditEntry.ResourceID = volumeName
	auditEntry.Metadata = map[string]string{"path": path}

	if err != nil {
		auditEntry.Outcome = store.AuditOutcomeFailure
		auditEntry.ErrorMessage = err.Error()
		_ = h.auditOr().Record(r.Context(), auditEntry)
		logctx.FromContext(ctx).WithError(err).Warn("volume read failed")
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer res.Content.Close()

	auditEntry.Metadata["size_bytes"] = strconv.FormatInt(res.SizeBytes, 10)
	_ = h.auditOr().Record(r.Context(), auditEntry)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(res.SizeBytes, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, res.Name))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, res.Content); err != nil {
		logctx.FromContext(ctx).WithError(err).Warn("volume read: truncated stream")
	}
}

// maxVolumeFileUpload caps how many bytes a single /files write can
// push. Same ceiling as maxVolumeFileDownload — this endpoint is for
// "emergency config patch" scenarios, not for seeding a 50GB dataset.
const maxVolumeFileUpload = 10 * 1024 * 1024 // 10 MB

// WriteVolumeFile writes (or overwrites) a file inside a managed
// volume. Disabled by default — operators opt in by setting
// ALLOW_VOLUME_WRITES=true on the accelero process. Every call is
// audited as volume.write whether it succeeded or not.
//
// Query parameters:
//   - path:  absolute within the volume; required; must point at a file
//   - mode:  optional octal POSIX file mode, e.g. "0644". Defaults to 0644.
//
// The request body is the raw file bytes (Content-Type is ignored;
// stored verbatim). Content-Length determines how many bytes are
// streamed into the helper container, so callers should set it.
// The 10 MB cap is enforced via http.MaxBytesReader — exceeding it
// produces a 413 Request Entity Too Large before the tar stream
// starts.
func (h *Handler) WriteVolumeFile(w http.ResponseWriter, r *http.Request) {
	if !h.AllowVolumeWrites {
		writeError(w, "volume writes are disabled on this server (set ALLOW_VOLUME_WRITES=true to enable)", http.StatusForbidden)
		return
	}
	if h.VolumeBrowser == nil || h.Docker == nil {
		writeError(w, "volume browsing is not configured on this server", http.StatusServiceUnavailable)
		return
	}

	name := mux.Vars(r)["name"]
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	managed, err := h.volumeIsManaged(ctx, name)
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("check volume managed")
		writeError(w, "failed to verify volume", http.StatusInternalServerError)
		return
	}
	if !managed {
		writeError(w, "volume not found", http.StatusNotFound)
		return
	}

	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		writeError(w, "path query param is required", http.StatusBadRequest)
		return
	}

	var mode uint32 = 0o644
	if raw := r.URL.Query().Get("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			writeError(w, "mode must be an octal POSIX file mode (e.g. 0644)", http.StatusBadRequest)
			return
		}
		mode = uint32(parsed)
	}

	// The body must be bounded — otherwise a caller could stream until
	// disk fills. maxBytesReader returns an error on Read past the cap
	// which surfaces cleanly to the client as a 413-ish bad request.
	body := http.MaxBytesReader(w, r.Body, maxVolumeFileUpload)
	defer body.Close()

	// Buffer into memory so we can compute the size without trusting
	// Content-Length (which may be missing or a lie). 10MB in-memory
	// is acceptable for an emergency-patch endpoint.
	buf, err := io.ReadAll(body)
	if err != nil {
		writeError(w, "failed to read request body (possibly exceeded 10 MB limit)", http.StatusRequestEntityTooLarge)
		return
	}

	writeErr := h.VolumeBrowser.WriteFile(ctx, name, reqPath, mode, bytes.NewReader(buf), int64(len(buf)))

	entry := audit.FromRequest(r, store.AuditOpVolumeWrite)
	entry.ResourceType = "volume"
	entry.ResourceID = name
	entry.Metadata = map[string]string{
		"path":       reqPath,
		"size_bytes": strconv.Itoa(len(buf)),
		"mode":       fmt.Sprintf("0%o", mode),
	}
	if writeErr != nil {
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = writeErr.Error()
	}
	_ = h.auditOr().Record(r.Context(), entry)

	if writeErr != nil {
		logctx.FromContext(ctx).WithError(writeErr).Warn("volume write failed")
		writeError(w, writeErr.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]interface{}{
		"status":     "written",
		"path":       reqPath,
		"size_bytes": len(buf),
	}, http.StatusOK)
}

// ListManagedNetworks returns networks labelled managed-by=accelero.
// Scope-narrowing via ?stack=<name>.
func (h *Handler) ListManagedNetworks(w http.ResponseWriter, r *http.Request) {
	if h.Docker == nil {
		writeError(w, "container introspection is not configured on this server", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), containerOpTimeout)
	defer cancel()

	filters := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", containerLabelManagedBy, containerLabelManagedValue))
	if stackName := r.URL.Query().Get("stack"); stackName != "" {
		filters = filters.Add("label", fmt.Sprintf("%s=%s", containerLabelStackName, stackName))
	}

	res, err := h.Docker.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		logctx.FromContext(ctx).WithError(err).Error("network list failed")
		writeError(w, "failed to list networks", http.StatusInternalServerError)
		return
	}

	out := make([]ManagedNetwork, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, ManagedNetwork{
			ID:        n.ID,
			Name:      n.Name,
			Driver:    n.Driver,
			Scope:     n.Scope,
			Stack:     n.Labels[containerLabelStackName],
			CreatedAt: n.Created.UTC(),
			Labels:    n.Labels,
			Options:   n.Options,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	writeJSON(w, out, http.StatusOK)
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
