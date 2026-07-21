package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// EmailConfig configures the SMTP sink. The sink is enabled only when Host,
// From, and at least one To recipient are set.
type EmailConfig struct {
	Host     string
	Port     int // 465 = implicit TLS; anything else uses STARTTLS when offered
	Username string
	Password string // when Username is set, PLAIN auth is used
	From     string
	To       []string
}

func (c EmailConfig) enabled() bool {
	return c.Host != "" && c.From != "" && len(c.To) > 0
}

// smtpSink delivers events by email. The send func is injectable so tests can
// exercise message construction without a real SMTP server.
type smtpSink struct {
	cfg  EmailConfig
	send func(cfg EmailConfig, msg []byte) error
}

func newSMTPSink(cfg EmailConfig) *smtpSink {
	return &smtpSink{cfg: cfg, send: smtpSend}
}

func (s *smtpSink) Name() string { return "email" }

func (s *smtpSink) Send(_ context.Context, e Event) error {
	return s.send(s.cfg, buildEmail(s.cfg, e, time.Now()))
}

// buildEmail renders an RFC 5322 message (CRLF line endings, UTF-8 plain text).
func buildEmail(cfg EmailConfig, e Event, now time.Time) []byte {
	subject := "[Accelero] " + e.Type
	if e.StackName != "" {
		subject += " — " + e.StackName
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(cfg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(e.Message)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// smtpSend delivers one message. It handles plaintext (no STARTTLS offered,
// e.g. a local relay), STARTTLS (port 587 / 25 when the server advertises it),
// and implicit TLS (port 465). PLAIN auth is used only when a username is set.
func smtpSend(cfg EmailConfig, msg []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}

	if cfg.Port == 465 {
		conn = tls.Client(conn, &tls.Config{ServerName: cfg.Host})
	}

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, rcpt := range cfg.To {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return c.Quit()
}
