// Package backup writes periodic snapshots of the Accelero database to a
// local directory and prunes old ones. It reuses the store's VACUUM INTO
// backup (see store.Backup) so scheduled and on-demand backups produce
// identical, consistent snapshots. Remote destinations (S3/GCS/SCP) and
// encryption are deliberately out of scope here — this is the local,
// dependency-free core.
package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// filePattern matches the snapshot files this package writes. The
// timestamp format is fixed-width UTC, so lexical sort == chronological.
const (
	filePrefix = "accelero-backup-"
	fileSuffix = ".db"
	tsLayout   = "20060102T150405Z"
	// glob matches both plaintext (.db) and age-encrypted (.db.age) snapshots.
	glob = filePrefix + "*" + fileSuffix + "*"
)

// Backuper is the slice of the store this package depends on.
type Backuper interface {
	Backup(ctx context.Context, destPath string) error
}

// RunOnce writes one timestamped snapshot into dir and then prunes older
// snapshots, keeping the newest `keep` (keep <= 0 keeps all). It returns the
// path of the snapshot just written. `now` is injected so callers (and tests)
// control the filename timestamp. enc transforms the snapshot on the way out
// (age encryption when configured, pass-through otherwise); a nil enc means
// pass-through. The final filename gets enc.Ext() appended (".age" when
// encrypting).
func RunOnce(ctx context.Context, b Backuper, dir string, keep int, now time.Time, enc Encryptor) (string, error) {
	if enc == nil {
		enc = nopEncryptor{}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create backup dir %q: %w", dir, err)
	}
	// Stage the raw VACUUM snapshot outside the backup dir so partial/temp
	// files never sit alongside finished snapshots (and never match the glob).
	stage, err := os.MkdirTemp("", "accelero-backup-stage-")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stage)

	raw := filepath.Join(stage, "snapshot.db")
	if err := b.Backup(ctx, raw); err != nil {
		return "", err
	}

	dest := filepath.Join(dir, filePrefix+now.UTC().Format(tsLayout)+fileSuffix+enc.Ext())
	if err := EncryptFile(raw, dest, enc); err != nil {
		return "", err
	}

	// Pruning is best-effort: a failed prune must not fail the backup that
	// already succeeded.
	_, _ = Prune(dir, keep)
	return dest, nil
}

// EncryptFile streams srcPath into dstPath via the encryptor (0600), cleaning
// up a partial destination on failure. A nil enc passes bytes through
// unchanged. Exported so the HTTP backup handler can reuse the exact same
// snapshot-transform path as the scheduler.
func EncryptFile(srcPath, dstPath string, enc Encryptor) error {
	if enc == nil {
		enc = nopEncryptor{}
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create %q: %w", dstPath, err)
	}
	if err := enc.Encrypt(dst, src); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return err
	}
	return dst.Close()
}

// Prune removes all but the newest `keep` snapshot files in dir and returns
// the number removed. keep <= 0 is a no-op (retain everything). Only files
// matching the accelero-backup naming pattern are ever considered, so it
// won't touch anything else in the directory.
func Prune(dir string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, glob))
	if err != nil {
		return 0, err
	}
	if len(matches) <= keep {
		return 0, nil
	}
	sort.Strings(matches) // fixed-width UTC timestamps sort chronologically
	removed := 0
	for _, f := range matches[:len(matches)-keep] {
		if err := os.Remove(f); err == nil {
			removed++
		}
	}
	return removed, nil
}
