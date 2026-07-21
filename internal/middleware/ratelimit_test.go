package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopHandler is the "real" handler we wrap — counts calls so tests can
// distinguish "middleware 429'd before reaching next" from "middleware
// passed through to the handler".
type noopHandler struct{ calls int }

func (h *noopHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls++
	w.WriteHeader(http.StatusOK)
}

// newReq returns a request with a fixed API key set.
func newReq(key string) *http.Request {
	r := httptest.NewRequest("GET", "/api/v1/ping", nil)
	r.Header.Set("X-API-KEY", key)
	return r
}

func TestRateLimit_Disabled_PassesEverythingThrough(t *testing.T) {
	// rps<=0 must return an identity middleware — no bucket, no state,
	// nothing that could be a hot-path cost on deployments that haven't
	// opted in.
	h := &noopHandler{}
	mw := NewRateLimit(0, 0)(h)

	for i := 0; i < 1000; i++ {
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, newReq("alice"))
		require.Equal(t, http.StatusOK, rr.Code)
	}
	assert.Equal(t, 1000, h.calls)
}

func TestRateLimit_AllowsBurstThenBlocks(t *testing.T) {
	// burst=3 with a slow refill: first three should pass, the fourth
	// lands in the empty-bucket branch and must 429 with Retry-After.
	h := &noopHandler{}
	mw := NewRateLimit(1, 3)(h) // 1 token/sec, capacity 3

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, newReq("alice"))
		require.Equalf(t, http.StatusOK, rr.Code, "request %d should have passed", i+1)
	}

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, newReq("alice"))
	require.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, 3, h.calls, "only the first 3 reached the handler")

	retry := rr.Header().Get("Retry-After")
	require.NotEmpty(t, retry)
	n, err := strconv.Atoi(retry)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "rate limit exceeded", body["error"])
	assert.NotEmpty(t, body["retry_after"])
}

func TestRateLimit_PerKeyIsolated(t *testing.T) {
	// Two keys, independent buckets. Burning one must not charge the other.
	h := &noopHandler{}
	mw := NewRateLimit(0.01, 2)(h) // near-zero refill isolates the test

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, newReq("alice"))
		require.Equal(t, http.StatusOK, rr.Code)
	}
	// alice's bucket is now empty.
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, newReq("alice"))
	require.Equal(t, http.StatusTooManyRequests, rr.Code)

	// bob's bucket is still full.
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, newReq("bob"))
		require.Equal(t, http.StatusOK, rr.Code)
	}
	// And bob can also be drained independently.
	rr = httptest.NewRecorder()
	mw.ServeHTTP(rr, newReq("bob"))
	require.Equal(t, http.StatusTooManyRequests, rr.Code)
}

func TestRateLimit_RefillsOverTime(t *testing.T) {
	// Drive the clock with an injected `now` so we assert the refill
	// math without wall-clock sleeps making the test flaky on loaded
	// CI runners. This tests the internal `allow()` directly — the
	// middleware wrapper is a thin shim around it.
	start := time.Unix(1_000_000, 0)
	current := start
	rl := &rateLimiter{
		rps:      10, // one token every 100ms
		capacity: 1,
		buckets:  map[string]*bucket{},
		now:      func() time.Time { return current },
	}

	ok, _ := rl.allow("alice")
	require.True(t, ok, "first request consumes the starting token")

	ok, retry := rl.allow("alice")
	require.False(t, ok, "immediate second request is throttled")
	assert.Greater(t, retry, time.Duration(0))
	assert.LessOrEqual(t, retry, 100*time.Millisecond,
		"retry-after should fit within a single refill window")

	current = start.Add(120 * time.Millisecond)
	ok, _ = rl.allow("alice")
	require.True(t, ok, "bucket refilled after >= 100ms")

	// Capacity cap: a long idle should not stack tokens beyond `capacity`.
	current = start.Add(10 * time.Second)
	ok, _ = rl.allow("alice")
	require.True(t, ok, "first draw after long idle")
	ok, _ = rl.allow("alice")
	require.False(t, ok, "second draw shows capacity was capped at 1, not stacked")
}

func TestRateLimit_NoKey_PassesThrough(t *testing.T) {
	// Callers without X-API-KEY never land here in production (auth
	// rejects first) — but if one does, we must NOT create a bucket
	// keyed on the empty string and let an unauthed caller starve the
	// single shared bucket.
	h := &noopHandler{}
	mw := NewRateLimit(1, 1)(h)

	for i := 0; i < 50; i++ {
		r := httptest.NewRequest("GET", "/api/v1/ping", nil)
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, r)
		require.Equal(t, http.StatusOK, rr.Code)
	}
	assert.Equal(t, 50, h.calls)
}

