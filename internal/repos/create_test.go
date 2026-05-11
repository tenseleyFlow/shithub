// SPDX-License-Identifier: AGPL-3.0-or-later

package repos_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// gitCmd wraps exec.Command with the single G204 suppression for this
// file — every git invocation runs against a t.TempDir path.
func gitCmd(args ...string) *exec.Cmd {
	//nolint:gosec // G204 false positive: callers feed t.TempDir paths and fixed flags.
	return exec.Command("git", args...)
}

// fixtureHash is a static PHC test fixture (zero salt, zero key) — not a credential.
const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// setupCreateEnv constructs Deps + a verified-email user against a
// fresh test DB.
func setupCreateEnv(t *testing.T) (*pgxpool.Pool, repos.Deps, int64, string, string) {
	t.Helper()
	pool := dbtest.NewTestDB(t)

	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	deps := repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice Anderson", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "alice@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
	return pool, deps, user.ID, user.Username, root
}

func TestCreate_EmptyRepo(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, root := setupCreateEnv(t)
	res, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID:   uid,
		OwnerUsername: uname,
		Name:          "empty-repo",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.InitialCommitOID != "" {
		t.Errorf("expected no initial commit; got %q", res.InitialCommitOID)
	}
	if !strings.HasPrefix(res.DiskPath, root) {
		t.Errorf("DiskPath %q not under root %q", res.DiskPath, root)
	}

	// HEAD must be a symbolic ref to refs/heads/trunk (unborn branch).
	out, err := gitCmd("-C", res.DiskPath, "symbolic-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("symbolic-ref: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/heads/trunk" {
		t.Fatalf("HEAD = %q, want refs/heads/trunk", got)
	}

	// Zero commits.
	out, _ = gitCmd("-C", res.DiskPath, "rev-list", "--all", "--count").CombinedOutput()
	if got := strings.TrimSpace(string(out)); got != "0" {
		t.Fatalf("rev-list count = %q, want 0", got)
	}
}

func TestCreate_WithReadmeLicenseGitignore(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	res, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID:       uid,
		OwnerUsername:     uname,
		Name:              "init-repo",
		Description:       "hello world",
		Visibility:        "public",
		InitReadme:        true,
		LicenseKey:        "MIT",
		GitignoreKey:      "Go",
		InitialCommitWhen: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.InitialCommitOID == "" {
		t.Fatal("expected an initial commit")
	}

	// Single commit, three files, expected names.
	out, _ := gitCmd("-C", res.DiskPath, "rev-list", "--count", "trunk").CombinedOutput()
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list count = %q, want 1", got)
	}
	out, _ = gitCmd("-C", res.DiskPath, "ls-tree", "--name-only", "trunk").CombinedOutput()
	got := strings.TrimSpace(string(out))
	for _, want := range []string{"README.md", "LICENSE", ".gitignore"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in tree: %q", want, got)
		}
	}
	// Author identity is alice's verified primary email.
	out, _ = gitCmd("-C", res.DiskPath, "log", "-1", "--format=%an <%ae>", "trunk").CombinedOutput()
	if want := "Alice Anderson <alice@example.com>"; strings.TrimSpace(string(out)) != want {
		t.Errorf("author = %q, want %q", strings.TrimSpace(string(out)), want)
	}
	// LICENSE has the year substituted.
	out, _ = gitCmd("-C", res.DiskPath, "show", "trunk:LICENSE").CombinedOutput()
	if !strings.Contains(string(out), "2026") {
		t.Errorf("LICENSE missing year 2026; got first 200 chars: %s", string(out)[:200])
	}
}

func TestCreate_RejectsDuplicate(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	if _, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname, Name: "dup", Visibility: "public",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname, Name: "dup", Visibility: "public",
	})
	if !errors.Is(err, repos.ErrTaken) {
		t.Fatalf("second create: err = %v, want ErrTaken", err)
	}
}

