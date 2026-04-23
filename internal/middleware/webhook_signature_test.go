package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWebhookSecret = "shhh-keep-it-secret"

// signRequest returns the GitHub-format X-Hub-Signature-256 value for
// body under the given HMAC key. Tests use this to assemble valid
// signatures; to produce an invalid signature, flip a byte afterwards.
func signRequest(t *testing.T, secret, body string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(body))
	require.NoError(t, err)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// bodyCapturingHandler reads the full request body when invoked. The
// middleware has to restore the body after reading it for HMAC
// verification; without this the wrapped handler would see an empty
// body and the legacy /webhook handler would return the wrong stack.
type bodyCapturingHandler struct {
	got    []byte
	called bool
}

func (h *bodyCapturingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	body, _ := io.ReadAll(r.Body)
	h.got = body
	w.WriteHeader(http.StatusOK)
}

func TestWebhookSignature_Disabled_PassesThrough(t *testing.T) {
	// Empty secret is the "feature off" signal. Returning an identity
	// middleware matters: we don't want the legacy /webhook route to
	// suddenly start 401-ing on existing deployments that haven't set
	// WEBHOOK_SECRET.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature("")(h)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{"stack":"web"}`))
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, h.called)
	assert.Equal(t, `{"stack":"web"}`, string(h.got))
}

func TestWebhookSignature_ValidSignature_PassesThroughAndPreservesBody(t *testing.T) {
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	body := `{"stack":"web","commit":"abc123"}`
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set(webhookSignatureHeader, signRequest(t, testWebhookSecret, body))

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, h.called, "handler must be invoked once the signature validates")
	assert.Equal(t, body, string(h.got), "body must be restored for the handler to re-read")
}

func TestWebhookSignature_MissingHeader_401(t *testing.T) {
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	require.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.False(t, h.called, "handler must not be invoked on rejection")
	assert.Contains(t, rr.Body.String(), "invalid webhook signature")
}

func TestWebhookSignature_WrongAlgorithmPrefix_401(t *testing.T) {
	// Accept only the explicit `sha256=` prefix. GitHub's legacy
	// `sha1=` header, or a bare hex digest, both must be rejected so
	// we never drop to a weaker algorithm by accident.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	req.Header.Set(webhookSignatureHeader, "sha1=deadbeef")

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.False(t, h.called)
}

func TestWebhookSignature_MalformedHex_401(t *testing.T) {
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	req.Header.Set(webhookSignatureHeader, "sha256=not-hex-at-all")

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWebhookSignature_WrongDigestLength_401(t *testing.T) {
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	// Valid hex but 8 bytes instead of 32.
	req.Header.Set(webhookSignatureHeader, "sha256=deadbeefdeadbeef")

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWebhookSignature_TamperedBody_401(t *testing.T) {
	// Sign over `body`, send `tampered` — HMAC must catch the
	// difference. This is the core security property.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	body := `{"stack":"web","force":false}`
	tampered := `{"stack":"web","force":true}` // attacker flips a bool

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(tampered))
	req.Header.Set(webhookSignatureHeader, signRequest(t, testWebhookSecret, body))

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.False(t, h.called)
}

func TestWebhookSignature_WrongSecret_401(t *testing.T) {
	// Attacker guesses a secret; HMAC comparison fails.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	body := `{"stack":"web"}`
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set(webhookSignatureHeader, signRequest(t, "wrong-secret", body))

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWebhookSignature_EmptyBody_PassesWithValidSignatureOverEmpty(t *testing.T) {
	// Some senders (healthchecks, "ping" events) POST with an empty
	// body. A valid HMAC over the empty string must still pass.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(nil))
	req.Header.Set(webhookSignatureHeader, signRequest(t, testWebhookSecret, ""))

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, h.called)
	assert.Empty(t, h.got)
}

func TestWebhookSignature_OversizedBody_413(t *testing.T) {
	// A 2 MiB body exceeds the 1 MiB cap. Reject with 413 rather
	// than spending the RAM to compute an HMAC over it.
	h := &bodyCapturingHandler{}
	mw := NewWebhookSignature(testWebhookSecret)(h)

	big := strings.Repeat("x", 2*1024*1024)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(big))
	req.Header.Set(webhookSignatureHeader, signRequest(t, testWebhookSecret, big))

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	assert.False(t, h.called)
}
