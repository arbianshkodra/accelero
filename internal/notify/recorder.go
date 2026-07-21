package notify

import (
	"context"
	"fmt"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/store"
)

// WrapRecorder returns an audit.Recorder that, after persisting each entry,
// pushes a notification for the notable deploy/drift operations. Wrapping the
// recorder keeps the deployer and reconciler untouched — they already record
// these events. If the notifier is nil/disabled, the base recorder is returned
// unchanged (zero overhead).
func WrapRecorder(base audit.Recorder, n Notifier) audit.Recorder {
	if n == nil || !n.Enabled() {
		return base
	}
	return &notifyingRecorder{base: base, n: n}
}

type notifyingRecorder struct {
	base audit.Recorder
	n    Notifier
}

func (r *notifyingRecorder) Record(ctx context.Context, e store.AuditEntry) error {
	err := r.base.Record(ctx, e) // persistence first; notification is secondary
	if ev, ok := eventFromAudit(e); ok {
		r.n.Notify(ev)
	}
	return err
}

// eventFromAudit maps the deploy/drift audit operations to a notification
// Event with a human-readable message. Other operations return ok=false and
// are not notified (avoids spamming on reads, browses, secret CRUD, etc.).
func eventFromAudit(e store.AuditEntry) (Event, bool) {
	stack := e.StackName
	if stack == "" {
		stack = e.StackID
	}
	var msg string
	switch e.Operation {
	case store.AuditOpDeployStart:
		msg = fmt.Sprintf("🚀 Deploy started for stack `%s`", stack)
	case store.AuditOpDeployComplete:
		msg = fmt.Sprintf("✅ Deploy completed for stack `%s`", stack)
		if c := e.Metadata["changes"]; c != "" {
			msg += fmt.Sprintf(" (%s change(s))", c)
		}
	case store.AuditOpDeployFailed:
		msg = fmt.Sprintf("❌ Deploy failed for stack `%s`", stack)
		if e.ErrorMessage != "" {
			msg += ": " + e.ErrorMessage
		}
	case store.AuditOpDeployRolledBack:
		msg = fmt.Sprintf("↩️ Deploy rolled back for stack `%s`", stack)
	case store.AuditOpDriftDetected:
		msg = fmt.Sprintf("⚠️ Drift detected on stack `%s`", stack)
		if c := e.Metadata["drift_count"]; c != "" {
			msg += fmt.Sprintf(" (%s item(s))", c)
		}
	case store.AuditOpDriftAutoDeployed:
		msg = fmt.Sprintf("🔄 Auto-deploying stack `%s` to resolve drift", stack)
	case store.AuditOpApprovalRequested:
		msg = fmt.Sprintf("🔔 Deploy of stack `%s` is awaiting approval", stack)
	case store.AuditOpApprovalGranted:
		msg = fmt.Sprintf("👍 Deploy of stack `%s` was approved", stack)
	case store.AuditOpApprovalRejected:
		msg = fmt.Sprintf("🚫 Deploy of stack `%s` was rejected", stack)
	case store.AuditOpApprovalTimedOut:
		msg = fmt.Sprintf("⏰ Approval for stack `%s` timed out", stack)
	default:
		return Event{}, false
	}
	return Event{
		Type:      e.Operation,
		StackName: e.StackName,
		Outcome:   e.Outcome,
		Message:   msg,
		Fields:    e.Metadata,
	}, true
}
