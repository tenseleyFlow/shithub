// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workspace owns shithubd-runner's on-disk job workspace layout.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Manager struct {
	Root string
}

func New(root string) Manager {
	return Manager{Root: root}
}

func (m Manager) Prepare(runID, jobID int64) (string, error) {
	dir := JobDir(m.Root, runID, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runner workspace: prepare %s: %w", dir, err)
	}
	return dir, nil
}

func (m Manager) Remove(runID, jobID int64) error {
	dir := JobDir(m.Root, runID, jobID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("runner workspace: remove %s: %w", dir, err)
	}
	_ = os.Remove(filepath.Dir(dir))
	return nil
}

func (m Manager) Sweep(ttl time.Duration, now time.Time) (int, error) {
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return 0, fmt.Errorf("runner workspace: create root: %w", err)
	}
	cutoff := now.Add(-ttl)
	runDirs, err := os.ReadDir(m.Root)
	if err != nil {
		return 0, fmt.Errorf("runner workspace: read root: %w", err)
	}
	removed := 0
	for _, runDir := range runDirs {
		if !runDir.IsDir() {
			continue
		}
		runPath := filepath.Join(m.Root, runDir.Name())
		jobDirs, err := os.ReadDir(runPath)
		if err != nil {
			return removed, fmt.Errorf("runner workspace: read %s: %w", runPath, err)
		}
		for _, jobDir := range jobDirs {
			if !jobDir.IsDir() {
				continue
			}
			jobPath := filepath.Join(runPath, jobDir.Name())
			info, err := jobDir.Info()
			if err != nil {
				return removed, fmt.Errorf("runner workspace: stat %s: %w", jobPath, err)
			}
			if info.ModTime().After(cutoff) {
				continue
			}
			if err := os.RemoveAll(jobPath); err != nil {
				return removed, fmt.Errorf("runner workspace: sweep %s: %w", jobPath, err)
			}
			removed++
		}
		_ = os.Remove(runPath)
	}
	return removed, nil
}

func JobDir(root string, runID, jobID int64) string {
	return filepath.Join(root, strconv.FormatInt(runID, 10), strconv.FormatInt(jobID, 10))
}
