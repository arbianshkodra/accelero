package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

const defaultCSP = "default-src 'none'; frame-ancestors 'none'"

func serveSecurityHeaders(t *testing.T, csp string) *httptest.ResponseRecorder {
	t.Helper()
	called := false
	mw := NewSecurityHeaders(csp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	assert.True(t, called, "next handler must be invoked")
	assert.Equal(t, http.StatusOK, rr.Code)
	return rr
}

func TestSecurityHeaders_CompanionsAlwaysSet(t *testing.T) {
	// nosniff / frame-options / referrer-policy have no downside for a
	// JSON API and are set regardless of the CSP value — including when
	// the CSP header is disabled.
	rr := serveSecurityHeaders(t, "")

	assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", rr.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", rr.Header().Get("Referrer-Policy"))
}

func TestSecurityHeaders_CSPSetWhenNonEmpty(t *testing.T) {
	rr := serveSecurityHeaders(t, defaultCSP)
	assert.Equal(t, defaultCSP, rr.Header().Get("Content-Security-Policy"))
}

func TestSecurityHeaders_CSPOmittedWhenEmpty(t *testing.T) {
	// Empty CSP => no Content-Security-Policy header, so operators can
	// let an edge proxy own the policy without also dropping the cheap
	// companion headers.
	rr := serveSecurityHeaders(t, "")
	assert.Empty(t, rr.Header().Get("Content-Security-Policy"))
}

func TestSecurityHeaders_CSPValueMatchesConfig(t *testing.T) {
	custom := "default-src 'self'; img-src 'self' data:"
	rr := serveSecurityHeaders(t, custom)
	assert.Equal(t, custom, rr.Header().Get("Content-Security-Policy"))
}
