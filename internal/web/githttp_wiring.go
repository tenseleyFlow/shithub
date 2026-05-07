// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	githttph "github.com/tenseleyFlow/shithub/internal/web/handlers/githttp"
)

// buildGitHTTPHandlers wires the smart-HTTP route handlers. The bare
// repos live at cfg.Storage.ReposRoot; we refuse to boot the git-HTTP
// surface without it.
func buildGitHTTPHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) (*githttph.Handlers, error) {
	if cfg.Storage.ReposRoot == "" {
		return nil, errors.New("git-http: cfg.Storage.ReposRoot is empty")
	}
	root, err := filepath.Abs(cfg.Storage.ReposRoot)
	if err != nil {
		return nil, fmt.Errorf("git-http: resolve repos_root: %w", err)
	}
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		return nil, fmt.Errorf("git-http: NewRepoFS: %w", err)
	}
	return githttph.New(githttph.Deps{
		Logger: logger,
		Pool:   pool,
		RepoFS: rfs,
	})
}
