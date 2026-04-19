package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/stretchr/testify/assert"
)

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	var captured string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = logctx.FromContext(r.Context()).Data["request_id"].(string)
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rr, req)

	// A new ID was generated and placed on both the context and the response.
	assert.NotEmpty(t, captured)
	assert.Equal(t, captured, rr.Header().Get(RequestIDHeader))
	assert.Len(t, captured, 16) // 8 bytes hex encoded
}

func TestRequestID_HonorsIncomingHeader(t *testing.T) {
	var captured string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = logctx.FromContext(r.Context()).Data["request_id"].(string)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "client-supplied-123")
	handler.ServeHTTP(rr, req)

	assert.Equal(t, "client-supplied-123", captured)
	assert.Equal(t, "client-supplied-123", rr.Header().Get(RequestIDHeader))
}

func TestRequestID_RejectsInvalidIncoming(t *testing.T) {
	// Injection attempts (CRLF, spaces, control chars) must be replaced
	// with a freshly generated ID.
	cases := []string{
		"has spaces",
		"has\nnewline",
		"has;semicolons",
		"has/slashes",
		"", // empty header is same as missing
	}

	for _, incoming := range cases {
		var captured string
		handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = logctx.FromContext(r.Context()).Data["request_id"].(string)
		}))

		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(RequestIDHeader, incoming)
		handler.ServeHTTP(rr, req)

		assert.NotEqual(t, incoming, captured, "should not accept %q verbatim", incoming)
		assert.Len(t, captured, 16, "generated ID should be 16 hex chars (got %q)", captured)
	}
}

func TestRequestID_RejectsOverlongHeader(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}

	var captured string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = logctx.FromContext(r.Context()).Data["request_id"].(string)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, string(long))
	handler.ServeHTTP(rr, req)

	assert.NotEqual(t, string(long), captured)
	assert.LessOrEqual(t, len(captured), 128)
}

func TestIsValidRequestID(t *testing.T) {
	cases := map[string]bool{
		"":                        false,
		"abc123":                  true,
		"abc-123_xyz":             true,
		"has space":               false,
		"has\tnewline":            false,
		"has\nnewline":            false,
		"has;special":             false,
		"a":                       true,
		string(make([]byte, 129)): false, // 129 zero bytes — too long AND invalid chars
	}

	for id, expected := range cases {
		assert.Equal(t, expected, isValidRequestID(id), "id=%q", id)
	}
}
