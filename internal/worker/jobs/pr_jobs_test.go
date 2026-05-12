// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const prJobsFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func prJobsGitCmd(args ...string) *exec.Cmd {
	//nolint:gosec
	return exec.Command("git", args...)
}

func prJobsCommitOnBranch(t *testing.T, gitDir, branch, msg, file, contents string) string {
	t.Helper()
	wt := t.TempDir()
	addArgs := []string{"-C", gitDir, "worktree", "add"}
	if _, err := prJobsGitCmd("-C", gitDir, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err != nil {
		addArgs = append(addArgs, "-b", branch, wt)
	} else {
		addArgs = append(addArgs, wt, branch)
	}
	if out, err := prJobsGitCmd(addArgs...).CombinedOutput(); err != nil {
		t.Fatalf("worktree add %s: %v (%s)", branch, err, out)
	}
	defer func() {
		_ = prJobsGitCmd("-C", gitDir, "worktree", "remove", "--force", wt).Run()
	}()

	if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, file)), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", file, err)
	}
	if err := os.WriteFile(filepath.Join(wt, file), []byte(contents), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write %s: %v", file, err)
	}
	for _, args := range [][]string{
		{"-C", wt, "config", "user.name", "Alice"},
		{"-C", wt, "config", "user.email", "alice@example.com"},
		{"-C", wt, "add", "."},
		{"-C", wt, "commit", "-m", msg},
	} {
		if out, err := prJobsGitCmd(args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	out, err := prJobsGitCmd("-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestPRMergeability_OrgOwnedRepoResolvesOwnerSlug(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: prJobsFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	email, err := uq.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "alice@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := uq.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: email.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}

	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	res, err := repos.Create(ctx, repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}, repos.Params{
		OwnerOrgID: org.ID, OwnerSlug: string(org.Slug), ActorUserID: user.ID,
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	gitDir, err := rfs.RepoPath(string(org.Slug), res.Repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	prJobsCommitOnBranch(t, gitDir, "feature", "add feature", "feature.txt", "feature\n")

	pr, err := pulls.Create(ctx, pulls.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pulls.CreateParams{
		RepoID:       res.Repo.ID,
		AuthorUserID: user.ID,
		Title:        "Add feature",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       gitDir,
	})
	if err != nil {
		t.Fatalf("pulls.Create: %v", err)
	}

	handler := PRMergeability(PRJobsDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	payload, _ := json.Marshal(PRMergeabilityPayload{PRID: pr.PullRequest.IssueID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("PRMergeability: %v", err)
	}

	got, err := pullsdb.New().GetPullRequestByIssueID(ctx, pool, pr.PullRequest.IssueID)
	if err != nil {
		t.Fatalf("GetPullRequestByIssueID: %v", err)
	}
	if got.MergeableState != pullsdb.PrMergeableStateClean {
		t.Fatalf("mergeable_state = %s, want clean", got.MergeableState)
	}
	if !got.Mergeable.Valid || !got.Mergeable.Bool {
		t.Fatalf("mergeable = %+v, want true", got.Mergeable)
	}
}
