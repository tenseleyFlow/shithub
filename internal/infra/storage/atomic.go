// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteAtomic writes src to path via a tempfile in the same directory,
// fsyncs, and renames. A crash between write and rename leaves the temp
// file behind (callers may sweep these on startup) but never a partial
// file at path.
//
// The temp file MUST live on the same mount as path so the rename is
// atomic — callers should not pass paths that cross mount points.
func WriteAtomic(path string, src io.Reader) error {
	dir := filepath.Dir(path)
	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("storage: atomic: random suffix: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp."+suffix)

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("storage: atomic: open temp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("storage: atomic: copy: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("storage: atomic: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("storage: atomic: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("storage: atomic: rename: %w", err)
	}
	return nil
}

func randomSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
