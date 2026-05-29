// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

func TestRepoIndexCodeSkipsOversizeBlobContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "indexer", DisplayName: "Indexer", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "indexed",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	gitDir, err := rfs.RepoPath(user.Username, repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := initBareRepo(gitDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	commitOID, err := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
		Message:     "seed index fixture",
		Branch:      "trunk",
		When:        time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
		Files: []repogit.FileEntry{
			{Path: "small.txt", Body: []byte("searchable needle\n")},
			{Path: "huge.txt", Body: []byte(strings.Repeat("x", 256*1024+1))},
		},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}

	handler := jobs.RepoIndexCode(jobs.IndexCodeDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	payload, _ := json.Marshal(jobs.IndexCodePayload{RepoID: repo.ID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("repo:index_code: %v", err)
	}

	var pathCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM code_search_paths
		WHERE repo_id = $1 AND path IN ('small.txt', 'huge.txt')
	`, repo.ID).Scan(&pathCount); err != nil {
		t.Fatalf("count paths: %v", err)
	}
	if pathCount != 2 {
		t.Fatalf("path count = %d, want 2", pathCount)
	}

	var contentCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM code_search_content
		WHERE repo_id = $1
	`, repo.ID).Scan(&contentCount); err != nil {
		t.Fatalf("count content: %v", err)
	}
	if contentCount != 1 {
		t.Fatalf("content count = %d, want 1", contentCount)
	}

	indexedRepo, err := rq.GetRepoByID(ctx, pool, repo.ID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	if !indexedRepo.LastIndexedOid.Valid || indexedRepo.LastIndexedOid.String != commitOID {
		t.Fatalf("last_indexed_oid = %#v, want %q", indexedRepo.LastIndexedOid, commitOID)
	}
}
