package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCipher constructs a Cipher with a fresh random key for a
// single test. Avoids any chance of cross-test state or accidental
// reliance on a fixed key.
func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	c, err := NewCipher(key)
	require.NoError(t, err)
	require.NotNil(t, c)
	return c
}

func TestNewCipher_RejectsShortKeys(t *testing.T) {
	_, err := NewCipher(make([]byte, 16))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestNewCipher_NilOrEmptyMeansDisabled(t *testing.T) {
	// The "encryption disabled" signal: caller gets (nil, nil).
	c, err := NewCipher(nil)
	require.NoError(t, err)
	assert.Nil(t, c)
	assert.False(t, c.Enabled())

	c, err = NewCipher([]byte{})
	require.NoError(t, err)
	assert.Nil(t, c)
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c := newTestCipher(t)

	cases := []string{
		"hello world",
		"",                                  // empty stays empty
		"unicode: 世界 🔒",
		strings.Repeat("A", 10000),          // something big enough to span GCM chunks
		`{"token":"gh_p_xxx","user":"bob"}`, // structured data
	}
	for _, pt := range cases {
		t.Run(pt, func(t *testing.T) {
			ct, err := c.Encrypt(pt)
			require.NoError(t, err)
			if pt == "" {
				assert.Equal(t, "", ct, "empty plaintext → empty ciphertext (no prefix)")
				return
			}
			assert.True(t, strings.HasPrefix(ct, "v1:"), "ct must carry the version prefix; got %q", ct[:min(10, len(ct))])

			back, err := c.Decrypt(ct)
			require.NoError(t, err)
			assert.Equal(t, pt, back)
		})
	}
}

func TestEncrypt_NonDeterministic(t *testing.T) {
	// Two encryptions of the same plaintext must produce different
	// ciphertexts (nonce must be random). Otherwise an attacker
	// with access to the DB could see which stacks share the same
	// token.
	c := newTestCipher(t)
	a, err := c.Encrypt("same-plaintext")
	require.NoError(t, err)
	b, err := c.Encrypt("same-plaintext")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "nonces must be per-message, not fixed")
}

func TestDecrypt_LegacyPlaintextPassesThrough(t *testing.T) {
	// Pre-encryption rows don't carry the v1: prefix. They should
	// round-trip as-is so existing deployments keep working after
	// the upgrade.
	c := newTestCipher(t)
	got, err := c.Decrypt("gh_p_legacyplain")
	require.NoError(t, err)
	assert.Equal(t, "gh_p_legacyplain", got)
}

func TestDecrypt_CiphertextWithoutKeyFailsClosed(t *testing.T) {
	// If the DB has v1: rows but the server was started without a
	// key, the store MUST NOT return garbage — it must fail. Returning
	// raw ciphertext would surface as confusing "git auth failed"
	// errors later.
	c := newTestCipher(t)
	ct, err := c.Encrypt("something-secret")
	require.NoError(t, err)

	var disabled *Cipher
	_, err = disabled.Decrypt(ct)
	require.ErrorIs(t, err, ErrNoKey)
}

func TestDecrypt_TamperedBytesRejected(t *testing.T) {
	c := newTestCipher(t)
	ct, err := c.Encrypt("original")
	require.NoError(t, err)

	// Flip a byte in the base64 payload.
	parts := strings.Split(ct, ":")
	require.Len(t, parts, 3)
	// Decode ct portion, flip last byte, re-encode.
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0x01
	parts[2] = base64.StdEncoding.EncodeToString(raw)
	tampered := strings.Join(parts, ":")

	_, err = c.Decrypt(tampered)
	require.ErrorIs(t, err, ErrCiphertextInvalid,
		"GCM authentication must catch single-bit flips")
}

func TestDecrypt_WrongKeyRejected(t *testing.T) {
	a := newTestCipher(t)
	b := newTestCipher(t)
	ct, err := a.Encrypt("x")
	require.NoError(t, err)
	_, err = b.Decrypt(ct)
	require.ErrorIs(t, err, ErrCiphertextInvalid)
}

func TestDecrypt_MalformedPrefixes(t *testing.T) {
	c := newTestCipher(t)

	// "v1:" prefix but the rest is junk.
	cases := []string{
		"v1:not-base64:not-base64",
		"v1::",
		"v1:only-one-section",
		"v1:" + base64.StdEncoding.EncodeToString([]byte("short")) + ":" + base64.StdEncoding.EncodeToString([]byte("short")),
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := c.Decrypt(s)
			require.ErrorIs(t, err, ErrCiphertextInvalid)
		})
	}
}

func TestIsPlaintext(t *testing.T) {
	assert.True(t, IsPlaintext("gh_p_legacyplain"))
	assert.True(t, IsPlaintext("random-text"))
	assert.False(t, IsPlaintext(""))
	assert.False(t, IsPlaintext("v1:nonce:ct"))
}

func TestLoadCipherFromEnv_EmptyMeansDisabled(t *testing.T) {
	t.Setenv(envKeyName, "")
	c, err := LoadCipherFromEnv()
	require.NoError(t, err)
	assert.Nil(t, c)
}

func TestLoadCipherFromEnv_ValidKey(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(key))
	c, err := LoadCipherFromEnv()
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.True(t, c.Enabled())
}

func TestLoadCipherFromEnv_MalformedFailsLoudly(t *testing.T) {
	// Setting the env but botching the value should fail, not fall
	// back to plaintext. The operator asked for encryption; honour it.
	t.Setenv(envKeyName, "not-valid-base64-!@#$")
	_, err := LoadCipherFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), envKeyName)
}

func TestLoadCipherFromEnv_WrongKeySizeFails(t *testing.T) {
	// Valid base64 but decodes to 16 bytes instead of 32.
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(make([]byte, 16)))
	_, err := LoadCipherFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
