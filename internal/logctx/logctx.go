// Package logctx provides context-aware logging built on logrus.
//
// The common pattern is:
//
//	ctx = logctx.WithFields(ctx, logrus.Fields{"deployment_id": id})
//	logctx.FromContext(ctx).Info("deploying")
//
// Any goroutine that inherits the context also inherits the log fields,
// so downstream log lines automatically carry request IDs, deployment IDs,
// stack names, etc.
package logctx

import (
	"context"

	"github.com/sirupsen/logrus"
)

type ctxKey struct{}

var loggerKey = ctxKey{}

// WithFields returns a copy of ctx with the given fields merged into the
// logger entry stored on the context.  If no entry is present, a new one
// is created from logrus.StandardLogger().
func WithFields(ctx context.Context, fields logrus.Fields) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	entry := FromContext(ctx).WithFields(fields)
	return context.WithValue(ctx, loggerKey, entry)
}

// WithField is a shortcut for WithFields with a single key/value pair.
func WithField(ctx context.Context, key string, value interface{}) context.Context {
	return WithFields(ctx, logrus.Fields{key: value})
}

// WithLogger attaches an existing logrus entry to the context, replacing
// any previous logger.  Useful when propagating logger state across
// goroutine boundaries (e.g. from an HTTP request into a background
// deployment task).
func WithLogger(ctx context.Context, entry *logrus.Entry) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if entry == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey, entry)
}

// FromContext returns the logger stored in the context, or a fresh entry
// from the standard logger if none is present.
func FromContext(ctx context.Context) *logrus.Entry {
	if ctx != nil {
		if entry, ok := ctx.Value(loggerKey).(*logrus.Entry); ok {
			return entry
		}
	}
	return logrus.NewEntry(logrus.StandardLogger())
}
