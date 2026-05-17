// SPDX-License-Identifier: AGPL-3.0-or-later

package fork_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/fork"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fx struct {
	pool   *pgxpool.Pool
	deps   fork.Deps
	rfs    *storage.RepoFS
	root   string
	source reposdb.Repo
	owner  usersdb.User
	other  usersdb.User
}

// setup spins a fresh DB + on-disk repos root + a real bare source
// repo so the fork orchestrator can exercise CloneBareShared,
// ResolveRefOID, IsAncestor end-to-end.
func setup(t *testing.T) fx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	owner, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	other, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser other: %v", err)
	}

	source, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo source: %v", err)
	}
	sourcePath, _ := rfs.RepoPath(owner.Username, source.Name)
	if err := rfs.InitBare(ctx, sourcePath); err != nil {
		t.Fatalf("InitBare source: %v", err)
	}

	return fx{
		pool: pool,
		deps: fork.Deps{
			Pool:   pool,
			RepoFS: rfs,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		rfs:    rfs,
		root:   root,
		source: source,
		owner:  owner,
		other:  other,
	}
}

func gitCmd(args ...string) *exec.Cmd {
	return exec.Command("git", args...) //nolint:gosec
}

// commitTo writes file=contents to a worktree of repoPath on branch
// and commits with msg. Returns the commit OID.
func commitTo(t *testing.T, repoPath, branch, msg, file, contents string) string {
	t.Helper()
	wt := t.TempDir()
	addArgs := []string{"-C", repoPath, "worktree", "add"}
	if _, err := gitCmd("-C", repoPath, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err != nil {
		addArgs = append(addArgs, "-b", branch, wt)
	} else {
		addArgs = append(addArgs, wt, branch)
	}
	if out, err := gitCmd(addArgs...).CombinedOutput(); err != nil {
		t.Fatalf("worktree add %s: %v (%s)", branch, err, out)
	}
	defer func() {
		_ = gitCmd("-C", repoPath, "worktree", "remove", "--force", wt).Run()
	}()
	if err := os.WriteFile(filepath.Join(wt, file), []byte(contents), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write %s: %v", file, err)
	}
	for _, a := range [][]string{
		{"-C", wt, "config", "user.name", "Alice"},
		{"-C", wt, "config", "user.email", "alice@example.com"},
		{"-C", wt, "add", "."},
		{"-C", wt, "commit", "-m", msg},
	} {
		if out, err := gitCmd(a...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", a, err, out)
		}
	}
	out, err := gitCmd("-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runForkClone simulates the worker job inline so tests can assert
// the post-clone state without spinning up the worker pool.
func (f fx) runForkClone(t *testing.T, sourceID, forkID int64) {
	t.Helper()
	ctx := context.Background()
	rq := reposdb.New()
	uq := usersdb.New()
	src, _ := rq.GetRepoByID(ctx, f.pool, sourceID)
	frk, _ := rq.GetRepoByID(ctx, f.pool, forkID)
	srcOwner, _ := uq.GetUserByID(ctx, f.pool, src.OwnerUserID.Int64)
	frkOwner, _ := uq.GetUserByID(ctx, f.pool, frk.OwnerUserID.Int64)
	srcPath, _ := f.rfs.RepoPath(srcOwner.Username, src.Name)
	frkPath, _ := f.rfs.RepoPath(frkOwner.Username, frk.Name)
	if err := f.rfs.CloneBareShared(ctx, srcPath, frkPath); err != nil {
		t.Fatalf("CloneBareShared: %v", err)
	}
	_ = rq.SetRepoInitStatus(ctx, f.pool, reposdb.SetRepoInitStatusParams{
		ID: forkID, InitStatus: reposdb.RepoInitStatusInitialized,
	})
}

func TestCreate_TargetOrg_Succeeds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// An org the other user can fork into.
	org, err := orgsdb.New().CreateOrg(ctx, f.pool, orgsdb.CreateOrgParams{
		Slug: "labgang", DisplayName: "Lab Gang",
		Description: "", BillingEmail: "billing@example.com",
		CreatedByUserID: pgtype.Int8{Int64: f.other.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	res, err := fork.Create(ctx, f.deps, fork.CreateParams{
		SourceRepoID:    f.source.ID,
		ActorUserID:     f.other.ID,
		TargetOwnerKind: "org",
		TargetOwnerID:   org.ID,
	})
	if err != nil {
		t.Fatalf("Create (org target): %v", err)
	}
	if res.Fork.OwnerUserID.Valid {
		t.Errorf("org-target fork should not set OwnerUserID")
	}
	if !res.Fork.OwnerOrgID.Valid || res.Fork.OwnerOrgID.Int64 != org.ID {
		t.Errorf("OwnerOrgID: got %v, want %d", res.Fork.OwnerOrgID, org.ID)
	}
	if !res.Fork.ForkOfRepoID.Valid || res.Fork.ForkOfRepoID.Int64 != f.source.ID {
		t.Errorf("fork_of_repo_id: got %v, want %d", res.Fork.ForkOfRepoID, f.source.ID)
	}
}

func TestCreate_TargetOrg_InvalidKindRejected(t *testing.T) {
	f := setup(t)
	_, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:    f.source.ID,
		ActorUserID:     f.other.ID,
		TargetOwnerKind: "team", // not user/org
		TargetOwnerID:   f.other.ID,
	})
	if err == nil {
		t.Fatal("Create unexpectedly succeeded for invalid TargetOwnerKind")
	}
	if !strings.Contains(err.Error(), "invalid TargetOwnerKind") {
		t.Errorf("error should name the invalid-kind condition: %v", err)
	}
}

func TestCreate_Basic(t *testing.T) {
	f := setup(t)
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Fork.InitStatus != reposdb.RepoInitStatusInitPending {
		t.Errorf("init_status: got %s, want init_pending", res.Fork.InitStatus)
	}
	if !res.Fork.ForkOfRepoID.Valid || res.Fork.ForkOfRepoID.Int64 != f.source.ID {
		t.Errorf("fork_of_repo_id: got %v, want %d", res.Fork.ForkOfRepoID, f.source.ID)
	}
	// Trigger should have bumped source's fork_count.
	src, _ := reposdb.New().GetRepoByID(context.Background(), f.pool, f.source.ID)
	if src.ForkCount != 1 {
		t.Errorf("source.fork_count: got %d, want 1", src.ForkCount)
	}
}

func TestCreate_VisibilityFloor_PrivateToPublic(t *testing.T) {
	f := setup(t)
	// Flip source to private.
	_, err := f.pool.Exec(context.Background(),
		"UPDATE repos SET visibility = 'private' WHERE id = $1", f.source.ID)
	if err != nil {
		t.Fatalf("flip private: %v", err)
	}
	_, err = fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:     f.source.ID,
		ActorUserID:      f.other.ID,
		TargetOwnerID:    f.other.ID,
		TargetVisibility: "public",
	})
	if !errors.Is(err, fork.ErrVisibilityFloor) {
		t.Errorf("expected ErrVisibilityFloor, got %v", err)
	}
}

func TestCreate_VisibilityFloor_PublicToPrivate_OK(t *testing.T) {
	f := setup(t)
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:     f.source.ID,
		ActorUserID:      f.other.ID,
		TargetOwnerID:    f.other.ID,
		TargetVisibility: "private",
	})
	if err != nil {
		t.Fatalf("Create public→private: %v", err)
	}
	if res.Fork.Visibility != reposdb.RepoVisibilityPrivate {
		t.Errorf("fork visibility: got %s, want private", res.Fork.Visibility)
	}
}

