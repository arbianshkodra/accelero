package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuth_ValidKey(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := APIKeyAuth(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-KEY", "test-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, handlerCalled, "expected inner handler to be called")
}

func TestAPIKeyAuth_MissingKey(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called")
	})

	handler := APIKeyAuth(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "API key is required")
}

func TestAPIKeyAuth_WrongKey(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called")
	})

	handler := APIKeyAuth(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-KEY", "wrong-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid API key")
}

// TestAPIKeyAuth_UnauthorizedLogsRequestID verifies that the unauthorized
// warn entries carry contextual fields from the request context — in
// particular request_id, which is attached upstream by RequestID middleware.
// This is the fix for the Phase 2 "structured ID propagation" gap: security
// events were previously logged bare.
func TestAPIKeyAuth_UnauthorizedLogsRequestID(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	handler := APIKeyAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called on unauth")
	}))

	// Simulate what the RequestID middleware does upstream: stick a
	// request_id onto the context before APIKeyAuth sees the request.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stacks", nil)
	ctx := logctx.WithField(req.Context(), "request_id", "req-abc-123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.NotEmpty(t, hook.Entries, "expected a log entry from the unauthorized path")
	entry := hook.LastEntry()
	assert.Equal(t, logrus.WarnLevel, entry.Level)
	assert.Equal(t, "req-abc-123", entry.Data["request_id"])
	assert.Equal(t, "/api/v1/stacks", entry.Data["path"])
	assert.Equal(t, "POST", entry.Data["method"])
	assert.Contains(t, entry.Message, "missing API key")
}

