// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
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

// gitCmd suppresses the gosec G204 noise — every invocation runs
// against a t.TempDir path the test set up.
func gitCmd(args ...string) *exec.Cmd {
	//nolint:gosec
	return exec.Command("git", args...)
}

// fixture spins a real bare git repo on disk + a DB row pair (user
// + repo + ensured issue counter) so the orchestrator's path-on-disk
// assumptions hold.
type fixture struct {
	pool   *pgxpool.Pool
	deps   pulls.Deps
	userID int64
	repoID int64
	gitDir string
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := uq.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "alice@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := uq.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}

	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
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

	root := t.TempDir()
	gitDir := filepath.Join(root, "demo.git")
	if out, err := gitCmd("init", "--bare", "-b", "trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}

	w := io.Discard
	if testing.Verbose() {
		w = os.Stderr
	}
	deps := pulls.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(w, nil)),
	}
	return fixture{pool: pool, deps: deps, userID: user.ID, repoID: repo.ID, gitDir: gitDir}
}

func createPullsTestUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	ctx := context.Background()
	uq := usersdb.New()
	u, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: username, DisplayName: strings.ToTitle(username), PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	em, err := uq.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID: u.ID, Email: username + "@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail %s: %v", username, err)
	}
	if err := uq.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID: u.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail %s: %v", username, err)
	}
	return u.ID
}

func grantRepoCollaborator(t *testing.T, pool *pgxpool.Pool, repoID, userID, addedByUserID int64, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO repo_collaborators (repo_id, user_id, role, added_by_user_id)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (repo_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		repoID, userID, role, addedByUserID); err != nil {
		t.Fatalf("grant collaborator: %v", err)
	}
}

