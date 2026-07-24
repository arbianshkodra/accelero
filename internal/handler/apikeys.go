package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/rbac"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/gorilla/mux"
)

// CreateAPIKey issues a new role-scoped API key. The plaintext key is returned
// exactly once in the response; only its hash is persisted. Admin-only (the
// route policy enforces the role). Audited as apikey.create.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		Role string `json:"role"`
		// StackGrants maps a stack (id or name) to a role that overrides the
		// base role for that stack. Optional.
		StackGrants map[string]string `json:"stack_grants"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		writeError(w, "name is required", http.StatusBadRequest)
		return
	}
	role, ok := rbac.ParseRole(input.Role)
	if !ok {
		writeError(w, "role must be one of: none, viewer, operator, admin", http.StatusBadRequest)
		return
	}

	// Resolve and validate per-stack grants: each key is a stack id-or-name
	// (must exist) and each value a valid role. Stored keyed by canonical ID.
	var grants map[string]string
	for stackToken, roleStr := range input.StackGrants {
		gr, ok := rbac.ParseRole(roleStr)
		if !ok {
			writeError(w, "stack_grants role for '"+stackToken+"' must be one of: none, viewer, operator, admin", http.StatusBadRequest)
			return
		}
		st, err := h.Store.GetStack(stackToken)
		if err != nil || st == nil {
			st, _ = h.Store.GetStackByName(stackToken)
		}
		if st == nil {
			writeError(w, "stack_grants references unknown stack: "+stackToken, http.StatusBadRequest)
			return
		}
		if grants == nil {
			grants = make(map[string]string, len(input.StackGrants))
		}
		grants[st.ID] = string(gr)
	}

	raw, err := store.GenerateAPIKey()
	if err != nil {
		writeError(w, "failed to generate key", http.StatusInternalServerError)
		return
	}
	id, err := generateID()
	if err != nil {
		writeError(w, "failed to generate ID", http.StatusInternalServerError)
		return
	}

	key := &store.APIKey{
		ID:          id,
		Name:        input.Name,
		Role:        string(role),
		KeyHash:     store.HashAPIKey(raw),
		StackGrants: grants,
		CreatedAt:   time.Now(),
	}
	if err := h.Store.CreateAPIKey(key); err != nil {
		writeError(w, "failed to create key", http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpAPIKeyCreate)
	entry.ResourceType = "apikey"
	entry.ResourceID = id
	entry.Metadata = map[string]string{"name": input.Name, "role": string(role)}
	if len(grants) > 0 {
		entry.Metadata["stack_grants"] = fmt.Sprintf("%d", len(grants))
	}
	_ = h.auditOr().Record(r.Context(), entry)

	// The raw key is shown ONCE — it cannot be recovered later.
	writeJSON(w, map[string]any{
		"id":           id,
		"name":         input.Name,
		"role":         string(role),
		"key":          raw,
		"stack_grants": grants,
		"created_at":   key.CreatedAt,
	}, http.StatusCreated)
}

// ListAPIKeys returns key metadata (never the key or its hash — APIKey.KeyHash
// is json:"-"). Admin-only.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Store.ListAPIKeys()
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []*store.APIKey{}
	}
	writeJSON(w, keys, http.StatusOK)
}

// DeleteAPIKey revokes a key by ID. 204 on success, 404 if absent. Admin-only.
// Audited as apikey.delete in both outcomes.
func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	deleted, err := h.Store.DeleteAPIKey(id)
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	entry := audit.FromRequest(r, store.AuditOpAPIKeyDelete)
	entry.ResourceType = "apikey"
	entry.ResourceID = id
	if !deleted {
		entry.Outcome = store.AuditOutcomeFailure
		entry.ErrorMessage = "not found"
	}
	_ = h.auditOr().Record(r.Context(), entry)

	if !deleted {
		writeError(w, "api key not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
