package breaker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// clocked returns a breaker whose clock is driven by the returned pointer,
// so tests advance time deterministically without sleeping.
func clocked(threshold int, cooldown time.Duration) (*Breaker, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	b := New(threshold, cooldown)
	b.now = func() time.Time { return now }
	return b, &now
}

func TestDisabled_AlwaysAllowsAndNoOps(t *testing.T) {
	b := New(0, time.Minute)
	assert.False(t, b.Enabled())
	for i := 0; i < 100; i++ {
		assert.True(t, b.Allow("s"))
		b.RecordFailure("s") // must not trip when disabled
	}
	assert.True(t, b.Allow("s"))
	assert.Equal(t, StateClosed, b.State("s"))
}

func TestNilBreaker_IsDisabled(t *testing.T) {
	var b *Breaker
	assert.False(t, b.Enabled())
	assert.True(t, b.Allow("s"))
	assert.False(t, b.RecordFailure("s"))
	b.RecordSuccess("s") // must not panic
	assert.Equal(t, StateClosed, b.State("s"))
	assert.Equal(t, 0, b.Threshold())
}

func TestTripsAfterThresholdFailures(t *testing.T) {
	b, _ := clocked(3, time.Minute)

	assert.False(t, b.RecordFailure("s"))
	assert.False(t, b.RecordFailure("s"))
	assert.Equal(t, StateClosed, b.State("s"))
	assert.True(t, b.Allow("s"), "still closed before threshold")

	// Third consecutive failure trips it.
	assert.True(t, b.RecordFailure("s"), "threshold reached should report justTripped")
	assert.Equal(t, StateOpen, b.State("s"))
	assert.False(t, b.Allow("s"), "open breaker blocks calls")
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b, _ := clocked(3, time.Minute)
	b.RecordFailure("s")
	b.RecordFailure("s")
	b.RecordSuccess("s") // clears the streak
	assert.False(t, b.RecordFailure("s"), "count restarts after success")
	assert.False(t, b.RecordFailure("s"))
	assert.Equal(t, StateClosed, b.State("s"))
	assert.True(t, b.RecordFailure("s"), "third fresh failure trips")
}

func TestHalfOpenTrial_SuccessCloses(t *testing.T) {
	b, now := clocked(2, time.Minute)
	b.RecordFailure("s")
	assert.True(t, b.RecordFailure("s")) // trips open
	assert.False(t, b.Allow("s"))

	*now = now.Add(time.Minute) // cooldown elapses
	assert.True(t, b.Allow("s"), "cooldown elapsed → half-open trial allowed")
	assert.Equal(t, StateHalfOpen, b.State("s"))

	b.RecordSuccess("s")
	assert.Equal(t, StateClosed, b.State("s"))
	assert.True(t, b.Allow("s"))
}

func TestHalfOpenTrial_FailureReopensWithoutRetrip(t *testing.T) {
	b, now := clocked(2, time.Minute)
	b.RecordFailure("s")
	assert.True(t, b.RecordFailure("s")) // trips

	*now = now.Add(time.Minute)
	assert.True(t, b.Allow("s")) // half-open
	// Trial fails: reopens, but does NOT report justTripped (avoids audit spam).
	assert.False(t, b.RecordFailure("s"))
	assert.Equal(t, StateOpen, b.State("s"))
	assert.False(t, b.Allow("s"), "blocked again until next cooldown")

	*now = now.Add(time.Minute)
	assert.True(t, b.Allow("s"), "cooldown restarted from the failed trial")
}

func TestOpenBlocksUntilCooldownElapses(t *testing.T) {
	b, now := clocked(1, 30*time.Second)
	assert.True(t, b.RecordFailure("s")) // threshold 1 → trips immediately

	*now = now.Add(29 * time.Second)
	assert.False(t, b.Allow("s"), "still within cooldown")

	*now = now.Add(1 * time.Second) // now exactly 30s
	assert.True(t, b.Allow("s"), "cooldown boundary reached")
}

func TestPerKeyIsolation(t *testing.T) {
	b, _ := clocked(2, time.Minute)
	b.RecordFailure("a")
	assert.True(t, b.RecordFailure("a")) // a trips
	assert.False(t, b.Allow("a"))
	// b is untouched.
	assert.True(t, b.Allow("b"))
	assert.Equal(t, StateClosed, b.State("b"))
}
