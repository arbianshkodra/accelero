package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

const (
	maxPayloadSize = 1 << 20 // 1 MB
)

// Deployer is the interface the stack deployer must satisfy.
type Deployer interface {
	Deploy(ctx context.Context, stack *store.Stack, trigger string) (*store.Deployment, error)
}

// Handler holds all dependencies for the HTTP API.
type Handler struct {
	Store      store.Store
	Deployer   Deployer
	Reconciler *reconciler.Reconciler
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

	// Unauthenticated
	r.HandleFunc("/health", h.Health).Methods("GET")

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

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
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
