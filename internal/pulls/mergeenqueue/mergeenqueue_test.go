// SPDX-License-Identifier: AGPL-3.0-or-later

package mergeenqueue_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls/mergeenqueue"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// S64: ForHeadSHA fans out one pr:mergeability job per open PR whose
// head_oid matches the given SHA. Used by the check-completion trigger.
// Merged or closed PRs are filtered out.
func TestForHeadSHA_EnqueuesPerOpenPR(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	uq := usersdb.New()
	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	iq := issuesdb.New()
	if err := iq.EnsureRepoIssueCounter(ctx, pool, repo.ID); err != nil {
		t.Fatalf("EnsureRepoIssueCounter: %v", err)
	}

	const sharedSHA = "deadbeefcafef00ddeadbeefcafef00ddeadbeef"
	const otherSHA = "0000000000000000000000000000000000000000"

	// Two PRs at sharedSHA, one PR at otherSHA — only the first two
	// should be enqueued.
	var nextNum int64
	openPR := func(headRef, headOID string) int64 {
		nextNum++
		issue, err := iq.CreateIssue(ctx, pool, issuesdb.CreateIssueParams{
			RepoID:       repo.ID,
			Number:       nextNum,
			Kind:         issuesdb.IssueKindPr,
			Title:        "x",
			Body:         "",
			AuthorUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		})
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if _, err := pullsdb.New().CreatePullRequest(ctx, pool, pullsdb.CreatePullRequestParams{
			IssueID: issue.ID, BaseRef: "trunk", HeadRef: headRef, HeadRepoID: repo.ID,
			BaseOid: sharedSHA, HeadOid: headOID, Draft: false,
		}); err != nil {
			t.Fatalf("CreatePullRequest: %v", err)
		}
		return issue.ID
	}
	pr1 := openPR("feature-a", sharedSHA)
	pr2 := openPR("feature-b", sharedSHA)
	_ = openPR("feature-c", otherSHA)

	mergeenqueue.ForHeadSHA(ctx, pool, logger, repo.ID, sharedSHA)

	var n1, n2 int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'pr:mergeability'
		   AND (payload->>'pr_id')::bigint = $1`, pr1).Scan(&n1); err != nil {
		t.Fatalf("count pr1: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'pr:mergeability'
		   AND (payload->>'pr_id')::bigint = $1`, pr2).Scan(&n2); err != nil {
		t.Fatalf("count pr2: %v", err)
	}
	if n1 == 0 || n2 == 0 {
		t.Errorf("expected jobs for pr1=%d and pr2=%d; got %d and %d", pr1, pr2, n1, n2)
	}

	// Verify the other-SHA PR was NOT enqueued.
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'pr:mergeability'`,
	).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if total > n1+n2 {
		t.Errorf("expected only PRs at sharedSHA to enqueue; total=%d (pr1+pr2=%d)", total, n1+n2)
	}
}

// Empty SHA is a no-op — guards against accidental fan-out on rows that
// haven't been snapshotted yet.
func TestForHeadSHA_EmptyHeadSHA(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No PRs needed: just confirms the helper doesn't panic + writes
	// nothing.
	mergeenqueue.ForHeadSHA(context.Background(), pool, logger, 1, "")
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'pr:mergeability'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("empty SHA should not enqueue; got %d jobs", n)
	}
}