func TestCreate_SelfForkSameName_Rejected(t *testing.T) {
	f := setup(t)
	_, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.owner.ID,
		TargetOwnerID: f.owner.ID,
	})
	if !errors.Is(err, fork.ErrSelfForkSameName) {
		t.Errorf("expected ErrSelfForkSameName, got %v", err)
	}
}

func TestCreate_SelfForkRenamed_OK(t *testing.T) {
	f := setup(t)
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.owner.ID,
		TargetOwnerID: f.owner.ID,
		TargetName:    "demo-fork",
	})
	if err != nil {
		t.Fatalf("self-fork renamed: %v", err)
	}
	if res.Fork.Name != "demo-fork" {
		t.Errorf("fork name: got %s, want demo-fork", res.Fork.Name)
	}
}

func TestCreate_DescriptionInheritsSource(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		"UPDATE repos SET description = $2 WHERE id = $1", f.source.ID, "from the source")
	if err != nil {
		t.Fatalf("set source description: %v", err)
	}
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Fork.Description != "from the source" {
		t.Errorf("fork description: got %q, want %q", res.Fork.Description, "from the source")
	}
}

func TestCreate_DescriptionOverride_Honored(t *testing.T) {
	f := setup(t)
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:      f.source.ID,
		ActorUserID:       f.other.ID,
		TargetOwnerID:     f.other.ID,
		TargetDescription: "  my own blurb  ",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Fork.Description != "my own blurb" {
		t.Errorf("fork description: got %q, want %q (trim expected)", res.Fork.Description, "my own blurb")
	}
}

