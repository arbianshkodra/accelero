// Package secrets provides the at-rest encryption primitives used by
// the SQLite store to protect sensitive fields — repo tokens, docker
// registry passwords, and future per-stack application secrets.
//
// Design:
//
//   - AES-256-GCM AEAD. 96-bit random nonce per message, authenticated
//     with the empty-AAD default. Same algorithm (TLS 1.2 cipher suites
//     onward) and same Go crypto primitives every mainstream project
//     uses.
//
//   - Ciphertext format is text: "v1:<base64-nonce>:<base64-ct>". The
//     "v1:" prefix lets the store distinguish ciphertext from legacy
//     plaintext rows without a schema change, and carves out room to
//     migrate to a different cipher (XChaCha20-Poly1305, AES-GCM-SIV)
//     later. Storing as text means we don't have to modify column types
//     in the schema.
//
//   - Master key sources (mutually exclusive):
//       1. ACCELERO_ENCRYPTION_KEY_VAULT — the key *wrapped* by a
//          HashiCorp Vault Transit key. Unwrapped at startup and held
//          in memory only, so the key is never at rest in plaintext;
//          rotating the Transit KEK needs no DB re-encryption. Strongest
//          option; see vault.go.
//       2. ACCELERO_ENCRYPTION_KEY_FILE — path to a file containing
//          the base64-encoded 32-byte key. Good in production:
//          env vars leak through `docker inspect`, process listings,
//          systemd unit files, and shell history; a file mounted as a
//          Docker/K8s secret does not.
//       3. ACCELERO_ENCRYPTION_KEY — the base64 key inline. Fine for
//          local development.
//     Setting more than one is a configuration error (ambiguous).
//     Setting none disables encryption — existing deployments keep
//     working through an upgrade but get a loud warning at startup
//     telling the operator they aren't getting encryption until they
//     set a key.
//
//   - Future key rotation is handled by the v1 prefix: v2 ciphertext
//     can be produced by a newer cipher, and the decrypt path picks
//     the right algorithm from the prefix. Not implemented yet because
//     there's nothing to rotate to.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// envKeyName / envKeyFileName are the two places we look for the master
// key. Exposed so cmd/main.go can keep startup log messages in sync
// with the docs. File source wins over inline source; see LoadCipherFromEnv.
const (
	envKeyName     = "ACCELERO_ENCRYPTION_KEY"
	envKeyFileName = "ACCELERO_ENCRYPTION_KEY_FILE"
)

// CipherVersionV1 is the only format version currently emitted or
// accepted. Bump (and implement the new branch in Decrypt) when a
// different algorithm needs to coexist with old data.
const CipherVersionV1 = "v1"

// ErrNoKey is returned when Encrypt/Decrypt is called on a Cipher that
// was constructed without a key — callers check for this and fall back
// to storing plaintext.
var ErrNoKey = errors.New("encryption disabled: no master key configured")

// ErrCiphertextInvalid covers all "this string isn't valid v1
// ciphertext" situations at the decrypt boundary: bad prefix, bad
// base64, short nonce, tampered tag. The caller always gets a generic
// failure — never any detail about *why* — so we don't accidentally
// leak oracle information through error messages.
var ErrCiphertextInvalid = errors.New("ciphertext invalid or tampered")

