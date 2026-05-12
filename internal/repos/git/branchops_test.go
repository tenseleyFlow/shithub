// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestDeleteBranchUsesExpectedOID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gitDir := initBare(t)
	when := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	base, err := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.com",
		Branch:      "trunk",
		When:        when,
		Files:       []repogit.FileEntry{{Path: "README.md", Body: []byte("base\n")}},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("Build base: %v", err)
	}
	if out, err := gitCmd("-C", gitDir, "update-ref", "refs/heads/topic", base).CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v\n%s", err, out)
	}

	if err := repogit.DeleteBranch(ctx, gitDir, "topic", strings.Repeat("f", 40)); !errors.Is(err, repogit.ErrRefRaced) {
		t.Fatalf("DeleteBranch stale oid = %v, want ErrRefRaced", err)
	}
	if _, err := repogit.ResolveRefOID(ctx, gitDir, "refs/heads/topic"); err != nil {
		t.Fatalf("topic should still exist after stale delete: %v", err)
	}

	if err := repogit.DeleteBranch(ctx, gitDir, "topic", base); err != nil {
		t.Fatalf("DeleteBranch current oid: %v", err)
	}
	if _, err := repogit.ResolveRefOID(ctx, gitDir, "refs/heads/topic"); !errors.Is(err, repogit.ErrRefNotFound) {
		t.Fatalf("topic lookup after delete = %v, want ErrRefNotFound", err)
	}
}
