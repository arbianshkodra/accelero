package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPass = "correct horse battery staple"

func TestNewEncryptor_NopVsAge(t *testing.T) {
	nop := NewEncryptor("")
	assert.False(t, nop.Enabled())
	assert.Equal(t, "", nop.Ext())

	enc := NewEncryptor(testPass)
	assert.True(t, enc.Enabled())
	assert.Equal(t, ".age", enc.Ext())
}

func TestNopEncryptor_PassesThrough(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, NewEncryptor("").Encrypt(&out, strings.NewReader("hello")))
	assert.Equal(t, "hello", out.String())
}

func TestAgeEncryptor_RoundTrip(t *testing.T) {
	plaintext := "SQLite format 3\x00...pretend db bytes..."
	var enc bytes.Buffer
	require.NoError(t, NewEncryptor(testPass).Encrypt(&enc, strings.NewReader(plaintext)))

	// Output is the standard age format.
	assert.True(t, bytes.HasPrefix(enc.Bytes(), []byte("age-encryption.org/v1")),
		"should be a standard age stream")
	assert.NotContains(t, enc.String(), "SQLite format 3", "plaintext must not be visible")

	// Decryptable with the passphrase (as `age -d` would).
	id, err := age.NewScryptIdentity(testPass)
	require.NoError(t, err)
	r, err := age.Decrypt(bytes.NewReader(enc.Bytes()), id)
	require.NoError(t, err)
	dec, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, plaintext, string(dec))

	// Wrong passphrase fails.
	badID, _ := age.NewScryptIdentity("wrong")
	_, err = age.Decrypt(bytes.NewReader(enc.Bytes()), badID)
	assert.Error(t, err)
}

func TestRunOnce_AgeEncryptsWhenPassphraseSet(t *testing.T) {
	dir := t.TempDir()
	fb := &fakeBackuper{} // writes "snapshot" as the raw db
	now := time.Date(2026, 7, 19, 3, 4, 5, 0, time.UTC)

	path, err := RunOnce(context.Background(), fb, dir, 7, now, NewEncryptor(testPass))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "accelero-backup-20260719T030405Z.db.age"), path)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(raw, []byte("age-encryption.org/v1")))

	// Decrypts back to the raw snapshot.
	id, _ := age.NewScryptIdentity(testPass)
	r, err := age.Decrypt(bytes.NewReader(raw), id)
	require.NoError(t, err)
	dec, _ := io.ReadAll(r)
	assert.Equal(t, "snapshot", string(dec))

	// No leftover staging/plaintext .db in the backup dir.
	entries, _ := filepath.Glob(filepath.Join(dir, "*"))
	require.Len(t, entries, 1)
	assert.True(t, strings.HasSuffix(entries[0], ".db.age"))
}

func TestPrune_MatchesEncryptedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, filePrefix+s+fileSuffix+".age"), []byte("x"), 0600))
	}
	removed, err := Prune(dir, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	remaining, _ := filepath.Glob(filepath.Join(dir, glob))
	assert.Len(t, remaining, 1)
	assert.FileExists(t, filepath.Join(dir, "accelero-backup-20260103T000000Z.db.age"))
}
