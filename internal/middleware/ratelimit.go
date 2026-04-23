package middleware

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/metrics"
)

// NewRateLimit returns a per-API-key token-bucket middleware. The returned
// middleware must be applied AFTER APIKeyAuth: the bucket is keyed off the
// validated X-API-KEY header, and we want invalid keys rejected before they
// can materialise a bucket entry (otherwise an attacker could grow the map
// by sending randomly-generated header values).
//
//   rps   — sustained refill rate in tokens/second. <=0 disables the
//           limiter (returns an identity middleware).
//   burst — bucket capacity (how many requests a quiescent caller can send
//           at once before being throttled). Clamped to >=1 when enabled.
//
// On exhaustion the response is 429 Too Many Requests with a conservative
// `Retry-After` header (seconds, ceil'd to at least 1) and a JSON body
// matching the rest of the API's error shape.
func NewRateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	if rps <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	if burst < 1 {
		burst = 1
	}
	rl := &rateLimiter{
		rps:      rps,
		capacity: float64(burst),
		buckets:  map[string]*bucket{},
		now:      time.Now,
	}
	return rl.middleware
}

// rateLimiter holds the per-key token buckets. The map is guarded by a
// single mutex rather than sync.Map because inserts (new keys) are rare
// after warm-up and the critical section is a fast hash lookup.
type rateLimiter struct {
	rps      float64
	capacity float64

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // injection point for tests
}

type bucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// bucketFor returns the bucket for key, creating a full one lazily.
func (rl *rateLimiter) bucketFor(key string) *bucket {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if b, ok := rl.buckets[key]; ok {
		return b
	}
	b := &bucket{tokens: rl.capacity, lastRefill: rl.now()}
	rl.buckets[key] = b
	return b
}

// allow deducts one token from the key's bucket and reports whether the
// caller may proceed. When refused, returns the duration until the bucket
// will hold one token again (for Retry-After).
func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
	b := rl.bucketFor(key)
	b.mu.Lock()
	defer b.mu.Unlock()

	now := rl.now()
	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rl.rps
		if b.tokens > rl.capacity {
			b.tokens = rl.capacity
		}
		b.lastRefill = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	needed := 1 - b.tokens
	return false, time.Duration(float64(time.Second) * needed / rl.rps)
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-KEY")
		if key == "" {
			// No key → auth layer should have rejected already. Pass
			// through so unauth endpoints mounted under this chain
			// (shouldn't happen, but be safe) don't 429.
			next.ServeHTTP(w, r)
			return
		}

		if ok, retry := rl.allow(key); ok {
			next.ServeHTTP(w, r)
			return
		} else {
			rejectTooManyRequests(w, r, retry)
		}
	})
}

func rejectTooManyRequests(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":       "rate limit exceeded",
		"retry_after": strconv.Itoa(seconds) + "s",
	})

	metrics.IncRateLimited(routeTemplate(r))
	logctx.FromContext(r.Context()).
		WithField("retry_after_seconds", seconds).
		Warn("rate limit exceeded")
}
