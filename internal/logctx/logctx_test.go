package logctx

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestFromContext_Default(t *testing.T) {
	// Nil context returns a default entry. Use a typed nil so staticcheck
	// doesn't trip SA1012 on an intentional nil-safety test.
	var nilCtx context.Context
	entry := FromContext(nilCtx)
	assert.NotNil(t, entry)

	// Empty background context also returns default.
	entry = FromContext(context.Background())
	assert.NotNil(t, entry)
}

func TestWithFields(t *testing.T) {
	ctx := WithFields(context.Background(), logrus.Fields{
		"request_id": "abc123",
		"stack_name": "test-stack",
	})

	entry := FromContext(ctx)
	assert.Equal(t, "abc123", entry.Data["request_id"])
	assert.Equal(t, "test-stack", entry.Data["stack_name"])
}

func TestWithFields_NilContext(t *testing.T) {
	// Passing nil must not panic; it should seed a fresh background ctx.
	var nilCtx context.Context
	ctx := WithFields(nilCtx, logrus.Fields{"k": "v"})
	assert.NotNil(t, ctx)
	assert.Equal(t, "v", FromContext(ctx).Data["k"])
}

func TestWithFields_MergesWithExisting(t *testing.T) {
	ctx := WithFields(context.Background(), logrus.Fields{"a": 1})
	ctx = WithFields(ctx, logrus.Fields{"b": 2})

	entry := FromContext(ctx)
	assert.Equal(t, 1, entry.Data["a"])
	assert.Equal(t, 2, entry.Data["b"])
}

func TestWithFields_Overwrites(t *testing.T) {
	ctx := WithFields(context.Background(), logrus.Fields{"k": "first"})
	ctx = WithFields(ctx, logrus.Fields{"k": "second"})

	entry := FromContext(ctx)
	assert.Equal(t, "second", entry.Data["k"])
}

func TestWithField(t *testing.T) {
	ctx := WithField(context.Background(), "deployment_id", "deploy-42")
	assert.Equal(t, "deploy-42", FromContext(ctx).Data["deployment_id"])
}

func TestWithLogger(t *testing.T) {
	original := logrus.NewEntry(logrus.StandardLogger()).WithField("preserved", true)
	ctx := WithLogger(context.Background(), original)

	entry := FromContext(ctx)
	assert.Equal(t, true, entry.Data["preserved"])
}

func TestWithLogger_NilEntry(t *testing.T) {
	// Passing a nil entry must not replace any existing logger.
	ctx := WithField(context.Background(), "k", "v")
	ctx = WithLogger(ctx, nil)

	entry := FromContext(ctx)
	assert.Equal(t, "v", entry.Data["k"])
}

func TestWithLogger_NilContext(t *testing.T) {
	entry := logrus.NewEntry(logrus.StandardLogger()).WithField("x", 1)
	var nilCtx context.Context
	ctx := WithLogger(nilCtx, entry)

	got := FromContext(ctx)
	assert.Equal(t, 1, got.Data["x"])
}

func TestEntry_WritesFieldsToOutput(t *testing.T) {
	// End-to-end: a context-derived entry actually emits the fields
	// when it logs a line.  Capture the standard logger's output.
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetFormatter(&logrus.JSONFormatter{})

	entry := logrus.NewEntry(logger).WithField("request_id", "rid-1")
	ctx := WithLogger(context.Background(), entry)
	FromContext(ctx).Info("hello")

	var out map[string]interface{}
	assert.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.Equal(t, "rid-1", out["request_id"])
	assert.Equal(t, "hello", out["msg"])
}
