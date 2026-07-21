package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackuper simulates store.Backup by writing marker bytes to destPath.
type fakeBackuper struct {
	err   error
	calls int
}

func (f *fakeBackuper) Backup(_ context.Context, destPath string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(destPath, []byte("snapshot"), 0600)
}

func TestRunOnce_WritesTimestampedSnapshot(t *testing.T) {
	dir := t.TempDir()
	fb := &fakeBackuper{}
	now := time.Date(2026, 7, 19, 3, 4, 5, 0, time.UTC)

	path, err := RunOnce(context.Background(), fb, dir, 7, now, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fb.calls)
	assert.Equal(t, filepath.Join(dir, "accelero-backup-20260719T030405Z.db"), path)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "snapshot", string(b))
}

func TestRunOnce_CreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "backups")
	_, err := RunOnce(context.Background(), &fakeBackuper{}, dir, 0, time.Now(), nil)
	require.NoError(t, err)
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
}

func TestRunOnce_PropagatesBackupError(t *testing.T) {
	dir := t.TempDir()
	_, err := RunOnce(context.Background(), &fakeBackuper{err: assert.AnError}, dir, 7, time.Now(), nil)
	assert.ErrorIs(t, err, assert.AnError)
}

func seedBackups(t *testing.T, dir string, stamps ...string) {
	t.Helper()
	for _, s := range stamps {
		p := filepath.Join(dir, filePrefix+s+fileSuffix)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0600))
	}
}

func TestPrune_KeepsNewestN(t *testing.T) {
	dir := t.TempDir()
	// Five snapshots, chronological by name.
	seedBackups(t, dir,
		"20260101T000000Z", "20260102T000000Z", "20260103T000000Z",
		"20260104T000000Z", "20260105T000000Z")

	removed, err := Prune(dir, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, removed)

	remaining, _ := filepath.Glob(filepath.Join(dir, glob))
	assert.Len(t, remaining, 2)
	// The two newest survive.
	assert.FileExists(t, filepath.Join(dir, "accelero-backup-20260104T000000Z.db"))
	assert.FileExists(t, filepath.Join(dir, "accelero-backup-20260105T000000Z.db"))
	assert.NoFileExists(t, filepath.Join(dir, "accelero-backup-20260101T000000Z.db"))
}

func TestPrune_KeepZeroRetainsAll(t *testing.T) {
	dir := t.TempDir()
	seedBackups(t, dir, "20260101T000000Z", "20260102T000000Z")
	removed, err := Prune(dir, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
	remaining, _ := filepath.Glob(filepath.Join(dir, glob))
	assert.Len(t, remaining, 2)
}

func TestPrune_FewerThanKeepIsNoop(t *testing.T) {
	dir := t.TempDir()
	seedBackups(t, dir, "20260101T000000Z")
	removed, err := Prune(dir, 5)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
}

func TestPrune_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	seedBackups(t, dir, "20260101T000000Z", "20260102T000000Z", "20260103T000000Z")
	// Unrelated files must never be touched.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "accelero.db"), []byte("live"), 0600))

	_, err := Prune(dir, 1)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(dir, "notes.txt"))
	assert.FileExists(t, filepath.Join(dir, "accelero.db"))
	assert.FileExists(t, filepath.Join(dir, "accelero-backup-20260103T000000Z.db"))
}
