package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHSTS_Disabled_NoHeaderAdded(t *testing.T) {
	// maxAge<=0 returns an identity middleware. No header lands on
	// the response — important when Accelero is fronted by a proxy
	// that's setting HSTS at the edge and we don't want a double-up.
	called := false
	mw := NewHSTS(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, called)
	assert.Empty(t, rr.Header().Get("Strict-Transport-Security"))
}

func TestHSTS_Enabled_SetsHeader(t *testing.T) {
	mw := NewHSTS(31536000)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	assert.Equal(t, "max-age=31536000", rr.Header().Get("Strict-Transport-Security"))
}

func TestHSTS_HeaderValueMatchesConfig(t *testing.T) {
	// Operator can tune the lifetime via TLS_HSTS_MAX_AGE — confirm
	// the value lands verbatim in the header.
	mw := NewHSTS(60)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	assert.Equal(t, "max-age=60", rr.Header().Get("Strict-Transport-Security"))
}

func TestHSTS_NoIncludeSubDomainsOrPreload(t *testing.T) {
	// includeSubDomains and preload are host-level decisions an
	// operator should make at the edge. Accelero must not set them
	// unilaterally — they affect siblings on the same origin and are
	// effectively un-undoable for `preload`.
	mw := NewHSTS(86400)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	hsts := rr.Header().Get("Strict-Transport-Security")
	assert.NotContains(t, hsts, "includeSubDomains")
	assert.NotContains(t, hsts, "preload")
}
