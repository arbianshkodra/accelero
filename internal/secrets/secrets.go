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
//   - Master key sources: env var ACCELERO_ENCRYPTION_KEY, base64-
//     encoded 32 bytes. Absent → store falls back to plaintext reads
//     and writes, with a loud warning at startup (see cmd/main.go).
//     This keeps existing deployments working through an upgrade while
//     telling the operator they aren't getting encryption until they
//     set the key.
//
//   - Future key rotation is handled by the v1 prefix: v2 ciphertext
//     can be produced by a newer cipher, and the decrypt path picks
//     the right algorithm from the prefix. Not implemented yet because
//     there's nothing to rotate to.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// envKeyName is the environment variable the master key is read from.
// Exposed so cmd/main.go can reference it from startup logs and the
// deprecation warning stays in sync with the docs.
const envKeyName = "ACCELERO_ENCRYPTION_KEY"

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

// LoadCipherFromEnv reads ACCELERO_ENCRYPTION_KEY (base64-encoded 32
// bytes) and returns a Cipher, or (nil, nil) if the variable is
// unset — matches NewCipher's "encryption disabled" signal. A set-but-
// malformed key is a startup error: the operator asked for encryption
// and we should fail loudly rather than silently fall back to plaintext.
func LoadCipherFromEnv() (*Cipher, error) {
	raw := strings.TrimSpace(os.Getenv(envKeyName))
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: base64 decode: %w", envKeyName, err)
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
