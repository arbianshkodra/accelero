// Package retry provides bounded exponential-backoff retries for the
// transient failures that dog GitOps deploys — image pulls, git clones,
// and Docker network operations all fail intermittently on flaky
// networks or briefly-unavailable registries, and a single retry with
// backoff turns most of those into successes instead of failed deploys.
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/arbianshkodra/accelero/internal/logctx"
)

// Policy configures a bounded exponential-backoff retry.
type Policy struct {
	// MaxAttempts is the total number of tries, including the first.
	// A value <=1 disables retrying (exactly one attempt).
	MaxAttempts int
	// BaseDelay is the backoff before the second attempt; it doubles
	// with each subsequent attempt. Zero means retry immediately (used
	// by tests to avoid wall-clock sleeps).
	BaseDelay time.Duration
	// MaxDelay caps any single backoff sleep so exponential growth
	// can't produce absurd waits on high attempt counts.
	MaxDelay time.Duration
}

// permanent wraps an error to tell Do it must not be retried.
type permanent struct{ err error }

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent marks err as non-retryable, so Do returns it immediately
// instead of burning the remaining attempt budget. Use it for failures
// that a retry cannot fix — bad credentials, a missing repository, an
// image reference that doesn't exist. Do unwraps the marker, so callers
// still see (and can errors.Is/As) the original error.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanent{err: err}
}

// Do runs fn, retrying transient failures with exponential backoff and
// full jitter. It stops — returning the operation's error — when fn
// succeeds, the attempt budget is spent, the error is marked Permanent,
// or ctx is cancelled. op is a short human label used only for the
// retry log line ("git clone https://...", "image pull nginx:1.27").
//
// Cancellation is honoured both between attempts (the backoff sleep is
// interruptible) and via any context error fn surfaces — a fn that
// returns something wrapping context.Canceled/DeadlineExceeded is not
// retried, since the caller has already given up.
func Do(ctx context.Context, p Policy, op string, fn func(ctx context.Context) error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// If the caller cancelled before we could (re)try, don't bother.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				return lastErr
			}
			return ctxErr
		}

		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}

		// Errors that a retry cannot fix stop the loop immediately.
		var perm *permanent
		if errors.As(lastErr, &perm) {
			return perm.err
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}

		// No point sleeping after the final attempt.
		if attempt == attempts {
			break
		}

		delay := backoffDuration(p, attempt)
		logctx.FromContext(ctx).WithError(lastErr).Warnf(
			"%s failed (attempt %d/%d), retrying in %s", op, attempt, attempts, delay.Round(time.Millisecond))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
	}

	return lastErr
}

// backoffDuration returns the (jittered) sleep before the retry that
// follows the given 1-based attempt number. It uses "full jitter":
// a uniform random draw in [0, cappedBackoff], which spreads retries
// from many concurrent deploys so they don't thundering-herd a
// recovering registry.
func backoffDuration(p Policy, attempt int) time.Duration {
	if p.BaseDelay <= 0 {
		return 0
	}
	// base * 2^(attempt-1), computed in float to dodge shift overflow
	// at high attempt counts, then capped.
	capped := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if p.MaxDelay > 0 && capped > float64(p.MaxDelay) {
		capped = float64(p.MaxDelay)
	}
	if capped <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(capped) + 1))
}
