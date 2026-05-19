// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/repos/advisoryimport"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type DependencyAdvisoryOSVImportDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

type DependencyAdvisoryOSVImportPayload struct {
	FilePath    string `json:"file_path"`
	SourceName  string `json:"source_name"`
	SourceURL   string `json:"source_url,omitempty"`
	License     string `json:"license,omitempty"`
	Attribution string `json:"attribution,omitempty"`
	MaxBytes    int64  `json:"max_bytes,omitempty"`
}

// DependencyAdvisoryOSVImport imports an operator-provided OSV JSON file into
// the local advisory catalog. It intentionally never fetches remote advisories.
func DependencyAdvisoryOSVImport(deps DependencyAdvisoryOSVImportDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("dependency advisory OSV import: missing pool")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}

		var p DependencyAdvisoryOSVImportPayload
		if len(raw) == 0 {
			return worker.PoisonError(errors.New("empty payload"))
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		p.SourceName = strings.TrimSpace(p.SourceName)
		if p.SourceName == "" {
			return worker.PoisonError(errors.New("missing source_name"))
		}
		p.FilePath = strings.TrimSpace(p.FilePath)
		if p.FilePath == "" {
			return worker.PoisonError(errors.New("missing file_path"))
		}
		abs, err := filepath.Abs(p.FilePath)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("invalid file_path: %w", err))
		}
		p.FilePath = abs

		result, err := runDependencyAdvisoryOSVImport(ctx, deps.Pool, p)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "dependency advisory OSV import completed",
			"source", p.SourceName,
			"advisory_count", result.AdvisoryCount,
			"upserted_count", result.UpsertedCount,
			"withdrawn_count", result.WithdrawnCount,
			"skipped_count", result.SkippedCount)
		return nil
	}
}

func runDependencyAdvisoryOSVImport(ctx context.Context, pool *pgxpool.Pool, p DependencyAdvisoryOSVImportPayload) (advisoryimport.ImportResult, error) {
	q := reposdb.New()
	sourceURL := strings.TrimSpace(p.SourceURL)
	if sourceURL == "" {
		sourceURL = "https://osv.dev"
	}
	if _, err := q.UpsertDependencyAdvisorySource(ctx, pool, reposdb.UpsertDependencyAdvisorySourceParams{
		Name:           p.SourceName,
		Kind:           "osv",
		DisplayName:    p.SourceName,
		Url:            sourceURL,
		License:        strings.TrimSpace(p.License),
		Attribution:    strings.TrimSpace(p.Attribution),
		Enabled:        true,
		LastSyncStatus: "running",
		LastSyncError:  "",
		Metadata:       syncMetadata(p),
	}); err != nil {
		return advisoryimport.ImportResult{}, fmt.Errorf("upsert advisory source: %w", err)
	}
	run, err := q.StartDependencyAdvisorySyncRun(ctx, pool, reposdb.StartDependencyAdvisorySyncRunParams{
		SourceName: p.SourceName,
		Metadata:   syncMetadata(p),
	})
	if err != nil {
		return advisoryimport.ImportResult{}, fmt.Errorf("start advisory sync run: %w", err)
	}

	result, err := importOSVFile(ctx, pool, q, p, sourceURL)
	if err != nil {
		message := truncateSyncError(err.Error())
		_, _ = q.FinishDependencyAdvisorySyncRun(ctx, pool, reposdb.FinishDependencyAdvisorySyncRunParams{
			ID:             run.ID,
			Status:         "failed",
			AdvisoryCount:  int32(result.AdvisoryCount),
			UpsertedCount:  int32(result.UpsertedCount),
			WithdrawnCount: int32(result.WithdrawnCount),
			ErrorMessage:   message,
			Metadata:       syncMetadata(p),
		})
		_ = q.MarkDependencyAdvisorySourceSync(ctx, pool, reposdb.MarkDependencyAdvisorySourceSyncParams{
			Name:           p.SourceName,
			LastSyncStatus: "failed",
			LastSyncError:  message,
		})
		if errors.Is(err, os.ErrNotExist) {
			return result, worker.PoisonError(err)
		}
		return result, err
	}

	_, err = q.FinishDependencyAdvisorySyncRun(ctx, pool, reposdb.FinishDependencyAdvisorySyncRunParams{
		ID:             run.ID,
		Status:         "success",
		AdvisoryCount:  int32(result.AdvisoryCount),
		UpsertedCount:  int32(result.UpsertedCount),
		WithdrawnCount: int32(result.WithdrawnCount),
		ErrorMessage:   "",
		Metadata:       syncMetadata(p),
	})
	if err != nil {
		return result, fmt.Errorf("finish advisory sync run: %w", err)
	}
	if err := q.MarkDependencyAdvisorySourceSync(ctx, pool, reposdb.MarkDependencyAdvisorySourceSyncParams{
		Name:           p.SourceName,
		LastSyncStatus: "success",
		LastSyncError:  "",
	}); err != nil {
		return result, fmt.Errorf("mark advisory source sync: %w", err)
	}
	return result, nil
}

func importOSVFile(ctx context.Context, pool *pgxpool.Pool, q *reposdb.Queries, p DependencyAdvisoryOSVImportPayload, sourceURL string) (advisoryimport.ImportResult, error) {
	f, err := os.Open(p.FilePath)
	if err != nil {
		return advisoryimport.ImportResult{}, fmt.Errorf("open OSV import file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return advisoryimport.ImportResult{}, fmt.Errorf("stat OSV import file: %w", err)
	}
	if info.IsDir() {
		return advisoryimport.ImportResult{}, worker.PoisonError(errors.New("OSV import path is a directory"))
	}
	return advisoryimport.ImportOSVTransactional(ctx, pool, q, f, advisoryimport.ImportOptions{
		SourceName:  p.SourceName,
		SourceURL:   sourceURL,
		License:     p.License,
		Attribution: p.Attribution,
		MaxBytes:    p.MaxBytes,
	})
}

func syncMetadata(p DependencyAdvisoryOSVImportPayload) []byte {
	body, err := json.Marshal(map[string]any{
		"file_path": p.FilePath,
		"max_bytes": p.MaxBytes,
	})
	if err != nil {
		return []byte("{}")
	}
	return body
}

func truncateSyncError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 2000 {
		return message
	}
	return message[:2000]
}
