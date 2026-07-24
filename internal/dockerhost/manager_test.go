package dockerhost

import (
	"testing"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientFor_EmptyHostIDReturnsDefault(t *testing.T) {
	def, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	require.NoError(t, err)
	m := NewManager(def, func(string) (*Host, error) {
		t.Fatal("lookup must not be called for the default host")
		return nil, nil
	})

	got, err := m.ClientFor("")
	require.NoError(t, err)
	assert.Same(t, def, got)
	assert.Same(t, def, m.Default())
}

func TestClientFor_NilLookupAlwaysDefault(t *testing.T) {
	def, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	require.NoError(t, err)
	m := NewManager(def, nil)

	got, err := m.ClientFor("some-host-id")
	require.NoError(t, err)
	assert.Same(t, def, got, "without a lookup every request falls back to the default host")
}

func TestClientFor_CachesPerHost(t *testing.T) {
	def, _ := client.New(client.WithHost("unix:///var/run/docker.sock"))
	calls := 0
	m := NewManager(def, func(id string) (*Host, error) {
		calls++
		return &Host{ID: id, Name: id, Endpoint: "tcp://127.0.0.1:2375"}, nil
	})
	t.Cleanup(m.Close)

	first, err := m.ClientFor("h1")
	require.NoError(t, err)
	second, err := m.ClientFor("h1")
	require.NoError(t, err)

	assert.Same(t, first, second, "same host yields the cached client")
	assert.Equal(t, 1, calls, "lookup happens once per host")
	assert.NotSame(t, def, first)

	// A different host builds its own client.
	other, err := m.ClientFor("h2")
	require.NoError(t, err)
	assert.NotSame(t, first, other)
	assert.Equal(t, 2, calls)
}

func TestClientFor_UnknownHostErrors(t *testing.T) {
	def, _ := client.New(client.WithHost("unix:///var/run/docker.sock"))
	m := NewManager(def, func(string) (*Host, error) { return nil, nil })

	_, err := m.ClientFor("ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestClientFor_LookupErrorPropagates(t *testing.T) {
	def, _ := client.New(client.WithHost("unix:///var/run/docker.sock"))
	m := NewManager(def, func(string) (*Host, error) { return nil, assert.AnError })

	_, err := m.ClientFor("h1")
	assert.ErrorIs(t, err, assert.AnError)
}

func TestTLSConfig_RequiresFullTriple(t *testing.T) {
	// A partial TLS triple is a misconfiguration, not a "TLS off" signal.
	_, err := newClient(&Host{Name: "h", Endpoint: "tcp://x:2376", TLSCert: "cert-only"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must all be set together")
}

func TestTLSConfig_RejectsGarbagePEM(t *testing.T) {
	_, err := newClient(&Host{
		Name: "h", Endpoint: "tcp://x:2376",
		TLSCA: "not-a-pem", TLSCert: "also-not", TLSKey: "nope",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client certificate")
}

func TestClose_DropsCachedClients(t *testing.T) {
	def, _ := client.New(client.WithHost("unix:///var/run/docker.sock"))
	calls := 0
	m := NewManager(def, func(id string) (*Host, error) {
		calls++
		return &Host{ID: id, Endpoint: "tcp://127.0.0.1:2375"}, nil
	})

	_, err := m.ClientFor("h1")
	require.NoError(t, err)
	m.Close()
	// After Close the cache is empty, so the next request rebuilds.
	_, err = m.ClientFor("h1")
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}
