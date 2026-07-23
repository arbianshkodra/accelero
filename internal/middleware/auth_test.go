package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/rbac"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookupFor returns a KeyLookup that resolves the given raw keys to identities.
func lookupFor(keys map[string]rbac.Identity) KeyLookup {
	return func(raw string) (rbac.Identity, bool, error) {
		id, ok := keys[raw]
		return id, ok, nil
	}
}

func serveAuth(t *testing.T, auth func(http.Handler) http.Handler, method, path, key string) (*httptest.ResponseRecorder, *rbac.Identity) {
	t.Helper()
	var captured *rbac.Identity
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := rbac.IdentityFromContext(r.Context()); ok {
			captured = &id
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("X-API-KEY", key)
	}
	rec := httptest.NewRecorder()
	auth(inner).ServeHTTP(rec, req)
	return rec, captured
}

func TestAuth_EnvKeyIsAdmin(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(nil))
	rec, id := serveAuth(t, auth, http.MethodPost, "/api/v1/admin/backup", "env-secret")
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, id)
	assert.Equal(t, "env-admin", id.Name)
	assert.Equal(t, rbac.RoleAdmin, id.Role)
}

func TestAuth_MissingKey401(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(nil))
	rec, _ := serveAuth(t, auth, http.MethodGet, "/api/v1/stacks", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "API key is required")
}

func TestAuth_UnknownKey403(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(nil))
	rec, _ := serveAuth(t, auth, http.MethodGet, "/api/v1/stacks", "nope")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid API key")
}

func TestAuth_ViewerRoleEnforcement(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(map[string]rbac.Identity{
		"vkey": {Name: "reader", Role: rbac.RoleViewer},
	}))
	// Viewer can read.
	rec, id := serveAuth(t, auth, http.MethodGet, "/api/v1/stacks", "vkey")
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, id)
	assert.Equal(t, "reader", id.Name)
	// Viewer cannot mutate.
	rec, _ = serveAuth(t, auth, http.MethodPost, "/api/v1/stacks/x/deploy", "vkey")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "insufficient role")
}

func TestAuth_OperatorRoleEnforcement(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(map[string]rbac.Identity{
		"okey": {Name: "ops", Role: rbac.RoleOperator},
	}))
	// Operator can deploy.
	rec, _ := serveAuth(t, auth, http.MethodPost, "/api/v1/stacks/x/deploy", "okey")
	assert.Equal(t, http.StatusOK, rec.Code)
	// Operator cannot touch admin endpoints or key management.
	rec, _ = serveAuth(t, auth, http.MethodPost, "/api/v1/admin/restore", "okey")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec, _ = serveAuth(t, auth, http.MethodPost, "/api/v1/apikeys", "okey")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAuth_ExecRequiresOperator(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", lookupFor(map[string]rbac.Identity{
		"vkey": {Name: "reader", Role: rbac.RoleViewer},
		"okey": {Name: "ops", Role: rbac.RoleOperator},
	}))
	// exec is a mutating GET — viewer is denied, operator allowed.
	rec, _ := serveAuth(t, auth, http.MethodGet, "/api/v1/stacks/x/containers/c/exec", "vkey")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec, _ = serveAuth(t, auth, http.MethodGet, "/api/v1/stacks/x/containers/c/exec", "okey")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_LookupErrorIs500(t *testing.T) {
	auth := NewAPIKeyAuth("env-secret", func(raw string) (rbac.Identity, bool, error) {
		return rbac.Identity{}, false, assertErr{}
	})
	rec, _ := serveAuth(t, auth, http.MethodGet, "/api/v1/stacks", "anything")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }

func TestAuth_UnauthorizedLogsRequestID(t *testing.T) {
	hook := logrustest.NewGlobal()
	defer hook.Reset()

	auth := NewAPIKeyAuth("test-key", lookupFor(nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called on unauth")
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stacks", nil)
	req = req.WithContext(logctx.WithField(req.Context(), "request_id", "req-abc-123"))
	rec := httptest.NewRecorder()

	auth(inner).ServeHTTP(rec, req)

	require.NotEmpty(t, hook.Entries)
	entry := hook.LastEntry()
	assert.Equal(t, logrus.WarnLevel, entry.Level)
	assert.Equal(t, "req-abc-123", entry.Data["request_id"])
	assert.Equal(t, "/api/v1/stacks", entry.Data["path"])
	assert.Equal(t, "POST", entry.Data["method"])
	assert.Contains(t, entry.Message, "missing API key")
}
