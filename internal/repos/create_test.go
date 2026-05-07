// SPDX-License-Identifier: AGPL-3.0-or-later

package repos_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	"github.com/tenseleyFlow/shithub/internal/repos"
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