func TestCreate_DescriptionOverride_BlankFallsBackToSource(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		"UPDATE repos SET description = $2 WHERE id = $1", f.source.ID, "source blurb")
	if err != nil {
		t.Fatalf("set source description: %v", err)
	}
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:      f.source.ID,
		ActorUserID:       f.other.ID,
		TargetOwnerID:     f.other.ID,
		TargetDescription: "   ",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Fork.Description != "source blurb" {
		t.Errorf("fork description: got %q, want source fallback", res.Fork.Description)
	}
}

func TestCreate_DescriptionOverride_TooLong(t *testing.T) {
	f := setup(t)
	long := strings.Repeat("a", 351)
	_, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:      f.source.ID,
		ActorUserID:       f.other.ID,
		TargetOwnerID:     f.other.ID,
		TargetDescription: long,
	})
	if !errors.Is(err, repos.ErrDescriptionTooLong) {
		t.Errorf("expected ErrDescriptionTooLong, got %v", err)
	}
}

func TestCreate_TargetNameTaken(t *testing.T) {
	f := setup(t)
	// other already has a "demo" repo.
	_, err := reposdb.New().CreateRepo(context.Background(), f.pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: f.other.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo collision: %v", err)
	}
	_, err = fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if !errors.Is(err, fork.ErrTargetNameTaken) {
		t.Errorf("expected ErrTargetNameTaken, got %v", err)
	}
}

func TestCreate_SourceArchived(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		"UPDATE repos SET is_archived = true WHERE id = $1", f.source.ID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, err = fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if !errors.Is(err, fork.ErrSourceArchived) {
		t.Errorf("expected ErrSourceArchived, got %v", err)
	}
}

// TestCreate_SourcePaused: PRO-EXT01-15 — a paused source can't be
// forked. Distinct from archived (which is a permanent retirement);
// pause is the temporary "frozen pending decision" state.
func TestCreate_SourcePaused(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		"UPDATE repos SET is_paused = true, paused_at = now() WHERE id = $1", f.source.ID)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, err = fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if !errors.Is(err, fork.ErrSourcePaused) {
		t.Errorf("expected ErrSourcePaused, got %v", err)
	}
}

