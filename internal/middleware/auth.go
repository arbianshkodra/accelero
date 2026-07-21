package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/sirupsen/logrus"
)

func APIKeyAuth(next http.Handler) http.Handler {
	apiKey := os.Getenv("API_KEY")

	// If API_KEY is not set, fail securely (require authentication)
	if apiKey == "" {
		logrus.Fatal("API_KEY environment variable must be set for security")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pull the request-scoped logger so security events carry
		// request_id (and whatever else is attached upstream). RequestID
		// middleware runs before APIKeyAuth, so the field is always set.
		log := logctx.FromContext(r.Context()).WithFields(logrus.Fields{
			"path":        r.URL.Path,
			"method":      r.Method,
			"remote_addr": r.RemoteAddr,
		})

		requestAPIKey := r.Header.Get("X-API-KEY")

		if requestAPIKey == "" {
			log.Warn("Unauthorized request: missing API key")
			http.Error(w, "API key is required", http.StatusUnauthorized)
			return
		}

		// Constant-time comparison prevents timing attacks.
		if subtle.ConstantTimeCompare([]byte(requestAPIKey), []byte(apiKey)) != 1 {
			log.Warn("Unauthorized request: invalid API key")
			http.Error(w, "Invalid API key", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}
