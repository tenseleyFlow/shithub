// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repoh "github.com/tenseleyFlow/shithub/internal/web/handlers/repo"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildRepoHandlers wires the repo-create + empty-home handlers. The
// bare repos live at cfg.Storage.ReposRoot (must be set; we refuse to
// boot the repo surface without it).
func buildRepoHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	tmplFS fs.FS,
	logger *slog.Logger,
) (*repoh.Handlers, error) {
	if cfg.Storage.ReposRoot == "" {
		return nil, errors.New("repo: cfg.Storage.ReposRoot is empty")
	}
	root, err := filepath.Abs(cfg.Storage.ReposRoot)
	if err != nil {
		return nil, fmt.Errorf("repo: resolve repos_root: %w", err)
	}
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		return nil, fmt.Errorf("repo: NewRepoFS: %w", err)
	}
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, fmt.Errorf("repo: render.New: %w", err)
	}
	// shithubdPath is the running binary, baked into hook shims by
	// repos.Create. os.Executable can rarely fail (e.g. exec name
	// stripped); when it does we fall back to "shithubd" on PATH so
	// hook shims still resolve in unusual environments.
	shithubdPath := "shithubd"
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			shithubdPath = abs
		}
	}

	return repoh.New(repoh.Deps{
		Logger:       logger,
		Render:       rr,
		Pool:         pool,
		RepoFS:       rfs,
		Audit:        audit.NewRecorder(),
		Limiter:      throttle.NewLimiter(),
		ShithubdPath: shithubdPath,
		CloneURLs: repoh.CloneURLs{
			BaseURL:    cfg.Auth.BaseURL,
			SSHEnabled: false, // S12/S13 will flip this when SSH service ships.
		},
	})
}
