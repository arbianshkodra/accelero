// Package notify pushes notable lifecycle events (deploys, drift) to external
// destinations — a generic JSON webhook and/or a Slack incoming webhook.
// It wraps the audit recorder (see recorder.go) so the deployer and reconciler
// need no changes: they already emit these events into the audit log.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// Event is a notable lifecycle event delivered to sinks.
type Event struct {
	Type      string            `json:"type"`             // e.g. "deploy.failed"
	StackName string            `json:"stack,omitempty"`
	Outcome   string            `json:"outcome,omitempty"` // success/failure/in_progress
	Message   string            `json:"message"`           // human-readable summary
	Fields    map[string]string `json:"fields,omitempty"`  // extra detail (from audit metadata)
	Time      time.Time         `json:"time"`
}

// Sink delivers one event to one destination.
type Sink interface {
	Send(ctx context.Context, e Event) error
	Name() string
}

// Notifier fans an event out to all configured sinks. Delivery is best-effort
// and asynchronous — a failing sink must never block or fail a deploy.
type Notifier interface {
	Notify(e Event)
	Enabled() bool
}

// Config holds the destination URLs for each supported sink. Empty fields are
// skipped. Using a struct (rather than positional args) keeps call sites
// readable as more sinks are added.
type Config struct {
	WebhookURL string       // generic JSON webhook (full Event)
	SlackURL   string       // Slack incoming webhook ({"text": ...})
	DiscordURL string       // Discord webhook ({"content": ...})
	TeamsURL   string       // Microsoft Teams incoming webhook (MessageCard)
	Email      *EmailConfig // SMTP sink; nil or incomplete = disabled
}

// New builds a Notifier from the configured destination URLs. Empty URLs are
// skipped; with none configured the result is a no-op (Enabled() == false).
func New(cfg Config) Notifier {
	d := &dispatcher{client: &http.Client{Timeout: 10 * time.Second}}
	if cfg.WebhookURL != "" {
		d.sinks = append(d.sinks, &webhookSink{url: cfg.WebhookURL, client: d.client})
	}
	if cfg.SlackURL != "" {
		d.sinks = append(d.sinks, &slackSink{url: cfg.SlackURL, client: d.client})
	}
	if cfg.DiscordURL != "" {
		d.sinks = append(d.sinks, &discordSink{url: cfg.DiscordURL, client: d.client})
	}
	if cfg.TeamsURL != "" {
		d.sinks = append(d.sinks, &teamsSink{url: cfg.TeamsURL, client: d.client})
	}
	if cfg.Email != nil && cfg.Email.enabled() {
		d.sinks = append(d.sinks, newSMTPSink(*cfg.Email))
	}
	return d
}

type dispatcher struct {
	sinks  []Sink
	client *http.Client
}

func (d *dispatcher) Enabled() bool { return len(d.sinks) > 0 }

func (d *dispatcher) Notify(e Event) {
	if len(d.sinks) == 0 {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// Fire-and-forget so notification latency never slows a deploy. Each
	// send is independently bounded by the client timeout.
	for _, s := range d.sinks {
		s := s
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.Send(ctx, e); err != nil {
				logrus.WithError(err).Warnf("notify: %s sink failed for event %s", s.Name(), e.Type)
			}
		}()
	}
}

// postJSON is the shared HTTP helper for both sinks.
func postJSON(ctx context.Context, client *http.Client, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// webhookSink POSTs the full Event as JSON to a generic endpoint.
type webhookSink struct {
	url    string
	client *http.Client
}

func (s *webhookSink) Name() string { return "webhook" }
func (s *webhookSink) Send(ctx context.Context, e Event) error {
	return postJSON(ctx, s.client, s.url, e)
}

// slackSink POSTs a Slack incoming-webhook payload ({"text": ...}).
type slackSink struct {
	url    string
	client *http.Client
}

func (s *slackSink) Name() string { return "slack" }
func (s *slackSink) Send(ctx context.Context, e Event) error {
	return postJSON(ctx, s.client, s.url, map[string]string{"text": e.Message})
}

// discordSink POSTs a Discord webhook payload ({"content": ...}).
type discordSink struct {
	url    string
	client *http.Client
}

func (s *discordSink) Name() string { return "discord" }
func (s *discordSink) Send(ctx context.Context, e Event) error {
	return postJSON(ctx, s.client, s.url, map[string]string{"content": e.Message})
}

// teamsSink POSTs a Microsoft Teams incoming-webhook MessageCard. The classic
// connector renders a bare {"text": ...}, but a MessageCard (with a summary,
// required by the schema) is the portable form across connector versions.
type teamsSink struct {
	url    string
	client *http.Client
}

func (s *teamsSink) Name() string { return "teams" }
func (s *teamsSink) Send(ctx context.Context, e Event) error {
	return postJSON(ctx, s.client, s.url, map[string]string{
		"@type":    "MessageCard",
		"@context": "https://schema.org/extensions",
		"summary":  "Accelero notification",
		"text":     e.Message,
	})
}
