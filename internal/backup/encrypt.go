package backup

import (
	"fmt"
	"io"

	"filippo.io/age"
)

// Encryptor wraps a backup snapshot as it is written out. The default
// (NewEncryptor with an empty passphrase) is a pass-through; a non-empty
// passphrase produces an age-encrypted stream (scrypt recipient), which is
// the standard age format — decryptable anywhere with `age -d` and the
// passphrase, so a backup stays restorable even without Accelero.
type Encryptor interface {
	// Encrypt copies src to dst, transforming it as configured.
	Encrypt(dst io.Writer, src io.Reader) error
	// Ext is the filename suffix to append (".age" when encrypting, "" otherwise).
	Ext() string
	// Enabled reports whether output is actually encrypted.
	Enabled() bool
}

// NewEncryptor returns an age passphrase Encryptor when passphrase is
// non-empty, otherwise a pass-through Encryptor.
func NewEncryptor(passphrase string) Encryptor {
	if passphrase == "" {
		return nopEncryptor{}
	}
	return ageEncryptor{passphrase: passphrase}
}

type nopEncryptor struct{}

func (nopEncryptor) Encrypt(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}
func (nopEncryptor) Ext() string   { return "" }
func (nopEncryptor) Enabled() bool { return false }

type ageEncryptor struct{ passphrase string }

func (a ageEncryptor) Encrypt(dst io.Writer, src io.Reader) error {
	recipient, err := age.NewScryptRecipient(a.passphrase)
	if err != nil {
		return fmt.Errorf("age recipient: %w", err)
	}
	w, err := age.Encrypt(dst, recipient)
	if err != nil {
		return fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := io.Copy(w, src); err != nil {
		_ = w.Close()
		return fmt.Errorf("age write: %w", err)
	}
	return w.Close() // finalizes the age stream
}
func (ageEncryptor) Ext() string   { return ".age" }
func (ageEncryptor) Enabled() bool { return true }
