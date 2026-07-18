package middleware

import "net/http"

// NewSecurityHeaders returns a middleware that sets a small set of
// defensive HTTP response headers on every response.
//
// Three headers are always set — they have no downside for a JSON API
// and are the standard companions to a Content-Security-Policy:
//   - X-Content-Type-Options: nosniff — stops a browser second-guessing
//     our declared Content-Type (e.g. rendering a JSON error body as
//     HTML/JS).
//   - X-Frame-Options: DENY — clickjacking defence for any HTML surface.
//     Superseded by CSP's frame-ancestors but honoured by older clients
//     that ignore CSP.
//   - Referrer-Policy: no-referrer — Accelero URLs carry stack ids and
//     names; don't leak them to third parties via the Referer header.
//
// The Content-Security-Policy header is set only when csp is non-empty,
// so an operator can omit it (CONTENT_SECURITY_POLICY="") when an edge
// proxy sets its own, without losing the always-on companions above.
// The default policy is deliberately locked down (default-src 'none')
// because Accelero serves no browser UI today — nothing legitimate
// needs to load. Revisit when a UI lands.
//
// Unlike HSTS, these headers are valid over both HTTP and HTTPS, so the
// middleware is applied unconditionally rather than being gated on TLS.
func NewSecurityHeaders(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			if csp != "" {
				h.Set("Content-Security-Policy", csp)
			}
			next.ServeHTTP(w, r)
		})
	}
}
