// SPDX-License-Identifier: AGPL-3.0-or-later

package review_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func gitCmd(args ...string) *exec.Cmd {
	//nolint:gosec
	return exec.Command("git", args...)
}

type fx struct {
	pool        *pgxpool.Pool
	pullsDeps   pulls.Deps
	reviewDeps  review.Deps
	authorID    int64
	reviewerID  int64
	otherID     int64
	repoID      int64
	gitDir      string
}

func setup(t *testing.T) fx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	mkUser := func(name string) usersdb.User {
		u, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
			Username: name, DisplayName: strings.ToTitle(name), PasswordHash: fixtureHash,
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		em, err := uq.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
			UserID: u.ID, Email: name + "@example.com", IsPrimary: true, Verified: true,
		})
		if err != nil {
			t.Fatalf("CreateUserEmail: %v", err)
		}
		if err := uq.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
			ID: u.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
		}); err != nil {
			t.Fatalf("LinkUserPrimaryEmail: %v", err)
		}
		return u
	}
	author := mkUser("alice")
	reviewer := mkUser("bob")
	other := mkUser("carol")

	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: author.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := issuesdb.New().EnsureRepoIssueCounter(ctx, pool, repo.ID); err != nil {
		t.Fatalf("EnsureRepoIssueCounter: %v", err)
	}

	root := t.TempDir()
	gitDir := filepath.Join(root, "demo.git")
	if out, err := gitCmd("init", "--bare", "-b", "trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return fx{
		pool: pool,
		pullsDeps:  pulls.Deps{Pool: pool, Logger: logger},
		reviewDeps: review.Deps{Pool: pool, Logger: logger},
		authorID:   author.ID,
		reviewerID: reviewer.ID,
		otherID:    other.ID,
		repoID:     repo.ID,
		gitDir:     gitDir,
	}
}

func commitOnBranch(t *testing.T, gitDir, branch, msg, file, contents string) string {
	t.Helper()
	wt := t.TempDir()
	addArgs := []string{"-C", gitDir, "worktree", "add"}
	if _, err := gitCmd("-C", gitDir, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err != nil {
		addArgs = append(addArgs, "-b", branch, wt)
	} else {
		addArgs = append(addArgs, wt, branch)
	}
	if out, err := gitCmd(addArgs...).CombinedOutput(); err != nil {
		t.Fatalf("worktree add %s: %v (%s)", branch, err, out)
	}
	defer func() {
		_ = gitCmd("-C", gitDir, "worktree", "remove", "--force", wt).Run()
	}()
	if err := os.WriteFile(filepath.Join(wt, file), []byte(contents), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write %s: %v", file, err)
	}
	for _, args := range [][]string{
		{"-C", wt, "config", "user.name", "Alice"},
		{"-C", wt, "config", "user.email", "alice@example.com"},
		{"-C", wt, "add", "."},
		{"-C", wt, "commit", "-m", msg},
	} {
		if out, err := gitCmd(args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	out, err := gitCmd("-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// openPR opens a same-repo PR and runs Mergeability so the state is
// fresh.
func (f fx) openPR(t *testing.T, base, head string) pullsdb.PullRequest {
	t.Helper()
	res, err := pulls.Create(context.Background(), f.pullsDeps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.authorID,
		Title: "test PR", BaseRef: base, HeadRef: head, GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("openPR: %v", err)
	}
	if err := pulls.Mergeability(context.Background(), f.pullsDeps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	return res.PullRequest
}

func TestSubmit_AuthorCannotApprove(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	_, err := review.Submit(context.Background(), f.reviewDeps, review.SubmitParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.authorID,
		State: "approve", PRAuthorUserID: f.authorID,
	})
	if err == nil {
		t.Fatalf("expected ErrAuthorCannotApprove, got nil")
	}
}

func TestSubmit_AttachesPendingComments(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	// Two pending draft comments by the reviewer.
	if _, err := review.AddComment(ctx, f.reviewDeps, review.CommentParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		FilePath: "x.txt", Side: "right", OriginalCommitSHA: pr.HeadOid,
		OriginalLine: 1, OriginalPosition: 1, CurrentPosition: 1,
		Body: "first draft", Pending: true,
	}); err != nil {
		t.Fatalf("AddComment 1: %v", err)
	}
	if _, err := review.AddComment(ctx, f.reviewDeps, review.CommentParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		FilePath: "x.txt", Side: "right", OriginalCommitSHA: pr.HeadOid,
		OriginalLine: 1, OriginalPosition: 1, CurrentPosition: 1,
		Body: "second draft", Pending: true,
	}); err != nil {
		t.Fatalf("AddComment 2: %v", err)
	}

	rv, err := review.Submit(ctx, f.reviewDeps, review.SubmitParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		State: "comment", Body: "review body", PRAuthorUserID: f.authorID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// All pending comments should now have review_id = rv.ID and pending=false.
	cs, _ := pullsdb.New().ListPRReviewComments(ctx, f.pool, pr.IssueID)
	for _, c := range cs {
		if c.Pending {
			t.Errorf("comment %d still pending after submit", c.ID)
		}
		if !c.ReviewID.Valid || c.ReviewID.Int64 != rv.ID {
			t.Errorf("comment %d review_id=%v, want %d", c.ID, c.ReviewID, rv.ID)
		}
	}
}

func TestRequiredReviews_BlocksThenUnblocks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	// Add a protection rule on trunk requiring 1 approval.
	rq := reposdb.New()
	id, err := rq.UpsertBranchProtectionRule(ctx, f.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: f.repoID, Pattern: "trunk",
		PreventForcePush: true, PreventDeletion: true, RequirePrForPush: false,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	})
	if err != nil {
		t.Fatalf("UpsertBranchProtectionRule: %v", err)
	}
	if err := rq.UpdateBranchProtectionReviewSettings(ctx, f.pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
		ID: id, RequiredReviewCount: 1,
	}); err != nil {
		t.Fatalf("UpdateBranchProtectionReviewSettings: %v", err)
	}

	// Re-tick mergeability — should be blocked now.
	if err := pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	got, _ := pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateBlocked {
		t.Errorf("after rule: state=%s, want blocked", got.MergeableState)
	}

	// Reviewer approves.
	if _, err := review.Submit(ctx, f.reviewDeps, review.SubmitParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		State: "approve", PRAuthorUserID: f.authorID,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID); err != nil {
		t.Fatalf("Mergeability after approve: %v", err)
	}
	got, _ = pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateClean {
		t.Errorf("after approve: state=%s, want clean", got.MergeableState)
	}

	// Merge proceeds.
	if err := pulls.Merge(ctx, f.pullsDeps, pulls.MergeParams{
		PRID: pr.IssueID, ActorUserID: f.authorID, GitDir: f.gitDir, Method: "merge",
	}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
}

func TestRequestChanges_BlocksMerge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	if _, err := review.Submit(ctx, f.reviewDeps, review.SubmitParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		State: "request_changes", PRAuthorUserID: f.authorID,
	}); err != nil {
		t.Fatalf("submit request_changes: %v", err)
	}
	if err := pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	got, _ := pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateBlocked {
		t.Errorf("after request_changes: state=%s, want blocked", got.MergeableState)
	}
	// Merge should refuse.
	if err := pulls.Merge(ctx, f.pullsDeps, pulls.MergeParams{
		PRID: pr.IssueID, ActorUserID: f.authorID, GitDir: f.gitDir, Method: "merge",
	}); err == nil {
		t.Errorf("Merge should have been blocked, got nil")
	}
}

