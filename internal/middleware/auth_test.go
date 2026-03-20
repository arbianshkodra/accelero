package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
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
