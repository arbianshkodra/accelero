package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailConfig_Enabled(t *testing.T) {
	assert.False(t, EmailConfig{}.enabled())
	assert.False(t, EmailConfig{Host: "smtp.x", From: "a@x"}.enabled(), "no recipients")
	assert.False(t, EmailConfig{Host: "smtp.x", To: []string{"b@x"}}.enabled(), "no from")
	assert.True(t, EmailConfig{Host: "smtp.x", From: "a@x", To: []string{"b@x"}}.enabled())
}

func TestNew_EnablesEmailSink(t *testing.T) {
	assert.False(t, New(Config{Email: &EmailConfig{Host: "smtp.x"}}).Enabled(), "incomplete email config = no sink")
	assert.True(t, New(Config{Email: &EmailConfig{Host: "smtp.x", From: "a@x", To: []string{"b@x"}}}).Enabled())
}

func TestBuildEmail_Headers(t *testing.T) {
	cfg := EmailConfig{From: "accelero@x", To: []string{"ops@x", "sre@x"}}
	e := Event{Type: "deploy.failed", StackName: "web", Message: "❌ Deploy failed for stack `web`"}
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	msg := string(buildEmail(cfg, e, now))
	assert.Contains(t, msg, "From: accelero@x\r\n")
	assert.Contains(t, msg, "To: ops@x, sre@x\r\n")
	assert.Contains(t, msg, "Subject: [Accelero] deploy.failed — web\r\n")
	assert.Contains(t, msg, "Content-Type: text/plain; charset=utf-8\r\n")
	// Blank line separates headers from body, and the body is the message.
	require.Contains(t, msg, "\r\n\r\n")
	body := msg[strings.Index(msg, "\r\n\r\n")+4:]
	assert.Contains(t, body, "Deploy failed for stack `web`")
}

func TestSMTPSink_SendInvokesTransport(t *testing.T) {
	var gotCfg EmailConfig
	var gotMsg []byte
	sink := &smtpSink{
		cfg: EmailConfig{Host: "smtp.x", From: "a@x", To: []string{"b@x"}},
		send: func(cfg EmailConfig, msg []byte) error {
			gotCfg = cfg
			gotMsg = msg
			return nil
		},
	}
	err := sink.Send(context.Background(), Event{Type: "deploy.complete", StackName: "web", Message: "✅ done"})
	require.NoError(t, err)
	assert.Equal(t, "smtp.x", gotCfg.Host)
	assert.Contains(t, string(gotMsg), "Subject: [Accelero] deploy.complete — web")
	assert.Contains(t, string(gotMsg), "✅ done")
	assert.Equal(t, "email", sink.Name())
}
