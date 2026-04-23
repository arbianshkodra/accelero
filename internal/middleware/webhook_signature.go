package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/metrics"
)

// webhookSignatureHeader is the canonical name for the HMAC-SHA256
// signature. We adopt GitHub's `X-Hub-Signature-256` wire format because
// GitHub, Gitea, Gogs, and most CI systems already emit it; any other
// sender can be pointed at the endpoint with a one-liner in front of it.
const (
	webhookSignatureHeader = "X-Hub-Signature-256"
	webhookSignaturePrefix = "sha256="
)

// maxWebhookBodyBytes caps how much we'll read for signature verification.
// External senders rarely go above 64 KB (a push event from a busy repo
// is still well under that); allowing unbounded bodies would be a trivial
// DoS vector at this layer because we have to buffer the entire body to
// compute the HMAC.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// NewWebhookSignature returns a middleware that verifies an HMAC-SHA256
// signature over the raw request body. secret is the shared key; an
// empty secret yields an identity middleware so deployments that haven't
// opted in retain the pre-existing API-key-only auth path.
//
// Success: the body is restored on the request so the wrapped handler
// can parse it normally — we have to read the whole body up front to
// compute the MAC, so buffering is unavoidable.
//
// Failure modes all return 401 Unauthorized with a generic error body;
// we never tell the caller *why* (missing header vs wrong MAC vs bad
// hex) since that helps attackers tune. Rejections bump the
// accelero_webhook_signature_rejected_total metric.
func NewWebhookSignature(secret string) func(http.Handler) http.Handler {
	if secret == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	key := []byte(secret)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := strings.TrimSpace(r.Header.Get(webhookSignatureHeader))
			if !strings.HasPrefix(header, webhookSignaturePrefix) {
				rejectSignature(w, r, "missing_or_malformed_header")
				return
			}
			got, err := hex.DecodeString(strings.TrimPrefix(header, webhookSignaturePrefix))
			if err != nil || len(got) != sha256.Size {
				rejectSignature(w, r, "malformed_signature")
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
			if err != nil {
				// MaxBytesReader returns an error when the cap is
				// exceeded; that's a 413 — not a signature issue —
				// but it lands here because the handler hasn't run.
				http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
				return
			}

			mac := hmac.New(sha256.New, key)
			_, _ = mac.Write(body)
			want := mac.Sum(nil)

			if !hmac.Equal(got, want) {
				rejectSignature(w, r, "hmac_mismatch")
				return
			}

			// Restore the body for the downstream handler. A fresh
			// NopCloser around a Reader on the buffered bytes is the
			// idiomatic restore.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			next.ServeHTTP(w, r)
		})
	}
}

// rejectSignature writes the 401 + error JSON, records the metric, and
// logs at warn. reason is a short tag that only appears in logs/metrics
// — never in the response — so we don't leak signal to an attacker
// tuning their forgeries.
func rejectSignature(w http.ResponseWriter, r *http.Request, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "invalid webhook signature",
	})
	metrics.IncWebhookSignatureRejected(reason)
	logctx.FromContext(r.Context()).
		WithField("reason", reason).
		WithField("remote_addr", r.RemoteAddr).
		Warn("webhook signature rejected")
}