// TestForkCount_DecrementOnDelete confirms the fork_count trigger
// honors the AFTER DELETE path (the spec promises this and S16's
// hard-delete cascade depends on it).
func TestForkCount_DecrementOnDelete(t *testing.T) {
	f := setup(t)
	res, err := fork.Create(context.Background(), f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		"DELETE FROM repos WHERE id = $1", res.Fork.ID); err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	src, _ := reposdb.New().GetRepoByID(context.Background(), f.pool, f.source.ID)
	if src.ForkCount != 0 {
		t.Errorf("source.fork_count after fork delete: got %d, want 0", src.ForkCount)
	}
}

// --- Sync tests --------------------------------------------------------

func TestSync_FastForward(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sourcePath, _ := f.rfs.RepoPath(f.owner.Username, f.source.Name)
	commitTo(t, sourcePath, "trunk", "init", "README.md", "v1\n")

	res, err := fork.Create(ctx, f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.runForkClone(t, f.source.ID, res.Fork.ID)

	// Source advances; fork is now strictly behind.
	commitTo(t, sourcePath, "trunk", "v2", "README.md", "v2\n")

	syncRes, err := fork.Sync(ctx, f.deps, f.other.ID, res.Fork.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	forkPath, _ := f.rfs.RepoPath(f.other.Username, res.Fork.Name)
	got, _ := repogit.ResolveRefOID(ctx, forkPath, "trunk")
	if got != syncRes.NewOID {
		t.Errorf("fork tip after sync: got %s, want %s", got, syncRes.NewOID)
	}
}

func TestSync_UpToDate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sourcePath, _ := f.rfs.RepoPath(f.owner.Username, f.source.Name)
	commitTo(t, sourcePath, "trunk", "init", "README.md", "v1\n")

	res, err := fork.Create(ctx, f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.runForkClone(t, f.source.ID, res.Fork.ID)

	// Both sides at same OID: sync returns ErrSyncUpToDate.
	_, err = fork.Sync(ctx, f.deps, f.other.ID, res.Fork.ID)
	if !errors.Is(err, fork.ErrSyncUpToDate) {
		t.Errorf("expected ErrSyncUpToDate, got %v", err)
	}
}

func TestSync_Diverged(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sourcePath, _ := f.rfs.RepoPath(f.owner.Username, f.source.Name)
	commitTo(t, sourcePath, "trunk", "init", "README.md", "v1\n")

	res, err := fork.Create(ctx, f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.runForkClone(t, f.source.ID, res.Fork.ID)
	forkPath, _ := f.rfs.RepoPath(f.other.Username, res.Fork.Name)

	// Both sides advance independently — diverged.
	commitTo(t, sourcePath, "trunk", "src v2", "README.md", "src-v2\n")
	commitTo(t, forkPath, "trunk", "fork v2", "README.md", "fork-v2\n")

	_, err = fork.Sync(ctx, f.deps, f.other.ID, res.Fork.ID)
	if !errors.Is(err, fork.ErrSyncDiverged) {
		t.Errorf("expected ErrSyncDiverged, got %v", err)
	}
}

// --- Ahead/behind tests ------------------------------------------------

func TestAheadBehind_StrictlyBehind(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sourcePath, _ := f.rfs.RepoPath(f.owner.Username, f.source.Name)
	commitTo(t, sourcePath, "trunk", "init", "README.md", "v1\n")

	res, err := fork.Create(ctx, f.deps, fork.CreateParams{
		SourceRepoID:  f.source.ID,
		ActorUserID:   f.other.ID,
		TargetOwnerID: f.other.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.runForkClone(t, f.source.ID, res.Fork.ID)
	commitTo(t, sourcePath, "trunk", "v2", "README.md", "v2\n")
	commitTo(t, sourcePath, "trunk", "v3", "README.md", "v3\n")

	stats, err := fork.AheadBehind(ctx, f.deps, res.Fork.ID)
	if err != nil {
		t.Fatalf("AheadBehind: %v", err)
	}
	if !stats.Comparable || stats.Ahead != 0 || stats.Behind != 2 {
		t.Errorf("stats: got %+v, want {Ahead:0 Behind:2 Comparable:true}", stats)
	}
}
