package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/sirupsen/logrus"
)

// RequestIDHeader is the canonical header name used to receive and return
// request identifiers.
const RequestIDHeader = "X-Request-ID"

// RequestID generates a new request ID (or honours an incoming
// X-Request-ID header), attaches it to the request context so downstream
// log lines are tagged, and echoes it back on the response header so
// callers can correlate requests across client/server boundaries.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || !isValidRequestID(id) {
			id = generateRequestID()
		}
		w.Header().Set(RequestIDHeader, id)

		ctx := logctx.WithFields(r.Context(), logrus.Fields{"request_id": id})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateRequestID produces a short, URL-safe random ID (16 hex chars).
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// isValidRequestID enforces a conservative whitelist on incoming request
// IDs to avoid log injection.  Accepts ASCII alphanumerics, '-', '_', and
// a reasonable length cap.
func isValidRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
