package middleware

import (
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
)

func APIKeyAuth(next http.Handler) http.Handler {
	apiKey := os.Getenv("API_KEY")

	// If API_KEY is not set, return the original handler (no authentication)
	if apiKey == "" {
		logrus.Info("API_KEY not set; authentication disabled")
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get API key from the request header
		requestAPIKey := r.Header.Get("X-API-KEY")

		if requestAPIKey == "" {
			http.Error(w, "API key is required", http.StatusUnauthorized)
			return
		}

		if requestAPIKey != apiKey {
			http.Error(w, "Invalid API key", http.StatusForbidden)
			return
		}

		// Proceed to the next handler
		next.ServeHTTP(w, r)
	})
}
