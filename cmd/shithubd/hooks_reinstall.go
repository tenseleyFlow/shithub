// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/git/hooks"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// runHooksReinstall is the implementation of `shithubd hooks reinstall`.
// It owns the deployment-time bootstrap of hook shims — call after
// upgrading the binary so every repo's hook scripts point at the new
// path.
func runHooksReinstall(ctx context.Context, all bool, repoArg string, out io.Writer) error {
	cfg, err := config.Load(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	root, err := filepath.Abs(cfg.Storage.ReposRoot)
	if err != nil || root == "" {
		return fmt.Errorf("repos_root unset")
	}
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		return fmt.Errorf("repo fs: %w", err)
	}
	binPath, err := shithubdBinaryPath()
	if err != nil {
		return fmt.Errorf("binary path: %w", err)
	}

	if !all {
		owner, name, ok := strings.Cut(repoArg, "/")
		if !ok || owner == "" || name == "" {
			return errors.New("hooks reinstall: --repo wants owner/name")
		}
		gitDir, err := rfs.RepoPath(owner, name)
		if err != nil {
			return fmt.Errorf("repo path: %w", err)
		}
		if err := hooks.Install(gitDir, binPath); err != nil {
			return fmt.Errorf("install: %w", err)
		}
		fmt.Fprintf(out, "ok: %s/%s\n", owner, name)
		return nil
	}

	// --all: enumerate via DB.
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := db.Open(dbCtx, db.Config{URL: cfg.DB.URL, MaxConns: 4})
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer pool.Close()
	rq := reposdb.New()
	rows, err := rq.ListAllRepoFullNames(dbCtx, pool)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	var ok, fail int
	for _, r := range rows {
		gitDir, err := rfs.RepoPath(r.OwnerUsername, r.Name)
		if err != nil {
			fmt.Fprintf(out, "skip: %s/%s: %v\n", r.OwnerUsername, r.Name, err)
			fail++
			continue
		}
		if err := hooks.Install(gitDir, binPath); err != nil {
			fmt.Fprintf(out, "fail: %s/%s: %v\n", r.OwnerUsername, r.Name, err)
			fail++
			continue
		}
		ok++
	}
	fmt.Fprintf(out, "reinstalled hooks on %d repos (%d failed)\n", ok, fail)
	if fail > 0 {
		return fmt.Errorf("%d failures", fail)
	}
	return nil
}

// shithubdBinaryPath returns the absolute path of the running binary.
// hooks.Install bakes this into the shim so every push exec's the same
// version that wrote the hook.
func shithubdBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}
