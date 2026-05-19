// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

func TestDependencyAdvisoryOSVImportWorkerMarksSuccess(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	path := filepath.Join(t.TempDir(), "osv.json")
	if err := os.WriteFile(path, []byte(`{
		"id": "GHSA-worker-0001",
		"affected": [{"package": {"ecosystem": "Go", "name": "example.com/worker"}, "ranges": [{"events": [{"introduced": "0"}, {"fixed": "1.4.0"}]}]}]
	}`), 0o600); err != nil {
		t.Fatalf("write OSV fixture: %v", err)
	}

	payload := DependencyAdvisoryOSVImportPayload{
		FilePath:    path,
		SourceName:  "osv-worker",
		SourceURL:   "https://osv.example.test",
		License:     "CC-BY-4.0",
		Attribution: "OSV fixture",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	handler := DependencyAdvisoryOSVImport(DependencyAdvisoryOSVImportDeps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := handler(ctx, raw); err != nil {
		t.Fatalf("handler: %v", err)
	}

	q := reposdb.New()
	source, err := q.GetDependencyAdvisorySource(ctx, pool, "osv-worker")
	if err != nil {
		t.Fatalf("GetDependencyAdvisorySource: %v", err)
	}
	if source.LastSyncStatus != "success" || source.LastSyncError != "" {
		t.Fatalf("unexpected source sync state: %+v", source)
	}
	advisory, err := q.GetDependencyAdvisoryBySourceExternalID(ctx, pool, reposdb.GetDependencyAdvisoryBySourceExternalIDParams{
		Source:     "osv-worker",
		ExternalID: "GHSA-worker-0001",
	})
	if err != nil {
		t.Fatalf("GetDependencyAdvisoryBySourceExternalID: %v", err)
	}
	if advisory.PackageName != "example.com/worker" || advisory.AffectedRange != "< 1.4.0" {
		t.Fatalf("unexpected advisory: %+v", advisory)
	}
}

func TestDependencyAdvisoryOSVImportWorkerMarksMissingFileFailed(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	payload := DependencyAdvisoryOSVImportPayload{
		FilePath:   filepath.Join(t.TempDir(), "missing-osv.json"),
		SourceName: "osv-missing",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	handler := DependencyAdvisoryOSVImport(DependencyAdvisoryOSVImportDeps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	err = handler(ctx, raw)
	if !errors.Is(err, worker.ErrPoison) {
		t.Fatalf("expected poison missing-file error, got %v", err)
	}

	source, err := reposdb.New().GetDependencyAdvisorySource(ctx, pool, "osv-missing")
	if err != nil {
		t.Fatalf("GetDependencyAdvisorySource: %v", err)
	}
	if source.LastSyncStatus != "failed" || !strings.Contains(source.LastSyncError, "open OSV import file") {
		t.Fatalf("unexpected source sync failure: %+v", source)
	}
}