// Cipher wraps the AEAD instance the store uses for every sensitive
// field. A nil *Cipher represents "encryption disabled" — all paths
// through the store call Encrypt/Decrypt via its methods, so the nil
// case can short-circuit without the caller learning the key state.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher initialises an AEAD with the given key. Key MUST be 32
// bytes (AES-256); anything else errors. Nil or zero-length key returns
// (nil, nil) — the caller treats that as "encryption off" and writes
// plaintext.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes (got %d); generate one with `openssl rand -base64 32`", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// LoadCipherFromEnv builds a Cipher from the environment:
//
//   - More than one source set: error — the configuration is ambiguous
//     and the operator should pick one. Checked first, on selector
//     presence alone, so "two sources" is reported ahead of any
//     completeness problem inside one of them.
//   - ACCELERO_ENCRYPTION_KEY_VAULT set: unwrap the key via Vault
//     Transit (needs VAULT_ADDR, VAULT_TOKEN and
//     ACCELERO_VAULT_TRANSIT_KEY; see vault.go). A missing variable,
//     unreachable Vault, or a token that can't decrypt is an error.
//   - ACCELERO_ENCRYPTION_KEY_FILE set: read that file, trim
//     whitespace, base64-decode. Empty file or unreadable path is an
//     error (the operator intended to provide a key and didn't).
//   - ACCELERO_ENCRYPTION_KEY set: base64-decode the inline value.
//   - None set: (nil, nil) — matches NewCipher's "encryption
//     disabled" signal.
//
// Any set-but-malformed value is a startup error rather than a silent
// fallback to plaintext: the operator asked for encryption and we
// should fail loudly.
func LoadCipherFromEnv() (*Cipher, error) {
	inline := strings.TrimSpace(os.Getenv(envKeyName))
	filePath := strings.TrimSpace(os.Getenv(envKeyFileName))

	// The three sources are mutually exclusive: having more than one set is
	// ambiguous, and silently preferring one could mean a key rotation the
	// operator performed is quietly ignored. Judged on *selector presence*
	// only, so "two sources configured" is reported ahead of any
	// completeness problem within one of them — it's the likelier mistake.
	vaultSelected := strings.TrimSpace(os.Getenv(envKeyVaultName)) != ""
	sources := 0
	for _, set := range []bool{inline != "", filePath != "", vaultSelected} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return nil, fmt.Errorf("set only one of %s, %s or %s, not several",
			envKeyName, envKeyFileName, envKeyVaultName)
	}

	vaultCfg, useVault, err := vaultConfigFromEnv()
	if err != nil {
		return nil, err
	}

	if useVault {
		// Bounded: a hung Vault must not wedge startup forever.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return loadCipherFromVault(ctx, vaultCfg)
	}

	if filePath != "" {
		return loadCipherFromFile(filePath)
	}

	if inline == "" {
		return nil, nil
	}

	key, err := base64.StdEncoding.DecodeString(inline)
	if err != nil {
		return nil, fmt.Errorf("%s: base64 decode: %w", envKeyName, err)
	}
	return NewCipher(key)
}

// loadCipherFromFile reads the key from the path given by
// ACCELERO_ENCRYPTION_KEY_FILE. File content is trimmed of surrounding
// whitespace so that common producers (Docker secrets, `echo "..." >
// /path`, K8s projected volumes with trailing newlines) work without
// the operator having to fight quoting. An empty file is an error
// rather than "disabled": the operator pointed us at a file, so a
// missing value means the mount is broken, not that encryption is off.
func loadCipherFromFile(path string) (*Cipher, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", envKeyFileName, path, err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil, fmt.Errorf("%s=%q: file is empty (expected base64-encoded 32-byte key)", envKeyFileName, path)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: base64 decode: %w", envKeyFileName, path, err)
	}
	return NewCipher(key)
}

// Enabled reports whether this Cipher will actually encrypt. Nil
// receivers are safe — treat them as disabled.
func (c *Cipher) Enabled() bool { return c != nil && c.aead != nil }

// Encrypt returns the v1 ciphertext for plaintext. Safe to call on a
// nil receiver: disabled ciphers return the plaintext unchanged so the
// caller can write the raw string to the DB without branching.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if !c.Enabled() {
		return plaintext, nil
	}
	if plaintext == "" {
		// Empty strings stay empty — they're our sentinel for
		// "no value" (no token, no password). Encrypting them would
		// still be valid, but adds noise to the DB and breaks
		// "is this column set" checks.
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ct := c.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return fmt.Sprintf("%s:%s:%s",
		CipherVersionV1,
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(ct),
	), nil
}

// Decrypt returns the plaintext for a v1 ciphertext string.  Strings
// that don't start with "v1:" are treated as legacy plaintext and
// returned as-is — this is the migration path for rows written by
// older accelero versions. Ciphertext with a v1 prefix that fails to
// decrypt (wrong key, tampered, truncated) produces ErrCiphertextInvalid
// with no further detail, so the caller cannot build a padding oracle.
func (c *Cipher) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, CipherVersionV1+":") {
		// Legacy plaintext row. Return as-is; the store's on-write
		// path will re-encrypt if the cipher is enabled.
		return stored, nil
	}
	if !c.Enabled() {
		// A ciphertext row exists in the DB but the server was
		// started without a key. Refuse — returning garbage would
		// surface as a confusing deploy failure later.
		return "", ErrNoKey
	}

	parts := strings.SplitN(stored, ":", 3)
	if len(parts) != 3 {
		return "", ErrCiphertextInvalid
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(nonce) != c.aead.NonceSize() {
		return "", ErrCiphertextInvalid
	}
	ct, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", ErrCiphertextInvalid
	}
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrCiphertextInvalid
	}
	return string(pt), nil
}

// IsPlaintext returns true when stored was written by an older
// version (no v1: prefix). Used by the admin migration endpoint to
// find rows that still need re-encrypting.
func IsPlaintext(stored string) bool {
	return stored != "" && !strings.HasPrefix(stored, CipherVersionV1+":")
}
