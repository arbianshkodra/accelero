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
// up.
//
// SHA-256 (not bcrypt/scrypt/argon2) is deliberate and correct here: these keys
// are 256-bit values from crypto/rand (see GenerateAPIKey), not human-chosen
// passwords. A slow, salted KDF exists to make low-entropy secrets expensive to
// brute-force after a DB leak; a 256-bit random token is infeasible to brute
// force regardless of hash speed, so a fast hash adds no attack surface while
// keeping per-request auth cheap. This is the same approach GitHub/Stripe use
// for API tokens. (CodeQL flags this as "weak password hashing" — a false
// positive: the input is a random token, not a password.)
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
