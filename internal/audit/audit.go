// Package audit provides the recorder that turns notable API actions and
// system events into immutable, persistent audit-log rows.
//
// Design notes:
//
//   - Writes go through a single Recorder interface so tests can swap in
//     a fake without touching SQLite. The concrete StoreRecorder just
//     delegates to the store.
//
//   - Audit writes are best-effort: a failure here MUST NOT block or roll
//     back the user-facing action. Callers log the error and move on.
//     The audit row is nice-to-have for operators; the business action
//     is the thing that actually matters.
//
//   - Recorder.Record takes a ready-made store.AuditEntry rather than a
//     kitchen-sink options bag. Callers assemble the entry from their
//     local context (HTTP request, stack, outcome) so the call site
//     reads like a plain log line.
//
//   - IDs are 16 random hex bytes. Collisions are astronomical given the
//     insert rate; matches the existing pattern for deployment/stack IDs.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/sirupsen/logrus"
)

// Recorder persists audit entries. The concrete StoreRecorder writes to
// SQLite; tests can provide their own implementation.
type Recorder interface {
	Record(ctx context.Context, e store.AuditEntry) error
}

// StoreRecorder is the production Recorder. It fills in ID + timestamp
// on each Record call if the caller didn't, and delegates the insert
// to the backing store. On write failure, the error is logged and
// swallowed — see package doc comment for the rationale.
type StoreRecorder struct {
	store store.Store
}

// NewStoreRecorder returns a Recorder that writes to the given store.
func NewStoreRecorder(s store.Store) *StoreRecorder {
	return &StoreRecorder{store: s}
}

// Record persists a single audit entry. ID and Timestamp are filled in
// here if the caller left them zero. Returns the error to the caller so
// it can be logged in context, but callers should NOT treat it as a
// reason to fail their own work.
func (r *StoreRecorder) Record(ctx context.Context, e store.AuditEntry) error {
	if e.ID == "" {
		id, err := generateID()
		if err != nil {
			return err
		}
		e.ID = id
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if err := r.store.CreateAuditEntry(&e); err != nil {
		// Log with enough context that operators can tell what was lost.
		logctx.FromContext(ctx).
			WithError(err).
			WithFields(logrus.Fields{
				"operation":     e.Operation,
				"resource_id":   e.ResourceID,
				"stack_id":      e.StackID,
				"outcome":       e.Outcome,
				"audit_dropped": true,
			}).
			Warn("audit write failed")
		return err
	}
	return nil
}

// FromRequest returns a partially-populated AuditEntry with the fields
// that are derivable from an HTTP request: actor (placeholder until
// multi-user auth lands), remote address, and request_id off context.
// Callers fill in Operation, ResourceType, ResourceID, StackID, etc.
func FromRequest(r *http.Request, op string) store.AuditEntry {
	ctx := r.Context()
	entry := store.AuditEntry{
		Operation: op,
		Actor:     ActorFromRequest(r),
		Outcome:   store.AuditOutcomeSuccess, // default; caller overrides on failure
	}
	if addr := remoteAddr(r); addr != "" {
		entry.RemoteAddr = addr
	}
	if reqID := requestIDFromContext(ctx); reqID != "" {
		entry.RequestID = reqID
	}
	return entry
}

// ActorFromRequest identifies who made a request. Until multi-user auth
// exists, every authenticated request collapses to "api-key" — we store
// it anyway so the field has stable semantics once real identities arrive.
func ActorFromRequest(r *http.Request) string {
	if r.Header.Get("X-API-Key") != "" || r.Header.Get("X-API-KEY") != "" {
		return "api-key"
	}
	return "anonymous"
}

// FromSystem returns an AuditEntry for events that didn't originate
// from an HTTP request — reconciler drift detection, deployer lifecycle
// completions. The actor is "system:<component>" so operators can
// distinguish these from user-initiated actions.
func FromSystem(component, op string) store.AuditEntry {
	return store.AuditEntry{
		Operation: op,
		Actor:     "system:" + component,
		Outcome:   store.AuditOutcomeSuccess,
	}
}

// remoteAddr extracts the client address, preferring X-Forwarded-For
// when set (common behind proxies). The field is best-effort — we never
// block a request because we couldn't parse an IP.
func remoteAddr(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}

// requestIDFromContext mirrors logctx's request_id field. Kept local so
// the audit package doesn't need to know logctx's implementation.
func requestIDFromContext(ctx context.Context) string {
	// logctx.FromContext returns a *logrus.Entry whose Data includes
	// the request_id we set in RequestID middleware.
	e := logctx.FromContext(ctx)
	if v, ok := e.Data["request_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// generateID returns 16 random hex bytes — matches the pattern used
// for stack / deployment IDs elsewhere in the codebase.
func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NoopRecorder discards all entries. Useful when audit isn't configured
// (e.g. early startup, tests that don't care about audit).
type NoopRecorder struct{}

// Record implements Recorder by discarding the entry.
func (NoopRecorder) Record(context.Context, store.AuditEntry) error { return nil }
