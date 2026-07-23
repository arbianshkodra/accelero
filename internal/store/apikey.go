package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// apiKeyPrefix marks Accelero-issued keys so they're recognisable in configs
// and logs (the secret part follows).
const apiKeyPrefix = "acc_"

// GenerateAPIKey returns a new random API key (256 bits of entropy, hex-encoded
// behind the acc_ prefix). The returned string is the plaintext shown to the
// caller once; only its hash is persisted.
func GenerateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return apiKeyPrefix + hex.EncodeToString(b), nil
}

// HashAPIKey returns the hex SHA-256 of a raw key — the value stored and looked
// up. Keys are high-entropy, so a plain hash lookup is safe (no timing oracle
// on a value an attacker can't guess byte-by-byte).
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
