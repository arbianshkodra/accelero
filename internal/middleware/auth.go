package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
)

func APIKeyAuth(next http.Handler) http.Handler {
	apiKey := os.Getenv("API_KEY")

	// If API_KEY is not set, fail securely (require authentication)
	if apiKey == "" {
		logrus.Fatal("API_KEY environment variable must be set for security")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get API key from the request header
		requestAPIKey := r.Header.Get("X-API-KEY")

		if requestAPIKey == "" {
			logrus.Warn("Unauthorized request: missing API key")
			http.Error(w, "API key is required", http.StatusUnauthorized)
			return
		}

		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(requestAPIKey), []byte(apiKey)) != 1 {
			logrus.Warn("Unauthorized request: invalid API key")
			http.Error(w, "Invalid API key", http.StatusForbidden)
			return
		}

		// Proceed to the next handler
		next.ServeHTTP(w, r)
	})
}