func TestTwoApprovers_UnblockMerge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	rq := reposdb.New()
	id, _ := rq.UpsertBranchProtectionRule(ctx, f.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: f.repoID, Pattern: "trunk",
		PreventForcePush: true, PreventDeletion: true, RequirePrForPush: false,
		AllowedPusherUserIds: []int64{}, CreatedByUserID: pgtype.Int8{Valid: false},
	})
	_ = rq.UpdateBranchProtectionReviewSettings(ctx, f.pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
		ID: id, RequiredReviewCount: 2,
	})
	if err := pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	for _, uid := range []int64{f.reviewerID, f.otherID} {
		if _, err := review.Submit(ctx, f.reviewDeps, review.SubmitParams{
			PRIssueID: pr.IssueID, AuthorUserID: uid,
			State: "approve", PRAuthorUserID: f.authorID,
		}); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	if err := pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	got, _ := pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateClean {
		t.Errorf("two approvers: state=%s, want clean", got.MergeableState)
	}
}

func TestResolveAndReopen(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	c, err := review.AddComment(ctx, f.reviewDeps, review.CommentParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		FilePath: "x.txt", Side: "right", OriginalCommitSHA: pr.HeadOid,
		OriginalLine: 1, OriginalPosition: 1, CurrentPosition: 1,
		Body: "comment",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := review.Resolve(ctx, f.reviewDeps, f.reviewerID, c.ID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := review.Resolve(ctx, f.reviewDeps, f.reviewerID, c.ID); err == nil {
		t.Errorf("double-resolve should error")
	}
	if err := review.Reopen(ctx, f.reviewDeps, c.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	got, _ := pullsdb.New().GetPRReviewComment(ctx, f.pool, c.ID)
	if got.ResolvedAt.Valid {
		t.Errorf("after reopen: resolved_at still set")
	}
}

func TestDismiss_ClearsBlock(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add", "x.txt", "x\n")
	pr := f.openPR(t, "trunk", "feature")

	rv, err := review.Submit(ctx, f.reviewDeps, review.SubmitParams{
		PRIssueID: pr.IssueID, AuthorUserID: f.reviewerID,
		State: "request_changes", PRAuthorUserID: f.authorID,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	_ = pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID)
	got, _ := pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateBlocked {
		t.Fatalf("pre-dismiss: state=%s, want blocked", got.MergeableState)
	}
	if err := review.Dismiss(ctx, f.reviewDeps, f.authorID, rv.ID, "stale"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	_ = pulls.Mergeability(ctx, f.pullsDeps, f.gitDir, pr.IssueID)
	got, _ = pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, pr.IssueID)
	if got.MergeableState != pullsdb.PrMergeableStateClean {
		t.Errorf("post-dismiss: state=%s, want clean", got.MergeableState)
	}
}
