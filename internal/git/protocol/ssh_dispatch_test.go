// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/git/protocol"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// dispatchEnv constructs deps + 2 users (alice owns repos, eve is a
// non-owner) + a public repo + a private repo against a fresh test DB.
type dispatchEnv struct {
	pool  *pgxpool.Pool
	deps  protocol.SSHDispatchDeps
	alice int64
	eve   int64
	root  string
}

func setupDispatch(t *testing.T) *dispatchEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	uq := usersdb.New()

	makeUser := func(name string) usersdb.User {
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
		if err := uq.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
			ID: u.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
		}); err != nil {
			t.Fatalf("LinkUserPrimaryEmail %s: %v", name, err)
		}
		return u
	}
	alice := makeUser("alice")
	eve := makeUser("eve")

	rdeps := repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
	}
	if _, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerUserID: alice.ID, OwnerUsername: alice.Username,
		Name: "public", Visibility: "public", InitReadme: true,
	}); err != nil {
		t.Fatalf("create public: %v", err)
	}
	if _, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerUserID: alice.ID, OwnerUsername: alice.Username,
		Name: "private", Visibility: "private", InitReadme: true,
	}); err != nil {
		t.Fatalf("create private: %v", err)
	}

	return &dispatchEnv{
		pool: pool, deps: protocol.SSHDispatchDeps{Pool: pool, RepoFS: rfs},
		alice: alice.ID, eve: eve.ID, root: root,
	}
}

func TestDispatch_PublicCloneByOwner(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	res, parsed, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/public'",
		UserID:          env.alice,
		RemoteIP:        "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("PrepareDispatch: %v", err)
	}
	if parsed.Service != protocol.UploadPack {
		t.Errorf("Service = %q", parsed.Service)
	}
	if !strings.HasSuffix(res.Argv0Args[1], "/public.git") {
		t.Errorf("Argv0Args[1] = %q", res.Argv0Args[1])
	}
	wantEnvSubstrings := []string{
		"SHITHUB_USER_ID=", "SHITHUB_USERNAME=alice",
		"SHITHUB_REPO_FULL_NAME=alice/public", "SHITHUB_PROTOCOL=ssh",
		"SHITHUB_REMOTE_IP=127.0.0.1",
	}
	envBlob := strings.Join(res.Env, "\n")
	for _, w := range wantEnvSubstrings {
		if !strings.Contains(envBlob, w) {
			t.Errorf("env missing %q in:\n%s", w, envBlob)
		}
	}
}

func TestDispatch_PublicCloneByOther(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/public'",
		UserID:          env.eve, // not owner
	})
	if err != nil {
		t.Fatalf("non-owner pull of public: %v", err)
	}
}

func TestDispatch_PrivateCloneByOtherIsNotFound(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/private'",
		UserID:          env.eve,
	})
	if !errors.Is(err, protocol.ErrSSHRepoNotFound) {
		t.Fatalf("err = %v, want ErrSSHRepoNotFound", err)
	}
}

func TestDispatch_PushByNonOwnerIsPermDenied(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/public'",
		UserID:          env.eve,
	})
	if !errors.Is(err, protocol.ErrSSHPermDenied) {
		t.Fatalf("err = %v, want ErrSSHPermDenied", err)
	}
}

func TestDispatch_PushToArchivedIsArchived(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE repos SET is_archived = true WHERE name = 'public'"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/public'",
		UserID:          env.alice,
	})
	if !errors.Is(err, protocol.ErrSSHArchived) {
		t.Fatalf("err = %v, want ErrSSHArchived", err)
	}
}

func TestDispatch_SuspendedUserSuspended(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	if _, err := env.pool.Exec(
		context.Background(),
		"UPDATE users SET suspended_at = now(), suspended_reason = 'test' WHERE id = $1",
		env.alice,
	); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/public'",
		UserID:          env.alice,
	})
	if !errors.Is(err, protocol.ErrSSHSuspended) {
		t.Fatalf("err = %v, want ErrSSHSuspended", err)
	}
}

func TestDispatch_UnknownCommandIsRejected(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "ls -la /etc",
		UserID:          env.alice,
	})
	if !errors.Is(err, protocol.ErrUnknownSSHCommand) {
		t.Fatalf("err = %v, want ErrUnknownSSHCommand", err)
	}
}

func TestFriendlyMessageFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{protocol.ErrSSHRepoNotFound, "shithub: repository not found"},
		{protocol.ErrSSHPermDenied, "shithub: permission denied"},
		{protocol.ErrSSHArchived, "shithub: this repository is archived; pushes are disabled"},
		{protocol.ErrSSHSuspended, "shithub: your account is suspended"},
		{protocol.ErrSSHRequires2FA, "shithub: this organization requires two-factor authentication"},
		{protocol.ErrUnknownSSHCommand, "shithub does not allow shell access"},
		{protocol.ErrInvalidSSHPath, "shithub: repository not found"},
	}
	for _, c := range cases {
		if got := protocol.FriendlyMessageFor(c.err, "abc123"); got != c.want {
			t.Errorf("FriendlyMessageFor(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestParseRemoteIP(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                               "",
		"203.0.113.7 12345 192.0.2.1 22": "203.0.113.7",
		"  203.0.113.8  ":                "203.0.113.8",
	}
	for in, want := range cases {
		if got := protocol.ParseRemoteIP(in); got != want {
			t.Errorf("ParseRemoteIP(%q) = %q, want %q", in, got, want)
		}
	}
}
