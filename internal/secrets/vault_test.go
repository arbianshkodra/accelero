package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVault serves the Transit decrypt endpoint, recording what it was asked.
type fakeVault struct {
	srv *httptest.Server

	gotPath       string
	gotToken      string
	gotNamespace  string
	gotCiphertext string
}

// newFakeVault returns a Vault that unwraps to the given key bytes.
func newFakeVault(t *testing.T, key []byte) *fakeVault {
	f := &fakeVault{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotToken = r.Header.Get("X-Vault-Token")
		f.gotNamespace = r.Header.Get("X-Vault-Namespace")

		var body struct{ Ciphertext string `json:"ciphertext"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.gotCiphertext = body.Ciphertext

		resp := map[string]any{"data": map[string]string{
			"plaintext": base64.StdEncoding.EncodeToString(key),
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestUnwrapKeyWithVault_HappyPath(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	fv := newFakeVault(t, key)

	got, err := UnwrapKeyWithVault(context.Background(), VaultConfig{
		Addr: fv.srv.URL, Token: "tkn", TransitKey: "accelero",
		TransitMount: "transit", Namespace: "team-a",
		WrappedKey: "vault:v1:abcdef",
		HTTPClient: fv.srv.Client(),
	})
	require.NoError(t, err)
	assert.Equal(t, key, got)

	// The request must target the Transit decrypt endpoint and carry auth.
	assert.Equal(t, "/v1/transit/decrypt/accelero", fv.gotPath)
	assert.Equal(t, "tkn", fv.gotToken)
	assert.Equal(t, "team-a", fv.gotNamespace)
	assert.Equal(t, "vault:v1:abcdef", fv.gotCiphertext)
}

func TestUnwrapKeyWithVault_CustomMount(t *testing.T) {
	fv := newFakeVault(t, make([]byte, 32))
	_, err := UnwrapKeyWithVault(context.Background(), VaultConfig{
		Addr: fv.srv.URL, Token: "t", TransitKey: "k",
		TransitMount: "kms/transit", WrappedKey: "vault:v1:x",
		HTTPClient: fv.srv.Client(),
	})
	require.NoError(t, err)
	assert.Equal(t, "/v1/kms/transit/decrypt/k", fv.gotPath)
}

func TestUnwrapKeyWithVault_SurfacesVaultErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
	}))
	defer srv.Close()

	_, err := UnwrapKeyWithVault(context.Background(), VaultConfig{
		Addr: srv.URL, Token: "bad", TransitKey: "k", TransitMount: "transit",
		WrappedKey: "vault:v1:x", HTTPClient: srv.Client(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	// The wrapped key must never be echoed into an error message.
	assert.NotContains(t, err.Error(), "vault:v1:x")
}

func TestUnwrapKeyWithVault_EmptyPlaintextIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{}})
	}))
	defer srv.Close()

	_, err := UnwrapKeyWithVault(context.Background(), VaultConfig{
		Addr: srv.URL, Token: "t", TransitKey: "k", TransitMount: "transit",
		WrappedKey: "vault:v1:x", HTTPClient: srv.Client(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no plaintext")
}

func TestUnwrapKeyWithVault_UnreachableVault(t *testing.T) {
	_, err := UnwrapKeyWithVault(context.Background(), VaultConfig{
		Addr: "http://127.0.0.1:1", Token: "t", TransitKey: "k",
		TransitMount: "transit", WrappedKey: "vault:v1:x",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transit decrypt request failed")
}

// --- env wiring -------------------------------------------------------------

func TestLoadCipherFromEnv_VaultSource(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 7
	fv := newFakeVault(t, key)

	t.Setenv(envKeyVaultName, "vault:v1:wrapped")
	t.Setenv(envVaultAddr, fv.srv.URL)
	t.Setenv(envVaultToken, "tkn")
	t.Setenv(envVaultTransitKey, "accelero")

	c, err := LoadCipherFromEnv()
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.True(t, c.Enabled(), "vault-sourced key enables encryption")

	// The cipher really works with the unwrapped key.
	ct, err := c.Encrypt("hello")
	require.NoError(t, err)
	assert.NotEqual(t, "hello", ct)
	pt, err := c.Decrypt(ct)
	require.NoError(t, err)
	assert.Equal(t, "hello", pt)
}

func TestLoadCipherFromEnv_VaultMissingRequiredVars(t *testing.T) {
	t.Setenv(envKeyVaultName, "vault:v1:wrapped")
	// VAULT_ADDR / VAULT_TOKEN / transit key deliberately unset.
	_, err := LoadCipherFromEnv()
	require.Error(t, err, "must fail loudly rather than silently running unencrypted")
	assert.Contains(t, err.Error(), envVaultAddr)
	assert.Contains(t, err.Error(), envVaultToken)
	assert.Contains(t, err.Error(), envVaultTransitKey)
}

func TestLoadCipherFromEnv_SourcesAreMutuallyExclusive(t *testing.T) {
	t.Setenv(envKeyVaultName, "vault:v1:wrapped")
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(make([]byte, 32)))

	_, err := LoadCipherFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one of")
}

func TestLoadCipherFromEnv_NoVaultVarsLeavesOtherSourcesAlone(t *testing.T) {
	// With no Vault selector, the inline key still works exactly as before.
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	c, err := LoadCipherFromEnv()
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.True(t, c.Enabled())
}
