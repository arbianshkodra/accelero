// Package dockerhost resolves a stack's target Docker host to a client.
//
// Accelero historically talked to exactly one daemon (DOCKER_SOCK). Multi-host
// keeps that daemon as the implicit "default host" and lets operators register
// additional hosts in the store; a stack's host_id selects which one it deploys
// to. Clients are built lazily and cached per host, since a Docker client owns
// an HTTP transport (and possibly a TLS config) that we don't want to rebuild
// per request.
package dockerhost

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// Host is a registered Docker endpoint. It mirrors store.DockerHost but is
// defined here so this package doesn't import store (and store doesn't need to
// import a Docker client). The store adapts between the two.
type Host struct {
	ID       string
	Name     string
	Endpoint string // unix:///... or tcp://host:port
	TLSCA    string // PEM; all three TLS fields set = mutual TLS
	TLSCert  string
	TLSKey   string
}

// Manager builds and caches one Docker client per host.
type Manager struct {
	// def is the client for the default host (DOCKER_SOCK). Returned for an
	// empty host ID, which is what every pre-multi-host stack has.
	def *client.Client

	lookup func(id string) (*Host, error)

	mu     sync.Mutex
	byHost map[string]*client.Client
}

// NewManager returns a Manager over the default client. lookup resolves a host
// ID to its connection details; it may be nil, in which case only the default
// host is available.
func NewManager(def *client.Client, lookup func(id string) (*Host, error)) *Manager {
	return &Manager{
		def:    def,
		lookup: lookup,
		byHost: make(map[string]*client.Client),
	}
}

// Default returns the default-host client.
func (m *Manager) Default() *client.Client { return m.def }

// ClientFor returns the client for a host ID. An empty ID (or a nil lookup)
// yields the default client, so callers can pass stack.HostID unconditionally.
func (m *Manager) ClientFor(hostID string) (*client.Client, error) {
	if hostID == "" || m.lookup == nil {
		return m.def, nil
	}

	m.mu.Lock()
	if c, ok := m.byHost[hostID]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	h, err := m.lookup(hostID)
	if err != nil {
		return nil, fmt.Errorf("look up docker host %s: %w", hostID, err)
	}
	if h == nil {
		return nil, fmt.Errorf("docker host %s not found", hostID)
	}

	c, err := newClient(h)
	if err != nil {
		return nil, err
	}

	// Another goroutine may have raced us here; keep whichever landed first so
	// there's exactly one cached client per host.
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.byHost[hostID]; ok {
		_ = c.Close()
		return existing, nil
	}
	m.byHost[hostID] = c
	return c, nil
}

// Ping verifies a host is reachable, without caching a client — used to
// validate a host at registration time.
func Ping(ctx context.Context, h *Host) error {
	c, err := newClient(h)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("ping %s: %w", h.Endpoint, err)
	}
	return nil
}

// Close closes every cached per-host client. The default client is owned by
// the caller (main) and is deliberately left alone.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.byHost {
		_ = c.Close()
		delete(m.byHost, id)
	}
}

// newClient builds a Docker client for a host, wiring mutual TLS when the host
// carries certificates (the usual setup for a tcp:// daemon).
func newClient(h *Host) (*client.Client, error) {
	opts := []client.Opt{client.WithHost(h.Endpoint)}

	if h.TLSCA != "" || h.TLSCert != "" || h.TLSKey != "" {
		tlsCfg, err := tlsConfig(h)
		if err != nil {
			return nil, err
		}
		// The client's WithTLSClientConfig takes *file paths*; our certs live
		// in the DB (encrypted), so inject a transport carrying the in-memory
		// config instead of spilling key material to disk.
		opts = append(opts, client.WithHTTPClient(&http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		}))
	}

	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client for %s: %w", h.Endpoint, err)
	}
	return c, nil
}

func tlsConfig(h *Host) (*tls.Config, error) {
	if h.TLSCA == "" || h.TLSCert == "" || h.TLSKey == "" {
		return nil, fmt.Errorf("host %s: tls_ca, tls_cert and tls_key must all be set together", h.Name)
	}
	cert, err := tls.X509KeyPair([]byte(h.TLSCert), []byte(h.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("host %s: parse client certificate: %w", h.Name, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(h.TLSCA)) {
		return nil, fmt.Errorf("host %s: tls_ca is not a valid PEM certificate", h.Name)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
