package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastPolicy retries without wall-clock sleeps so tests stay quick and
// deterministic (BaseDelay=0 => backoffDuration returns 0).
func fastPolicy(attempts int) Policy {
	return Policy{MaxAttempts: attempts, BaseDelay: 0, MaxDelay: 0}
}

func TestDo_SucceedsFirstTry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(3), "op", func(ctx context.Context) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "should not retry after success")
}

func TestDo_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(3), "op", func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestDo_ExhaustsBudgetReturnsLastError(t *testing.T) {
	calls := 0
	sentinel := errors.New("still broken")
	err := Do(context.Background(), fastPolicy(3), "op", func(ctx context.Context) error {
		calls++
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, 3, calls, "should try exactly MaxAttempts times")
}

func TestDo_MaxAttemptsBelowOneMeansSingleTry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(0), "op", func(ctx context.Context) error {
		calls++
		return errors.New("boom")
	})
	assert.Error(t, err)
	assert.Equal(t, 1, calls, "MaxAttempts<1 collapses to a single attempt")
}

func TestDo_PermanentStopsImmediately(t *testing.T) {
	calls := 0
	underlying := errors.New("bad credentials")
	err := Do(context.Background(), fastPolicy(5), "op", func(ctx context.Context) error {
		calls++
		return Permanent(underlying)
	})
	assert.Equal(t, 1, calls, "permanent error must not be retried")
	// The caller sees the original error, not the wrapper.
	assert.ErrorIs(t, err, underlying)
}

func TestDo_ContextCanceledErrorNotRetried(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(5), "op", func(ctx context.Context) error {
		calls++
		return context.Canceled
	})
	assert.Equal(t, 1, calls)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDo_StopsWhenContextCancelledBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, fastPolicy(5), "op", func(ctx context.Context) error {
		calls++
		cancel() // cancel after the first failure
		return errors.New("transient")
	})
	assert.Error(t, err)
	assert.Equal(t, 1, calls, "must not attempt again once ctx is cancelled")
}

func TestDo_CancelledContextBeforeFirstAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Do(ctx, fastPolicy(3), "op", func(ctx context.Context) error {
		calls++
		return nil
	})
	assert.Equal(t, 0, calls, "already-cancelled ctx short-circuits before running fn")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBackoffDuration_ZeroBaseIsZero(t *testing.T) {
	assert.Equal(t, time.Duration(0), backoffDuration(Policy{BaseDelay: 0}, 1))
}

func TestBackoffDuration_WithinCappedBounds(t *testing.T) {
	p := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 1 * time.Second}
	// Full jitter => result in [0, cap]. Verify the cap holds even as the
	// uncapped exponential (100ms<<n) blows past MaxDelay.
	for attempt := 1; attempt <= 10; attempt++ {
		d := backoffDuration(p, attempt)
		assert.GreaterOrEqual(t, d, time.Duration(0))
		assert.LessOrEqual(t, d, p.MaxDelay, "attempt %d exceeded MaxDelay", attempt)
	}
}