func TestCreate_ReusesSoftDeletedRepoName(t *testing.T) {
	t.Parallel()
	pool, deps, uid, uname, _ := setupCreateEnv(t)
	ctx := context.Background()
	first, err := repos.Create(ctx, deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname, Name: "reuse", Visibility: "public",
		InitReadme: true,
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	deletedPath, err := deps.RepoFS.DeletedRepoPath(uname, "reuse", first.Repo.ID)
	if err != nil {
		t.Fatalf("DeletedRepoPath: %v", err)
	}
	ldeps := lifecycle.Deps{Pool: pool, RepoFS: deps.RepoFS, Audit: audit.NewRecorder(), Logger: deps.Logger}
	if err := lifecycle.SoftDelete(ctx, ldeps, uid, first.Repo.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := os.Stat(first.DiskPath); !os.IsNotExist(err) {
		t.Fatalf("canonical path after soft-delete: err = %v, want not exist", err)
	}
	if _, err := os.Stat(deletedPath); err != nil {
		t.Fatalf("deleted tombstone missing: %v", err)
	}

	second, err := repos.Create(ctx, deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname, Name: "reuse", Visibility: "public",
	})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if second.Repo.ID == first.Repo.ID {
		t.Fatalf("recreate reused old repo id %d", second.Repo.ID)
	}
	if _, err := os.Stat(second.DiskPath); err != nil {
		t.Fatalf("replacement canonical path missing: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE repos SET deleted_at = now() - interval '8 days' WHERE id = $1`, first.Repo.ID); err != nil {
		t.Fatalf("backdate deleted repo: %v", err)
	}
	if err := lifecycle.HardDelete(ctx, ldeps, 0, first.Repo.ID); err != nil {
		t.Fatalf("HardDelete old repo: %v", err)
	}
	if _, err := os.Stat(second.DiskPath); err != nil {
		t.Fatalf("replacement path should survive hard-delete of old repo: %v", err)
	}
}

func TestCreate_DisplacesLegacySoftDeletedOrgRepoPath(t *testing.T) {
	t.Parallel()
	pool, deps, uid, _, _ := setupCreateEnv(t)
	ctx := context.Background()
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug: "gardesk", DisplayName: "gardesk", CreatedByUserID: uid,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	first, err := repos.Create(ctx, deps, repos.Params{
		OwnerOrgID: org.ID, OwnerSlug: string(org.Slug), ActorUserID: uid,
		Name: "garterm", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE repos SET deleted_at = now(), updated_at = now() WHERE id = $1`, first.Repo.ID); err != nil {
		t.Fatalf("legacy soft delete: %v", err)
	}

	second, err := repos.Create(ctx, deps, repos.Params{
		OwnerOrgID: org.ID, OwnerSlug: string(org.Slug), ActorUserID: uid,
		Name: "garterm", Visibility: "public",
	})
	if err != nil {
		t.Fatalf("recreate after legacy soft delete: %v", err)
	}
	if second.Repo.ID == first.Repo.ID {
		t.Fatalf("recreate reused old repo id %d", second.Repo.ID)
	}
	deletedPath, err := deps.RepoFS.DeletedRepoPath(string(org.Slug), "garterm", first.Repo.ID)
	if err != nil {
		t.Fatalf("DeletedRepoPath: %v", err)
	}
	if _, err := os.Stat(deletedPath); err != nil {
		t.Fatalf("legacy repo was not moved to tombstone: %v", err)
	}
	if _, err := os.Stat(second.DiskPath); err != nil {
		t.Fatalf("replacement canonical path missing: %v", err)
	}
}

func TestCreate_RejectsReservedName(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	_, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname, Name: "head", Visibility: "public",
	})
	if !errors.Is(err, repos.ErrReservedName) {
		t.Fatalf("err = %v, want ErrReservedName", err)
	}
}

func TestCreate_RefusesWithoutVerifiedEmail(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, _ := storage.NewRepoFS(root)
	deps := repos.Deps{
		Pool: pool, RepoFS: rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}
	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// User exists but has NO verified primary email.
	_, err = repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: user.Username,
		Name: "needs-email", Visibility: "public",
		InitReadme: true,
	})
	if !errors.Is(err, repos.ErrNoVerifiedEmail) {
		t.Fatalf("err = %v, want ErrNoVerifiedEmail", err)
	}
}

func TestCreate_PrivateVisibilityPersists(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	res, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID: uid, OwnerUsername: uname,
		Name: "secret", Visibility: "private",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if string(res.Repo.Visibility) != "private" {
		t.Errorf("Visibility = %q, want private", res.Repo.Visibility)
	}
	// Sharded path layout sanity check (shard is first 2 chars of OWNER).
	if want := filepath.Join("al", "alice", "secret.git"); !strings.HasSuffix(res.DiskPath, want) {
		t.Errorf("DiskPath %q does not end with %q", res.DiskPath, want)
	}
}

func TestCreate_OrgOwned(t *testing.T) {
	t.Parallel()
	pool, deps, uid, _, _ := setupCreateEnv(t)
	// Create an org owned by alice (the setupCreateEnv user).
	odeps := orgs.Deps{Pool: pool}
	org, err := orgs.Create(context.Background(), odeps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme", CreatedByUserID: uid,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	res, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerOrgID:  org.ID,
		OwnerSlug:   string(org.Slug),
		ActorUserID: uid,
		Name:        "demo",
		Visibility:  "public",
	})
	if err != nil {
		t.Fatalf("Create org-owned: %v", err)
	}
	if !res.Repo.OwnerOrgID.Valid || res.Repo.OwnerOrgID.Int64 != org.ID {
		t.Fatalf("expected org-owned row; got owner_org_id=%v", res.Repo.OwnerOrgID)
	}
	if res.Repo.OwnerUserID.Valid {
		t.Fatalf("owner_user_id should be NULL for org-owned, got %v", res.Repo.OwnerUserID)
	}
	// Disk path uses the org slug, not a user-namespace prefix.
	if !strings.Contains(res.DiskPath, "acme") {
		t.Fatalf("DiskPath %q should contain org slug 'acme'", res.DiskPath)
	}
}

// TestCreate_ThrottlesNonAdmin saturates the per-actor cap directly via
// the limiter and confirms a non-admin Create call returns the typed
// throttle error. Doing it this way avoids spinning up
// CreateRateLimitMax+1 real repositories.
func TestCreate_ThrottlesNonAdmin(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	saturateCreateLimiter(t, deps, uid)

	_, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID:   uid,
		OwnerUsername: uname,
		Name:          "should-throttle",
		Visibility:    "public",
	})
	if !throttle.IsThrottled(err) {
		t.Fatalf("Create: err = %v, want throttle error", err)
	}
}

// TestCreate_SiteAdminBypassesThrottle is the bookend: same saturated
// counter, but with ActorIsSiteAdmin=true the create succeeds.
func TestCreate_SiteAdminBypassesThrottle(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	saturateCreateLimiter(t, deps, uid)

	res, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID:      uid,
		OwnerUsername:    uname,
		ActorIsSiteAdmin: true,
		Name:             "admin-bypass",
		Visibility:       "public",
	})
	if err != nil {
		t.Fatalf("Create with admin bypass: %v", err)
	}
	if res.Repo.Name != "admin-bypass" {
		t.Fatalf("created repo name = %q, want admin-bypass", res.Repo.Name)
	}
}

// saturateCreateLimiter pushes the per-actor counter for "repo_create"
// up to the cap so the next non-admin Hit returns ErrThrottled.
func saturateCreateLimiter(t *testing.T, deps repos.Deps, uid int64) {
	t.Helper()
	lim := throttle.Limit{
		Scope:      "repo_create",
		Identifier: fmt.Sprintf("user:%d", uid),
		Max:        repos.CreateRateLimitMax,
		Window:     repos.CreateRateLimitWindow,
	}
	for i := 0; i < repos.CreateRateLimitMax; i++ {
		if err := deps.Limiter.Hit(context.Background(), deps.Pool, lim); err != nil {
			t.Fatalf("priming limiter hit %d: %v", i, err)
		}
	}
}

func TestCreate_RejectsBothOwnerKindsSet(t *testing.T) {
	t.Parallel()
	_, deps, uid, uname, _ := setupCreateEnv(t)
	_, err := repos.Create(context.Background(), deps, repos.Params{
		OwnerUserID:   uid,
		OwnerUsername: uname,
		OwnerOrgID:    999, // both set — XOR violated
		OwnerSlug:     "x",
		Name:          "noop",
		Visibility:    "public",
	})
	if err == nil {
		t.Fatal("expected XOR violation error, got nil")
	}
}
