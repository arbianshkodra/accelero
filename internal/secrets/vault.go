package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Env vars for the Vault Transit master-key source.
const (
	// envKeyVaultName holds the master key *wrapped* by Vault Transit (a
	// "vault:v1:..." ciphertext). Its presence selects this source.
	envKeyVaultName = "ACCELERO_ENCRYPTION_KEY_VAULT"

	envVaultAddr      = "VAULT_ADDR"  // standard Vault env name
	envVaultToken     = "VAULT_TOKEN" // standard Vault env name
	envVaultTransitKey   = "ACCELERO_VAULT_TRANSIT_KEY"
	envVaultTransitMount = "ACCELERO_VAULT_TRANSIT_MOUNT"
	envVaultNamespace    = "ACCELERO_VAULT_NAMESPACE"

	defaultTransitMount = "transit"
)

// VaultConfig describes how to reach a Vault Transit endpoint that can unwrap
// Accelero's master key.
//
// The model is envelope encryption for the *master key*: an operator generates a
// 32-byte key, encrypts it with a Transit key (`vault write
// transit/encrypt/<key> plaintext=<base64>`), and hands Accelero only the
// resulting ciphertext. Accelero asks Vault to decrypt it at startup and keeps
// the plaintext in memory only. The master key is therefore never at rest in
// plaintext anywhere — and rotating the Transit key (the KEK) never requires
// re-encrypting Accelero's database fields.
type VaultConfig struct {
	Addr      string // e.g. https://vault.internal:8200
	Token     string
	TransitKey   string // Transit key name that wrapped the master key
	TransitMount string // Transit mount path (default "transit")
	Namespace    string // optional, Vault Enterprise

	// WrappedKey is the "vault:v1:..." ciphertext of the master key.
	WrappedKey string

	// HTTPClient allows tests (and operators needing custom TLS) to supply
	// their own client. Nil uses a 10s-timeout default.
	HTTPClient *http.Client
}

// vaultConfigFromEnv assembles a VaultConfig, returning ok=false when the
// selector env var is absent (i.e. this source isn't in use).
func vaultConfigFromEnv() (VaultConfig, bool, error) {
	wrapped := strings.TrimSpace(os.Getenv(envKeyVaultName))
	if wrapped == "" {
		return VaultConfig{}, false, nil
	}

	cfg := VaultConfig{
		Addr:         strings.TrimSpace(os.Getenv(envVaultAddr)),
		Token:        strings.TrimSpace(os.Getenv(envVaultToken)),
		TransitKey:   strings.TrimSpace(os.Getenv(envVaultTransitKey)),
		TransitMount: strings.TrimSpace(os.Getenv(envVaultTransitMount)),
		Namespace:    strings.TrimSpace(os.Getenv(envVaultNamespace)),
		WrappedKey:   wrapped,
	}
	if cfg.TransitMount == "" {
		cfg.TransitMount = defaultTransitMount
	}

	// Fail loudly rather than silently running unencrypted: the operator
	// clearly asked for Vault-sourced encryption.
	var missing []string
	if cfg.Addr == "" {
		missing = append(missing, envVaultAddr)
	}
	if cfg.Token == "" {
		missing = append(missing, envVaultToken)
	}
	if cfg.TransitKey == "" {
		missing = append(missing, envVaultTransitKey)
	}
	if len(missing) > 0 {
		return VaultConfig{}, false, fmt.Errorf(
			"%s is set but %s must also be set", envKeyVaultName, strings.Join(missing, ", "))
	}
	return cfg, true, nil
}

// loadCipherFromVault unwraps the master key via Vault Transit and builds the
// cipher from it.
func loadCipherFromVault(ctx context.Context, cfg VaultConfig) (*Cipher, error) {
	key, err := UnwrapKeyWithVault(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return NewCipher(key)
}

// UnwrapKeyWithVault asks Vault Transit to decrypt the wrapped master key and
// returns the plaintext key bytes.
func UnwrapKeyWithVault(ctx context.Context, cfg VaultConfig) ([]byte, error) {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	url := fmt.Sprintf("%s/v1/%s/decrypt/%s",
		strings.TrimRight(cfg.Addr, "/"), cfg.TransitMount, cfg.TransitKey)

	body, err := json.Marshal(map[string]string{"ciphertext": cfg.WrappedKey})
	if err != nil {
		return nil, fmt.Errorf("vault: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vault: build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	if cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", cfg.Namespace)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault: transit decrypt request failed: %w", err)
	}
	defer resp.Body.Close()

	// Cap the response read: a misconfigured VAULT_ADDR could point at
	// something that streams unbounded data.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("vault: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Vault reports failures as {"errors":[...]}; surface them, but never
		// echo the request body (it contains key ciphertext).
		var errResp struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(raw, &errResp) == nil && len(errResp.Errors) > 0 {
			return nil, fmt.Errorf("vault: transit decrypt failed (%s): %s",
				resp.Status, strings.Join(errResp.Errors, "; "))
		}
		return nil, fmt.Errorf("vault: transit decrypt failed: %s", resp.Status)
	}

	var decoded struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("vault: decode response: %w", err)
	}
	if decoded.Data.Plaintext == "" {
		return nil, fmt.Errorf("vault: transit decrypt returned no plaintext (is %q a Transit key?)", cfg.TransitKey)
	}

	// Transit returns the plaintext base64-encoded.
	key, err := base64.StdEncoding.DecodeString(decoded.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault: base64-decode unwrapped key: %w", err)
	}
	return key, nil
}
