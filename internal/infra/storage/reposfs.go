// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// RepoFS owns the on-disk layout for bare git repositories. All callers
// that touch repo paths route through this type so the path-validation
// rules live in exactly one place.
type RepoFS struct {
	root string
}

// NewRepoFS validates root (must be absolute, must exist, must be a
// directory) and returns the layer.
func NewRepoFS(root string) (*RepoFS, error) {
	if root == "" {
		return nil, errors.New("storage: repofs: root required")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("storage: repofs: root must be absolute, got %q", root)
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("storage: repofs: clean root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("storage: repofs: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("storage: repofs: root %q is not a directory", abs)
	}
	return &RepoFS{root: abs}, nil
}

// Root returns the absolute root path. Useful for logging and `storage check`.
func (r *RepoFS) Root() string { return r.root }

// ownerNameRE is the whitelist for owner names: lowercase ASCII letters,
// digits, and hyphens; cannot start or end with a hyphen; length 1..39
// (matches GitHub's username constraint).
var ownerNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

// repoNameRE is the whitelist for repository names: lowercase ASCII
// letters, digits, hyphens, dots, and underscores. Can't start or end
// with a separator. Length 1..100 (matches GitHub).
var repoNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9_])?$`)

// validateName enforces the per-kind whitelist. Returns ErrInvalidPath
// wrapped with a precise reason on failure.
func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s empty", ErrInvalidPath, kind)
	}
	maxLen, re, alphabet := 39, ownerNameRE, "[a-z0-9-]"
	if kind == "repo" {
		maxLen, re, alphabet = 100, repoNameRE, "[a-z0-9._-]"
	}
	if len(name) > maxLen {
		return fmt.Errorf("%w: %s %q too long (max %d)", ErrInvalidPath, kind, name, maxLen)
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("%w: %s %q must be lowercase", ErrInvalidPath, kind, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: %s contains dot-dot", ErrInvalidPath, kind)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %s starts with dot", ErrInvalidPath, kind)
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("%w: %s is absolute", ErrInvalidPath, kind)
	}
	if !re.MatchString(name) {
		return fmt.Errorf("%w: %s %q fails whitelist %s", ErrInvalidPath, kind, name, alphabet)
	}
	return nil
}

// shardOf returns the two-character shard prefix for owner. When owner is
// shorter than two characters, pads with `_` so the path remains stable.
func shardOf(owner string) string {
	switch len(owner) {
	case 0:
		return "__"
	case 1:
		return owner + "_"
	default:
		return owner[:2]
	}
}

// RepoPath returns the absolute disk path for the bare repository at
// (owner, name). Validates inputs and guarantees the result is rooted at
// r.root. Both inputs are lowercased before path construction.
func (r *RepoFS) RepoPath(owner, name string) (string, error) {
	owner = strings.ToLower(owner)
	name = strings.ToLower(name)
	if err := validateName("owner", owner); err != nil {
		return "", err
	}
	if err := validateName("repo", name); err != nil {
		return "", err
	}
	p := filepath.Join(r.root, shardOf(owner), owner, name+".git")
	if err := r.containedInRoot(p); err != nil {
		return "", err
	}
	return p, nil
}

// containedInRoot returns ErrEscapesRoot when p does not resolve under r.root.
// Defense-in-depth: validateName already rejects ".." and absolute paths,
// but a future caller might compose paths differently.
func (r *RepoFS) containedInRoot(p string) error {
	clean := filepath.Clean(p)
	if !strings.HasPrefix(clean, r.root+string(filepath.Separator)) && clean != r.root {
		return fmt.Errorf("%w: %s not under %s", ErrEscapesRoot, clean, r.root)
	}
	return nil
}

// Exists reports whether path exists. Validates that path is under root.
func (r *RepoFS) Exists(path string) (bool, error) {
	if err := r.containedInRoot(path); err != nil {
		return false, err
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("storage: repofs: stat %s: %w", path, err)
}

// InitBare creates a bare git repository at path. Default branch is
// "trunk" — there is no path through this package that creates a bare
// repo with a different initial branch.
//
// The parent directory tree is created on demand. ErrAlreadyExists is
// returned if path is non-empty.
func (r *RepoFS) InitBare(ctx context.Context, path string) error {
	if err := r.containedInRoot(path); err != nil {
		return err
	}
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("storage: repofs: mkdir parent: %w", err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("storage: repofs: mkdir target: %w", err)
	}
	// G204: path is constructed via RepoPath (strict whitelist) and verified
	// to live under r.root. Caller cannot inject arbitrary args.
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", "--initial-branch=trunk", path) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("storage: repofs: git init --bare: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Move atomically renames oldPath to newPath. Both must be under root.
// If newPath already exists, returns ErrAlreadyExists rather than
// overwriting (avoids silent corruption on concurrent moves).
func (r *RepoFS) Move(oldPath, newPath string) error {
	if err := r.containedInRoot(oldPath); err != nil {
		return err
	}
	if err := r.containedInRoot(newPath); err != nil {
		return err
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, newPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: repofs: stat dest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o750); err != nil {
		return fmt.Errorf("storage: repofs: mkdir parent: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("storage: repofs: rename: %w", err)
	}
	return nil
}

// Delete removes the bare repo at path. Refuses paths outside root.
func (r *RepoFS) Delete(path string) error {
	if err := r.containedInRoot(path); err != nil {
		return err
	}
	if path == r.root {
		return fmt.Errorf("%w: refusing to delete root", ErrEscapesRoot)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("storage: repofs: remove: %w", err)
	}
	return nil
}
