package audit

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturingStore implements the minimal store.Store surface the recorder
// actually hits. The rest of the interface is no-op; we care only about
// the audit calls.
type capturingStore struct {
	store.Store
	created  []*store.AuditEntry
	createErr error
}

func (s *capturingStore) CreateAuditEntry(e *store.AuditEntry) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.created = append(s.created, e)
	return nil
}

func TestStoreRecorder_FillsIDAndTimestamp(t *testing.T) {
	s := &capturingStore{}
	r := NewStoreRecorder(s)

	err := r.Record(context.Background(), store.AuditEntry{
		Operation: store.AuditOpStackCreate,
		Actor:     "api-key",
		Outcome:   store.AuditOutcomeSuccess,
	})
	require.NoError(t, err)
	require.Len(t, s.created, 1)

	got := s.created[0]
	assert.NotEmpty(t, got.ID, "ID must be populated by the recorder")
	assert.Len(t, got.ID, 32, "generated IDs are 16 bytes of hex = 32 chars")
	assert.False(t, got.Timestamp.IsZero(), "timestamp must be populated by the recorder")
	assert.WithinDuration(t, time.Now(), got.Timestamp, 2*time.Second)
}

func TestStoreRecorder_PreservesCallerFields(t *testing.T) {
	s := &capturingStore{}
	r := NewStoreRecorder(s)

	fixedID := "deadbeefcafebabe"
	fixedTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	err := r.Record(context.Background(), store.AuditEntry{
		ID:        fixedID,
		Timestamp: fixedTime,
		Operation: store.AuditOpStackDelete,
		Actor:     "system:test",
	})
	require.NoError(t, err)

	got := s.created[0]
	assert.Equal(t, fixedID, got.ID, "caller-provided ID must be preserved")
	assert.True(t, got.Timestamp.Equal(fixedTime), "caller-provided timestamp must be preserved")
}

func TestStoreRecorder_WriteFailureReturnsError(t *testing.T) {
	// Policy: the recorder logs-and-returns-the-error so callers can log
	// it in context. Callers must NOT let it fail their business path.
	s := &capturingStore{createErr: errors.New("disk full")}
	r := NewStoreRecorder(s)

	err := r.Record(context.Background(), store.AuditEntry{
		Operation: store.AuditOpStackCreate,
		Actor:     "api-key",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk full")
}

func TestFromRequest_PicksUpContextFields(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/stacks", nil)
	req.Header.Set("X-API-Key", "super-secret")
	req.RemoteAddr = "10.0.0.1:55555"

	// Put a request_id on the context the way RequestID middleware does.
	req = req.WithContext(logctx.WithField(req.Context(), "request_id", "req-123"))

	entry := FromRequest(req, store.AuditOpStackCreate)
	assert.Equal(t, "stack.create", entry.Operation)
	assert.Equal(t, "api-key", entry.Actor, "presence of X-API-Key → actor=api-key")
	assert.Equal(t, "10.0.0.1:55555", entry.RemoteAddr)
	assert.Equal(t, "req-123", entry.RequestID)
	assert.Equal(t, "success", entry.Outcome, "default outcome is success; caller overrides")
}

func TestFromRequest_AnonymousWithoutAPIKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/stacks", nil)
	entry := FromRequest(req, store.AuditOpStackCreate)
	assert.Equal(t, "anonymous", entry.Actor)
}

func TestFromRequest_PrefersXForwardedFor(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-API-Key", "k")
	req.Header.Set("X-Forwarded-For", "203.0.113.4")
	req.RemoteAddr = "10.0.0.1:55555"

	entry := FromRequest(req, store.AuditOpStackCreate)
	assert.Equal(t, "203.0.113.4", entry.RemoteAddr,
		"proxied IP via X-Forwarded-For beats RemoteAddr")
}

func TestFromSystem_StampsActor(t *testing.T) {
	entry := FromSystem("reconciler", store.AuditOpDriftDetected)
	assert.Equal(t, "system:reconciler", entry.Actor)
	assert.Equal(t, "drift.detected", entry.Operation)
	assert.Equal(t, "success", entry.Outcome)
}

func TestNoopRecorder_DiscardsSilently(t *testing.T) {
	var r Recorder = NoopRecorder{}
	assert.NoError(t, r.Record(context.Background(), store.AuditEntry{Operation: "anything"}))
}
