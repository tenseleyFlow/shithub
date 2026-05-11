// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareAndRemove(t *testing.T) {
	t.Parallel()
	m := New(t.TempDir())
	dir, err := m.Prepare(10, 20)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if dir != filepath.Join(m.Root, "10", "20") {
		t.Fatalf("dir: %s", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := m.Remove(10, 20); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Stat after remove: %v", err)
	}
}

func TestSweepRemovesExpiredJobWorkspaces(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	m := New(t.TempDir())
	stale, err := m.Prepare(1, 1)
	if err != nil {
		t.Fatalf("Prepare stale: %v", err)
	}
	fresh, err := m.Prepare(1, 2)
	if err != nil {
		t.Fatalf("Prepare fresh: %v", err)
	}
	if err := os.Chtimes(stale, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("Chtimes stale: %v", err)
	}
	if err := os.Chtimes(fresh, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("Chtimes fresh: %v", err)
	}
	removed, err := m.Sweep(24*time.Hour, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed: %d", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale still exists: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh stat: %v", err)
	}
}
