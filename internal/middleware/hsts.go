package middleware

import (
	"net/http"
	"strconv"
)

// NewHSTS returns a middleware that sets the Strict-Transport-Security
// header on every response. Intended to be applied only when the
// server is actually serving TLS — over plain HTTP the header is
// either ignored by clients or actively misleading.
//
// maxAgeSeconds <= 0 yields an identity middleware so callers can wire
// the constructor unconditionally and let the config knob disable it
// (e.g. when Accelero sits behind a TLS-terminating proxy that already
// sets HSTS).
//
// We intentionally do NOT add `includeSubDomains` or `preload`. Those
// are host-level decisions an operator should make once for the whole
// origin (typically at the reverse proxy), not something a single
// service should impose on neighbours sharing the hostname.
func NewHSTS(maxAgeSeconds int) func(http.Handler) http.Handler {
	if maxAgeSeconds <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	headerValue := "max-age=" + strconv.Itoa(maxAgeSeconds)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Strict-Transport-Security", headerValue)
			next.ServeHTTP(w, r)
		})
	}
}
