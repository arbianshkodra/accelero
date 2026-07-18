// Package breaker implements a small per-key circuit breaker used to stop
// the reconciler from auto-deploying a persistently-failing stack on every
// cycle. Retries (internal/retry) handle transient failures *within* a
// single operation; this breaker handles the complementary case — a stack
// whose deploy keeps failing across cycles (bad image reference, revoked
// credentials, malformed compose). Without it, such a stack is redeployed
// every reconcile interval forever, burning CPU/registry bandwidth and
// spamming logs and alerts.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker's state for a single key.
type State int

const (
	// StateClosed — normal operation, calls allowed.
	StateClosed State = iota
	// StateOpen — tripped, calls blocked until the cooldown elapses.
	StateOpen
	// StateHalfOpen — cooldown elapsed, a single trial call is allowed;
	// its outcome decides whether to close (success) or re-open (failure).
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

type entry struct {
	consecutiveFailures int
	state               State
	openedAt            time.Time
}

// Breaker is a set of per-key circuit breakers sharing one threshold and
// cooldown. It is safe for concurrent use.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // injection point for tests

	mu      sync.Mutex
	entries map[string]*entry
}

// New returns a Breaker that trips after threshold consecutive failures and
// stays open for cooldown before allowing a half-open trial. A threshold
// <=0 disables the breaker entirely (Allow always permits, Record* are
// no-ops), so callers can wire it unconditionally.
func New(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		entries:   make(map[string]*entry),
	}
}

// Enabled reports whether the breaker actually gates anything.
func (b *Breaker) Enabled() bool {
	return b != nil && b.threshold > 0
}

// Threshold returns the configured consecutive-failure trip threshold.
func (b *Breaker) Threshold() int {
	if b == nil {
		return 0
	}
	return b.threshold
}

// Allow reports whether an operation for key may proceed now. When the
// breaker is open and the cooldown has elapsed, it transitions to half-open
// and permits a single trial call; while open and still cooling down it
// returns false.
func (b *Breaker) Allow(key string) bool {
	if !b.Enabled() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entryFor(key)
	if e.state == StateOpen {
		if b.now().Sub(e.openedAt) >= b.cooldown {
			e.state = StateHalfOpen
			return true
		}
		return false
	}
	// closed or half-open
	return true
}

// RecordSuccess closes the breaker for key and clears its failure count.
func (b *Breaker) RecordSuccess(key string) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entryFor(key)
	e.consecutiveFailures = 0
	e.state = StateClosed
}

// RecordFailure registers a failed operation for key. It returns true only
// when this failure freshly trips the breaker open from the closed state
// (i.e. the threshold was just reached), so the caller can log/audit the
// trip exactly once. A failed half-open trial re-opens the breaker (resetting
// the cooldown) but returns false — that recurs once per cooldown for a
// persistently-broken stack and would otherwise spam the audit log.
func (b *Breaker) RecordFailure(key string) (justTripped bool) {
	if !b.Enabled() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entryFor(key)
	e.consecutiveFailures++

	switch e.state {
	case StateHalfOpen:
		// Trial failed — reopen with a fresh cooldown.
		e.state = StateOpen
		e.openedAt = b.now()
		return false
	case StateClosed:
		if e.consecutiveFailures >= b.threshold {
			e.state = StateOpen
			e.openedAt = b.now()
			return true
		}
	}
	return false
}

// State returns the current state for key (StateClosed if unseen or disabled).
func (b *Breaker) State(key string) State {
	if !b.Enabled() {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[key]; ok {
		return e.state
	}
	return StateClosed
}

// entryFor returns the entry for key, creating a closed one lazily. Caller
// must hold b.mu.
func (b *Breaker) entryFor(key string) *entry {
	e, ok := b.entries[key]
	if !ok {
		e = &entry{state: StateClosed}
		b.entries[key] = e
	}
	return e
}
