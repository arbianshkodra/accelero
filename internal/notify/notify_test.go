package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_DisabledWhenNoURLs(t *testing.T) {
	assert.False(t, New("", "").Enabled())
	assert.True(t, New("http://x/webhook", "").Enabled())
	assert.True(t, New("", "http://x/slack").Enabled())
}

// capture is a tiny server that records the bodies it receives.
type capture struct {
	mu     sync.Mutex
	bodies []map[string]any
	srv    *httptest.Server
}

func newCapture(t *testing.T) *capture {
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		c.mu.Lock()
		c.bodies = append(c.bodies, m)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.bodies)
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d deliveries", n)
}

func TestNotify_WebhookAndSlackReceivePayloads(t *testing.T) {
	webhook := newCapture(t)
	slack := newCapture(t)
	n := New(webhook.srv.URL, slack.srv.URL)

	n.Notify(Event{Type: "deploy.failed", StackName: "web", Outcome: "failure", Message: "❌ Deploy failed for stack `web`"})

	webhook.waitFor(t, 1)
	slack.waitFor(t, 1)

	// Generic webhook gets the full event.
	assert.Equal(t, "deploy.failed", webhook.bodies[0]["type"])
	assert.Equal(t, "web", webhook.bodies[0]["stack"])
	// Slack gets a {"text": ...} payload.
	assert.Contains(t, slack.bodies[0]["text"], "Deploy failed")
}

func TestWrapRecorder_NotifiesOnDeployAndDriftOnly(t *testing.T) {
	webhook := newCapture(t)
	base := &fakeRecorder{}
	rec := WrapRecorder(base, New(webhook.srv.URL, ""))

	// Notable events → notify.
	require.NoError(t, rec.Record(context.Background(), store.AuditEntry{Operation: store.AuditOpDeployComplete, StackName: "web", Outcome: "success", Metadata: map[string]string{"changes": "3"}}))
	require.NoError(t, rec.Record(context.Background(), store.AuditEntry{Operation: store.AuditOpDriftDetected, StackName: "api", Metadata: map[string]string{"drift_count": "2"}}))
	// Non-notable events → persisted but NOT notified.
	require.NoError(t, rec.Record(context.Background(), store.AuditEntry{Operation: store.AuditOpStackCreate, StackName: "web"}))
	require.NoError(t, rec.Record(context.Background(), store.AuditEntry{Operation: store.AuditOpVolumeBrowse}))

	webhook.waitFor(t, 2)
	// All four were persisted through to the base recorder.
	assert.Equal(t, 4, base.count())
	// Only the two notable ones were delivered.
	time.Sleep(50 * time.Millisecond) // let any stray deliveries arrive
	webhook.mu.Lock()
	defer webhook.mu.Unlock()
	assert.Len(t, webhook.bodies, 2)
	types := []string{webhook.bodies[0]["type"].(string), webhook.bodies[1]["type"].(string)}
	assert.ElementsMatch(t, []string{"deploy.complete", "drift.detected"}, types)
}

func TestWrapRecorder_NoopWhenDisabled(t *testing.T) {
	base := &fakeRecorder{}
	// Disabled notifier → returns the base recorder unchanged.
	assert.Same(t, base, WrapRecorder(base, New("", "")))
}

func TestEventFromAudit_Messages(t *testing.T) {
	cases := map[string]string{
		store.AuditOpDeployStart:      "🚀",
		store.AuditOpDeployComplete:   "✅",
		store.AuditOpDeployFailed:     "❌",
		store.AuditOpDeployRolledBack: "↩️",
		store.AuditOpDriftDetected:    "⚠️",
		store.AuditOpDriftAutoDeployed: "🔄",
	}
	for op, emoji := range cases {
		ev, ok := eventFromAudit(store.AuditEntry{Operation: op, StackName: "s"})
		require.True(t, ok, op)
		assert.Contains(t, ev.Message, emoji)
		assert.Contains(t, ev.Message, "`s`")
	}
	_, ok := eventFromAudit(store.AuditEntry{Operation: store.AuditOpStackDelete})
	assert.False(t, ok, "non-deploy/drift ops are not notified")
}

type fakeRecorder struct {
	mu sync.Mutex
	n  int
}

func (f *fakeRecorder) Record(_ context.Context, _ store.AuditEntry) error {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return nil
}
func (f *fakeRecorder) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }
