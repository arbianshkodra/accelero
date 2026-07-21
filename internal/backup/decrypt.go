package backup

import (
	"bytes"
	"fmt"
	"io"

	"filippo.io/age"
)

// ageMagic is the header every age stream starts with.
var ageMagic = []byte("age-encryption.org/v1")

// IsAgeEncrypted reports whether b looks like an age stream (by header).
func IsAgeEncrypted(b []byte) bool {
	return bytes.HasPrefix(b, ageMagic)
}

// Decrypt streams src to dst, decrypting an age scrypt-passphrase stream with
// the given passphrase. It is the inverse of the age path in Encryptor.
func Decrypt(dst io.Writer, src io.Reader, passphrase string) error {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return fmt.Errorf("age identity: %w", err)
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		return fmt.Errorf("age decrypt (wrong passphrase or not an age file): %w", err)
	}
	if _, err := io.Copy(dst, r); err != nil {
		return fmt.Errorf("age read: %w", err)
	}
	return nil
}