// commitOnBranch creates a commit on branch from a temp worktree.
// Returns the new HEAD oid.
func commitOnBranch(t *testing.T, gitDir, branch, msg, file, contents string) string {
	t.Helper()
	wt := t.TempDir()
	// Add a worktree that creates the branch if missing.
	addArgs := []string{"-C", gitDir, "worktree", "add"}
	// If branch doesn't exist yet, create it; otherwise check it out.
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

func TestCreate_OpensPRWithIssueRow(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add foo", "foo.txt", "foo\n")

	res, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID:       f.repoID,
		AuthorUserID: f.userID,
		Title:        "Add foo",
		Body:         "fixes nothing yet",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Issue.Kind != issuesdb.IssueKindPr {
		t.Errorf("issue kind: got %s, want pr", res.Issue.Kind)
	}
	if res.PullRequest.BaseRef != "trunk" || res.PullRequest.HeadRef != "feature" {
		t.Errorf("ref mismatch: %+v", res.PullRequest)
	}
	if res.PullRequest.BaseOid == "" || res.PullRequest.HeadOid == "" {
		t.Errorf("OIDs not snapshotted: %+v", res.PullRequest)
	}
	commits, _ := pullsdb.New().ListPullRequestCommits(context.Background(), f.pool, res.PullRequest.IssueID)
	if len(commits) == 0 {
		t.Errorf("expected commits populated by initial sync")
	}
}

func TestCreate_RequestsCodeOwners(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	reviewerID := createPullsTestUser(t, f.pool, "bob")
	grantRepoCollaborator(t, f.pool, f.repoID, reviewerID, f.userID, "write")
	commitOnBranch(t, f.gitDir, "trunk", "codeowners", "CODEOWNERS", "*.go @bob\n")
	commitOnBranch(t, f.gitDir, "feature", "add go", "main.go", "package main\n")

	res, err := pulls.Create(ctx, f.deps, pulls.CreateParams{
		RepoID:       f.repoID,
		AuthorUserID: f.userID,
		Title:        "Add go",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	requests, err := pullsdb.New().ListPRReviewRequests(ctx, f.pool, res.PullRequest.IssueID)
	if err != nil {
		t.Fatalf("ListPRReviewRequests: %v", err)
	}
	if len(requests) != 1 || !requests[0].RequestedUserID.Valid || requests[0].RequestedUserID.Int64 != reviewerID {
		t.Fatalf("expected bob code owner request, got %+v", requests)
	}
}

func TestCreate_RequestsTeamCodeOwners(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	reviewerID := createPullsTestUser(t, f.pool, "bob")
	org, err := orgsdb.New().CreateOrg(ctx, f.pool, orgsdb.CreateOrgParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		BillingEmail:    "billing@example.com",
		CreatedByUserID: pgtype.Int8{Int64: f.userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, f.pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo org: %v", err)
	}
	if err := issuesdb.New().EnsureRepoIssueCounter(ctx, f.pool, repo.ID); err != nil {
		t.Fatalf("EnsureRepoIssueCounter org: %v", err)
	}
	team, err := orgsdb.New().CreateTeam(ctx, f.pool, orgsdb.CreateTeamParams{
		OrgID:           org.ID,
		Slug:            "reviewers",
		DisplayName:     "Reviewers",
		Privacy:         orgsdb.TeamPrivacyVisible,
		CreatedByUserID: pgtype.Int8{Int64: f.userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := orgsdb.New().AddTeamMember(ctx, f.pool, orgsdb.AddTeamMemberParams{
		TeamID: team.ID,
		UserID: reviewerID,
		Role:   orgsdb.TeamRoleMember,
	}); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}
	if err := orgsdb.New().GrantTeamRepoAccess(ctx, f.pool, orgsdb.GrantTeamRepoAccessParams{
		TeamID: team.ID,
		RepoID: repo.ID,
		Role:   orgsdb.TeamRepoRoleWrite,
	}); err != nil {
		t.Fatalf("GrantTeamRepoAccess: %v", err)
	}
	commitOnBranch(t, f.gitDir, "trunk", "codeowners", "CODEOWNERS", "*.go @acme/reviewers\n")
	commitOnBranch(t, f.gitDir, "feature", "add go", "main.go", "package main\n")

	res, err := pulls.Create(ctx, f.deps, pulls.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: f.userID,
		Title:        "Add go",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	requests, err := pullsdb.New().ListPRReviewRequests(ctx, f.pool, res.PullRequest.IssueID)
	if err != nil {
		t.Fatalf("ListPRReviewRequests: %v", err)
	}
	if len(requests) != 1 || !requests[0].RequestedTeamID.Valid || requests[0].RequestedTeamID.Int64 != team.ID {
		t.Fatalf("expected team code owner request, got %+v", requests)
	}
}

func TestMergeability_CodeOwnerReviewRequired(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	reviewerID := createPullsTestUser(t, f.pool, "bob")
	grantRepoCollaborator(t, f.pool, f.repoID, reviewerID, f.userID, "write")
	ruleID, err := reposdb.New().UpsertBranchProtectionRule(ctx, f.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               f.repoID,
		Pattern:              "trunk",
		AllowedPusherUserIds: []int64{},
	})
	if err != nil {
		t.Fatalf("UpsertBranchProtectionRule: %v", err)
	}
	if err := reposdb.New().UpdateBranchProtectionReviewSettings(ctx, f.pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
		ID:                     ruleID,
		RequireCodeOwnerReview: true,
	}); err != nil {
		t.Fatalf("UpdateBranchProtectionReviewSettings: %v", err)
	}
	commitOnBranch(t, f.gitDir, "trunk", "codeowners", "CODEOWNERS", "*.go @bob\n")
	commitOnBranch(t, f.gitDir, "feature", "add go", "main.go", "package main\n")

	res, err := pulls.Create(ctx, f.deps, pulls.CreateParams{
		RepoID:       f.repoID,
		AuthorUserID: f.userID,
		Title:        "Add go",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := pulls.Mergeability(ctx, f.deps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability before approval: %v", err)
	}
	pr, err := pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, res.PullRequest.IssueID)
	if err != nil {
		t.Fatalf("GetPullRequestByIssueID: %v", err)
	}
	if pr.MergeableState != pullsdb.PrMergeableStateBlocked {
		t.Fatalf("mergeable_state=%s want blocked", pr.MergeableState)
	}

	if _, err := review.Submit(ctx, review.Deps{Pool: f.pool, Logger: f.deps.Logger}, review.SubmitParams{
		PRIssueID:      res.PullRequest.IssueID,
		AuthorUserID:   reviewerID,
		State:          "approve",
		PRAuthorUserID: f.userID,
	}); err != nil {
		t.Fatalf("Submit approve: %v", err)
	}
	if err := pulls.Mergeability(ctx, f.deps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability after approval: %v", err)
	}
	pr, err = pullsdb.New().GetPullRequestByIssueID(ctx, f.pool, res.PullRequest.IssueID)
	if err != nil {
		t.Fatalf("GetPullRequestByIssueID after approval: %v", err)
	}
	if pr.MergeableState != pullsdb.PrMergeableStateClean {
		t.Fatalf("mergeable_state=%s want clean", pr.MergeableState)
	}
}

func TestCreate_RejectsSameBranch(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	_, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "x", BaseRef: "trunk", HeadRef: "trunk", GitDir: f.gitDir,
	})
	if err == nil {
		t.Fatalf("expected ErrSameBranch, got nil")
	}
}

func TestMergeability_Clean(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add foo", "foo.txt", "foo\n")
	res, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "x", BaseRef: "trunk", HeadRef: "feature", GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := pulls.Mergeability(context.Background(), f.deps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	pr, _ := pullsdb.New().GetPullRequestByIssueID(context.Background(), f.pool, res.PullRequest.IssueID)
	if pr.MergeableState != pullsdb.PrMergeableStateClean {
		t.Errorf("got %s, want clean", pr.MergeableState)
	}
}

func TestMergeability_Dirty(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "shared.txt", "base content\n")
	// Modify shared.txt on trunk.
	commitOnBranch(t, f.gitDir, "trunk", "trunk edit", "shared.txt", "trunk content\n")
	// Branch from earlier trunk and also edit shared.txt → conflict.
	// Create the feature branch from the first trunk commit.
	out, err := gitCmd("-C", f.gitDir, "rev-list", "--reverse", "trunk").Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	firstSHA := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if out, err := gitCmd("-C", f.gitDir, "branch", "feature", firstSHA).CombinedOutput(); err != nil {
		t.Fatalf("create feature branch: %v (%s)", err, out)
	}
	commitOnBranch(t, f.gitDir, "feature", "feature edit", "shared.txt", "feature content\n")

	res, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "x", BaseRef: "trunk", HeadRef: "feature", GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := pulls.Mergeability(context.Background(), f.deps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	pr, _ := pullsdb.New().GetPullRequestByIssueID(context.Background(), f.pool, res.PullRequest.IssueID)
	if pr.MergeableState != pullsdb.PrMergeableStateDirty {
		t.Errorf("got %s, want dirty", pr.MergeableState)
	}
}

func TestMerge_MergeCommit(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add foo", "foo.txt", "foo\n")
	res, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "Add foo", BaseRef: "trunk", HeadRef: "feature", GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := pulls.Mergeability(context.Background(), f.deps, f.gitDir, res.PullRequest.IssueID); err != nil {
		t.Fatalf("Mergeability: %v", err)
	}
	if err := pulls.Merge(context.Background(), f.deps, pulls.MergeParams{
		PRID: res.PullRequest.IssueID, ActorUserID: f.userID,
		GitDir: f.gitDir, Method: "merge",
	}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	pr, _ := pullsdb.New().GetPullRequestByIssueID(context.Background(), f.pool, res.PullRequest.IssueID)
	if !pr.MergedAt.Valid {
		t.Errorf("merged_at not set")
	}
	if !pr.MergeCommitSha.Valid {
		t.Errorf("merge_commit_sha not set")
	}
	// Issue side closed?
	iq := issuesdb.New()
	issue, _ := iq.GetIssueByID(context.Background(), f.pool, res.PullRequest.IssueID)
	if issue.State != issuesdb.IssueStateClosed {
		t.Errorf("issue state: got %s, want closed", issue.State)
	}
}

func TestMerge_RejectsConcurrentDouble(t *testing.T) {
	f := setup(t)
	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add foo", "foo.txt", "foo\n")
	res, err := pulls.Create(context.Background(), f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "x", BaseRef: "trunk", HeadRef: "feature", GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = pulls.Mergeability(context.Background(), f.deps, f.gitDir, res.PullRequest.IssueID)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pulls.Merge(context.Background(), f.deps, pulls.MergeParams{
				PRID: res.PullRequest.IssueID, ActorUserID: f.userID,
				GitDir: f.gitDir, Method: "merge",
			})
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, e := range errs {
		if e == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly one successful merge, got %d (errors: %v, %v)", successes, errs[0], errs[1])
	}
}

func TestMerge_LinkedIssueAutoClose(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Create an issue first so the PR body can reference it.
	iq := issuesdb.New()
	num, err := iq.AllocateIssueNumber(ctx, f.pool, f.repoID)
	if err != nil {
		t.Fatalf("AllocateIssueNumber: %v", err)
	}
	issue, err := iq.CreateIssue(ctx, f.pool, issuesdb.CreateIssueParams{
		RepoID:       f.repoID,
		Number:       num,
		Kind:         issuesdb.IssueKindIssue,
		Title:        "bug",
		Body:         "fix me",
		AuthorUserID: pgtype.Int8{Int64: f.userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	commitOnBranch(t, f.gitDir, "trunk", "init", "README.md", "hi\n")
	commitOnBranch(t, f.gitDir, "feature", "add foo", "foo.txt", "foo\n")

	res, err := pulls.Create(ctx, f.deps, pulls.CreateParams{
		RepoID: f.repoID, AuthorUserID: f.userID,
		Title: "fix the bug", Body: "Fixes #1",
		BaseRef: "trunk", HeadRef: "feature", GitDir: f.gitDir,
	})
	if err != nil {
		t.Fatalf("Create PR: %v", err)
	}
	_ = pulls.Mergeability(ctx, f.deps, f.gitDir, res.PullRequest.IssueID)

	if err := pulls.Merge(ctx, f.deps, pulls.MergeParams{
		PRID: res.PullRequest.IssueID, ActorUserID: f.userID,
		GitDir: f.gitDir, Method: "squash",
	}); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// The pre-existing issue (#1) should now be closed.
	got, err := iq.GetIssueByID(ctx, f.pool, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueByID: %v", err)
	}
	if got.State != issuesdb.IssueStateClosed {
		t.Errorf("linked issue state: got %s, want closed", got.State)
	}
}
