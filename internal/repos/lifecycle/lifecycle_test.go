// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type env struct {
	deps  lifecycle.Deps
	rdeps repos.Deps

	alice usersdb.User
	bob   usersdb.User

	repoID     int64
	originalFS string
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	uq := usersdb.New()
	mk := func(name string) usersdb.User {
		u, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
			Username: name, DisplayName: name, PasswordHash: fixtureHash,
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		em, err := uq.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
			UserID: u.ID, Email: name + "@example.com", IsPrimary: true, Verified: true,
		})
		if err != nil {
			t.Fatalf("CreateUserEmail %s: %v", name, err)
		}
		_ = uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
			ID: u.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
		})
		return u
	}
	alice := mk("alice")
	bob := mk("bob")

	rdeps := repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}
	res, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerUserID: alice.ID, OwnerUsername: alice.Username,
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}

	return &env{
		deps:  lifecycle.Deps{Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder()},
		rdeps: rdeps,
		alice: alice, bob: bob,
		repoID:     res.Repo.ID,
		originalFS: res.DiskPath,
	}
}

func TestRename_HappyPath(t *testing.T) {
	t.Parallel()
	env := setup(t)
	if err := lifecycle.Rename(context.Background(), env.deps, lifecycle.RenameParams{
		ActorUserID: env.alice.ID,
		RepoID:      env.repoID,
		OwnerUserID: env.alice.ID,
		OwnerName:   "alice",
		OldName:     "demo",
		NewName:     "renamed",
	}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(context.Background(), env.deps.Pool, env.repoID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	if repo.Name != "renamed" {
		t.Errorf("name = %q, want renamed", repo.Name)
	}
	// Redirect row exists.
	rid, err := rq.LookupRedirectByUserOwner(context.Background(), env.deps.Pool, reposdb.LookupRedirectByUserOwnerParams{
		OldOwnerUserID: pgtype.Int8{Int64: env.alice.ID, Valid: true},
		OldName:        "demo",
	})
	if err != nil || rid != env.repoID {
		t.Errorf("LookupRedirect: id=%d err=%v", rid, err)
	}
	// FS dir moved.
	newPath := filepath.Join(filepath.Dir(env.originalFS), "renamed.git")
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new path missing: %v", err)
	}
}

func TestRename_RejectsSameName(t *testing.T) {
	t.Parallel()
	env := setup(t)
	err := lifecycle.Rename(context.Background(), env.deps, lifecycle.RenameParams{
		RepoID: env.repoID, OwnerUserID: env.alice.ID, OwnerName: "alice",
		OldName: "demo", NewName: "demo",
	})
	if !errors.Is(err, lifecycle.ErrSameName) {
		t.Errorf("err = %v, want ErrSameName", err)
	}
}

func TestRename_RateLimit(t *testing.T) {
	t.Parallel()
	env := setup(t)
	for i := 0; i < 5; i++ {
		newName := []string{"a1", "a2", "a3", "a4", "a5"}[i]
		oldName := "demo"
		if i > 0 {
			oldName = []string{"a1", "a2", "a3", "a4"}[i-1]
		}
		if err := lifecycle.Rename(context.Background(), env.deps, lifecycle.RenameParams{
			RepoID: env.repoID, OwnerUserID: env.alice.ID, OwnerName: "alice",
			OldName: oldName, NewName: newName,
		}); err != nil {
			t.Fatalf("rename %d: %v", i, err)
		}
	}
	err := lifecycle.Rename(context.Background(), env.deps, lifecycle.RenameParams{
		RepoID: env.repoID, OwnerUserID: env.alice.ID, OwnerName: "alice",
		OldName: "a5", NewName: "a6",
	})
	if !errors.Is(err, lifecycle.ErrRenameRateLimited) {
		t.Errorf("6th rename: err=%v, want ErrRenameRateLimited", err)
	}
}

func TestArchiveUnarchive(t *testing.T) {
	t.Parallel()
	env := setup(t)
	if err := lifecycle.Archive(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := lifecycle.Archive(context.Background(), env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrAlreadyArchived) {
		t.Errorf("double archive: err=%v, want ErrAlreadyArchived", err)
	}
	if err := lifecycle.Unarchive(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if err := lifecycle.Unarchive(context.Background(), env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrNotArchived) {
		t.Errorf("double unarchive: err=%v, want ErrNotArchived", err)
	}
}

func TestSetVisibility(t *testing.T) {
	t.Parallel()
	env := setup(t)
	if err := lifecycle.SetVisibility(context.Background(), env.deps, env.alice.ID, env.repoID, "private"); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	rq := reposdb.New()
	repo, _ := rq.GetRepoByID(context.Background(), env.deps.Pool, env.repoID)
	if string(repo.Visibility) != "private" {
		t.Errorf("Visibility = %q, want private", repo.Visibility)
	}
	if err := lifecycle.SetVisibility(context.Background(), env.deps, env.alice.ID, env.repoID, "bogus"); !errors.Is(err, lifecycle.ErrInvalidVisibility) {
		t.Errorf("invalid: err=%v, want ErrInvalidVisibility", err)
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	t.Parallel()
	env := setup(t)
	deletedPath, err := env.deps.RepoFS.DeletedRepoPath("alice", "demo", env.repoID)
	if err != nil {
		t.Fatalf("DeletedRepoPath: %v", err)
	}
	if err := lifecycle.SoftDelete(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := os.Stat(env.originalFS); !os.IsNotExist(err) {
		t.Fatalf("canonical path after soft-delete: err = %v, want not exist", err)
	}
	if _, err := os.Stat(deletedPath); err != nil {
		t.Fatalf("deleted tombstone missing: %v", err)
	}
	if err := lifecycle.SoftDelete(context.Background(), env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrAlreadyDeleted) {
		t.Errorf("double soft-delete: err=%v", err)
	}
	if err := lifecycle.Restore(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(env.originalFS); err != nil {
		t.Fatalf("canonical path after restore missing: %v", err)
	}
	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Fatalf("deleted tombstone after restore: err = %v, want not exist", err)
	}
	if err := lifecycle.Restore(context.Background(), env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrNotDeleted) {
		t.Errorf("double restore: err=%v", err)
	}
}

func TestRestore_RefusesWhenNameReused(t *testing.T) {
	t.Parallel()
	env := setup(t)
	ctx := context.Background()
	if err := lifecycle.SoftDelete(ctx, env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	replacement, err := repos.Create(ctx, env.rdeps, repos.Params{
		OwnerUserID: env.alice.ID, OwnerUsername: env.alice.Username,
		Name: "demo", Visibility: "public",
	})
	if err != nil {
		t.Fatalf("replacement create: %v", err)
	}
	if err := lifecycle.Restore(ctx, env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrNameTaken) {
		t.Fatalf("Restore: err = %v, want ErrNameTaken", err)
	}
	if _, err := os.Stat(replacement.DiskPath); err != nil {
		t.Fatalf("replacement canonical path missing: %v", err)
	}
	deletedPath, err := env.deps.RepoFS.DeletedRepoPath("alice", "demo", env.repoID)
	if err != nil {
		t.Fatalf("DeletedRepoPath: %v", err)
	}
	if _, err := os.Stat(deletedPath); err != nil {
		t.Fatalf("old tombstone should remain restorable: %v", err)
	}
}

func TestRestore_PastGraceRefuses(t *testing.T) {
	t.Parallel()
	env := setup(t)
	// Pin Now to 8 days from now after a soft-delete to simulate
	// past-grace state.
	if err := lifecycle.SoftDelete(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatal(err)
	}
	env.deps.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := lifecycle.Restore(context.Background(), env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrPastGrace) {
		t.Errorf("past-grace restore: err=%v, want ErrPastGrace", err)
	}
}

func TestTransfer_AcceptHappyPath(t *testing.T) {
	t.Parallel()
	env := setup(t)
	id, err := lifecycle.RequestTransfer(context.Background(), env.deps, lifecycle.TransferRequestParams{
		ActorUserID: env.alice.ID, RepoID: env.repoID,
		FromUserID: env.alice.ID, ToPrincipalKind: "user", ToPrincipalID: env.bob.ID,
	})
	if err != nil {
		t.Fatalf("RequestTransfer: %v", err)
	}
	if err := lifecycle.AcceptTransfer(context.Background(), env.deps, env.bob.ID, id); err != nil {
		t.Fatalf("AcceptTransfer: %v", err)
	}
	rq := reposdb.New()
	repo, _ := rq.GetRepoByID(context.Background(), env.deps.Pool, env.repoID)
	if !repo.OwnerUserID.Valid || repo.OwnerUserID.Int64 != env.bob.ID {
		t.Errorf("owner_user_id = %v, want %d", repo.OwnerUserID, env.bob.ID)
	}
	// Redirect from alice/demo → repo_id should resolve.
	rid, err := rq.LookupRedirectByUserOwner(context.Background(), env.deps.Pool, reposdb.LookupRedirectByUserOwnerParams{
		OldOwnerUserID: pgtype.Int8{Int64: env.alice.ID, Valid: true},
		OldName:        "demo",
	})
	if err != nil || rid != env.repoID {
		t.Errorf("redirect: id=%d err=%v", rid, err)
	}
}

func TestTransfer_DeclineLeavesOwnerUnchanged(t *testing.T) {
	t.Parallel()
	env := setup(t)
	id, _ := lifecycle.RequestTransfer(context.Background(), env.deps, lifecycle.TransferRequestParams{
		ActorUserID: env.alice.ID, RepoID: env.repoID,
		FromUserID: env.alice.ID, ToPrincipalKind: "user", ToPrincipalID: env.bob.ID,
	})
	if err := lifecycle.DeclineTransfer(context.Background(), env.deps, env.bob.ID, id); err != nil {
		t.Fatalf("DeclineTransfer: %v", err)
	}
	rq := reposdb.New()
	repo, _ := rq.GetRepoByID(context.Background(), env.deps.Pool, env.repoID)
	if !repo.OwnerUserID.Valid || repo.OwnerUserID.Int64 != env.alice.ID {
		t.Errorf("owner changed despite decline: %v", repo.OwnerUserID)
	}
}

func TestTransfer_CancelByOwner(t *testing.T) {
	t.Parallel()
	env := setup(t)
	id, _ := lifecycle.RequestTransfer(context.Background(), env.deps, lifecycle.TransferRequestParams{
		ActorUserID: env.alice.ID, RepoID: env.repoID,
		FromUserID: env.alice.ID, ToPrincipalKind: "user", ToPrincipalID: env.bob.ID,
	})
	if err := lifecycle.CancelTransfer(context.Background(), env.deps, env.alice.ID, id); err != nil {
		t.Fatalf("CancelTransfer: %v", err)
	}
	// Bob's accept now fails.
	if err := lifecycle.AcceptTransfer(context.Background(), env.deps, env.bob.ID, id); !errors.Is(err, lifecycle.ErrTransferTerminal) {
		t.Errorf("accept-after-cancel: err=%v, want ErrTransferTerminal", err)
	}
}

func TestTransfer_ExpireSweepFlipsPending(t *testing.T) {
	t.Parallel()
	env := setup(t)
	_, _ = lifecycle.RequestTransfer(context.Background(), env.deps, lifecycle.TransferRequestParams{
		ActorUserID: env.alice.ID, RepoID: env.repoID,
		FromUserID: env.alice.ID, ToPrincipalKind: "user", ToPrincipalID: env.bob.ID,
	})
	// Force the row past its expires_at by SQL update so we don't need
	// to advance lifecycle.now() through the call path.
	_, err := env.deps.Pool.Exec(context.Background(),
		`UPDATE repo_transfer_requests SET expires_at = now() - interval '1 minute' WHERE repo_id = $1`, env.repoID)
	if err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	n, err := lifecycle.ExpirePending(context.Background(), env.deps)
	if err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	if n != 1 {
		t.Errorf("expired count = %d, want 1", n)
	}
}

func TestHardDelete_PastGraceCascades(t *testing.T) {
	t.Parallel()
	env := setup(t)
	deletedPath, err := env.deps.RepoFS.DeletedRepoPath("alice", "demo", env.repoID)
	if err != nil {
		t.Fatalf("DeletedRepoPath: %v", err)
	}
	if err := lifecycle.SoftDelete(context.Background(), env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatal(err)
	}
	// Force deleted_at past grace and pin Now likewise.
	_, _ = env.deps.Pool.Exec(context.Background(),
		`UPDATE repos SET deleted_at = now() - interval '8 days' WHERE id = $1`, env.repoID)
	env.deps.Now = time.Now

	if err := lifecycle.HardDelete(context.Background(), env.deps, 0, env.repoID); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	// Repo row gone.
	rq := reposdb.New()
	if _, err := rq.GetRepoByID(context.Background(), env.deps.Pool, env.repoID); err == nil {
		t.Errorf("repo row still present after hard-delete")
	}
	// Tombstone dir gone.
	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Errorf("deleted tombstone still present: err=%v", err)
	}
}
