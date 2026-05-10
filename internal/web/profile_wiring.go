// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	profileh "github.com/tenseleyFlow/shithub/internal/web/handlers/profile"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildObjectStore returns an S3-backed ObjectStore when the config
// supplies a bucket name, else nil. Avatar uploads (S10) are silently
// disabled when this is nil.
func buildObjectStore(s config.S3StorageConfig, logger *slog.Logger) (storage.ObjectStore, error) {
	if s.Bucket == "" {
		logger.Info("storage: no bucket configured; avatar uploads disabled")
		return nil, nil
	}
	return storage.NewS3Store(storage.S3Config{
		Endpoint:        s.Endpoint,
		Region:          s.Region,
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
		Bucket:          s.Bucket,
		UseSSL:          s.UseSSL,
		ForcePathStyle:  s.ForcePathStyle,
	})
}

// buildProfileHandlers constructs the read-only profile handler set.
// objectStore may be nil — handlers fall back to the identicon path.
func buildProfileHandlers(
	cfg config.Config, pool *pgxpool.Pool, objectStore storage.ObjectStore, tmplFS fs.FS,
	logger *slog.Logger,
) (*profileh.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	var repoFS *storage.RepoFS
	if cfg.Storage.ReposRoot != "" {
		root, err := filepath.Abs(cfg.Storage.ReposRoot)
		if err != nil {
			return nil, err
		}
		repoFS, err = storage.NewRepoFS(root)
		if err != nil {
			return nil, err
		}
	}
	return profileh.New(profileh.Deps{
		Logger:      logger,
		Render:      rr,
		Pool:        pool,
		RepoFS:      repoFS,
		ObjectStore: objectStore,
	})
}
