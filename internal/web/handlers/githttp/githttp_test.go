// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	githttph "github.com/tenseleyFlow/shithub/internal/web/handlers/githttp"
)

// fixtureHash is a static PHC test fixture (zero salt, zero key) — not a credential.
const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// gitCmd wraps exec.Command — test paths are all t.TempDir.
func gitCmd(args ...string) *exec.Cmd {
	//nolint:gosec // G204: test fixture, t.TempDir paths.
	return exec.Command("git", args...)
}

// env wires a verified user + a public repo (with init commit) + a
// private repo, mounts the smart-HTTP handlers on a httptest server,
// and returns everything callers need.
type env struct {
	srv        *httptest.Server
	pool       *pgxpool.Pool
	userID     int64
	user       string
	pwd        string
	patRaw     string
	pubRepo    string
	pubRepoID  int64
	privRepo   string
	privRepoID int64
	root       string
	runnerJWT  *runnerjwt.Signer
}

func setupEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	root := t.TempDir()
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}

	uq := usersdb.New()
	user, err := uq.CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
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

	// Public repo with one initial commit (so info/refs returns refs).
	rdeps := repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	publicRes, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: user.Username,
		Name: "public-repo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("create public: %v", err)
	}
	privateRes, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerUserID: user.ID, OwnerUsername: user.Username,
		Name: "private-repo", Visibility: "private", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("create private: %v", err)
	}

	// Mint a PAT for alice.
	raw, hash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("mint PAT: %v", err)
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	if _, err := uq.InsertUserToken(context.Background(), pool, usersdb.InsertUserTokenParams{
		UserID:      user.ID,
		Name:        "test",
		TokenHash:   hash,
		TokenPrefix: prefix,
		ExpiresAt:   expires,
		Scopes:      []string{"repo"},
	}); err != nil {
		t.Fatalf("create PAT row: %v", err)
	}

	runnerJWT := runnerHTTPSigner(t)
	h, err := githttph.New(githttph.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:   pool, RepoFS: rfs, RunnerJWT: runnerJWT,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := chi.NewRouter()
	h.MountSmartHTTP(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &env{
		srv: srv, pool: pool, userID: user.ID,
		user: "alice", pwd: "wrong-not-real-password", patRaw: raw,
		pubRepo: "public-repo", pubRepoID: publicRes.Repo.ID,
		privRepo: "private-repo", privRepoID: privateRes.Repo.ID,
		root: root, runnerJWT: runnerJWT,
	}
}

// authedURL embeds Basic credentials in the URL. git's libcurl honors
// the userinfo prefix for HTTP Basic.
func authedURL(srvURL, user, secret, repoPath string) string {
	u, _ := url.Parse(srvURL)
	u.User = url.UserPassword(user, secret)
	u.Path = repoPath
	return u.String()
}

func TestGitHTTP_AnonClonePublic(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	dst := filepath.Join(t.TempDir(), "clone")
	out, err := gitCmd("clone", env.srv.URL+"/alice/public-repo.git", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	// HEAD must resolve to the initial commit's tree.
	out, err = gitCmd("-C", dst, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list = %q, want 1", got)
	}
}

func TestGitHTTP_AnonClonePrivateFails(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	dst := filepath.Join(t.TempDir(), "clone")
	cmd := gitCmd("clone", env.srv.URL+"/alice/private-repo.git", dst)
	// Suppress git's interactive credential prompt during the test.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected clone to fail; output: %s", out)
	}
}

func TestGitHTTP_PATClonePrivate(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	dst := filepath.Join(t.TempDir(), "clone")
	cloneURL := authedURL(env.srv.URL, env.user, env.patRaw, "/alice/private-repo.git")
	out, err := gitCmd("clone", cloneURL, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	out, err = gitCmd("-C", dst, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list = %q, want 1", got)
	}
}

func TestGitHTTP_RunnerCheckoutTokenClonesPrivateRepoReadOnly(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	token := env.runnerCheckoutToken(t, env.privRepoID)

	dst := filepath.Join(t.TempDir(), "clone")
	cloneURL := authedURL(env.srv.URL, "shithub-actions", token, "/alice/private-repo.git")
	out, err := gitCmd("clone", cloneURL, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone with checkout token: %v\n%s", err, out)
	}
	out, err = gitCmd("-C", dst, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list = %q, want 1", got)
	}

	for _, c := range [][]string{
		{"-C", dst, "config", "user.name", "Runner"},
		{"-C", dst, "config", "user.email", "runner@example.test"},
	} {
		if out, err := gitCmd(c...).CombinedOutput(); err != nil {
			t.Fatalf("config: %v\n%s", err, out)
		}
	}
	if err := writeFile(filepath.Join(dst, "blocked.txt"), "x\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, c := range [][]string{
		{"-C", dst, "add", "blocked.txt"},
		{"-C", dst, "commit", "-m", "blocked"},
	} {
		if out, err := gitCmd(c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c, err, out)
		}
	}
	out, err = gitCmd("-C", dst, "push", "origin", "trunk").CombinedOutput()
	if err == nil {
		t.Fatalf("checkout token push unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "read-only") {
		t.Fatalf("expected read-only checkout-token message, got: %s", out)
	}
}

func TestGitHTTP_RunnerCheckoutTokenCannotReadOtherRepo(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	token := env.runnerCheckoutToken(t, env.pubRepoID)
	dst := filepath.Join(t.TempDir(), "clone")
	cmd := gitCmd("clone", authedURL(env.srv.URL, "shithub-actions", token, "/alice/private-repo.git"), dst)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected checkout token for public repo to fail against private repo; output: %s", out)
	}
}

func TestGitHTTP_PATPushRoundtrip(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	dst := filepath.Join(t.TempDir(), "clone")
	cloneURL := authedURL(env.srv.URL, env.user, env.patRaw, "/alice/private-repo.git")

	// Clone.
	if out, err := gitCmd("clone", cloneURL, dst).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// Configure committer + make a new commit.
	for _, c := range [][]string{
		{"-C", dst, "config", "user.name", "Alice"},
		{"-C", dst, "config", "user.email", "alice@example.com"},
	} {
		if out, err := gitCmd(c...).CombinedOutput(); err != nil {
			t.Fatalf("config: %v\n%s", err, out)
		}
	}
	newFile := filepath.Join(dst, "newfile.txt")
	if err := writeFile(newFile, "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, c := range [][]string{
		{"-C", dst, "add", "newfile.txt"},
		{"-C", dst, "commit", "-m", "add newfile"},
	} {
		if out, err := gitCmd(c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c, err, out)
		}
	}

	// Push.
	out, err := gitCmd("-C", dst, "push", "origin", "trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// Re-clone to a fresh dir; the new commit must be visible.
	dst2 := filepath.Join(t.TempDir(), "clone2")
	if out, err := gitCmd("clone", cloneURL, dst2).CombinedOutput(); err != nil {
		t.Fatalf("clone2: %v\n%s", err, out)
	}
	if out, err := gitCmd("-C", dst2, "log", "--oneline", "trunk").CombinedOutput(); err != nil {
		t.Fatalf("log: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "add newfile") {
		t.Fatalf("expected pushed commit in re-clone log; got: %s", out)
	}
}

func TestGitHTTP_PushToArchivedRejected(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)
	// Archive the public repo.
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE repos SET is_archived = true WHERE name = 'public-repo'"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Authed clone is fine (read still allowed).
	dst := filepath.Join(t.TempDir(), "clone")
	cloneURL := authedURL(env.srv.URL, env.user, env.patRaw, "/alice/public-repo.git")
	if out, err := gitCmd("clone", cloneURL, dst).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// Make a commit and try to push.
	for _, c := range [][]string{
		{"-C", dst, "config", "user.name", "Alice"},
		{"-C", dst, "config", "user.email", "alice@example.com"},
	} {
		_, _ = gitCmd(c...).CombinedOutput()
	}
	_ = writeFile(filepath.Join(dst, "blocked.txt"), "x\n")
	for _, c := range [][]string{
		{"-C", dst, "add", "blocked.txt"},
		{"-C", dst, "commit", "-m", "blocked"},
	} {
		_, _ = gitCmd(c...).CombinedOutput()
	}

	out, err := gitCmd("-C", dst, "push", "origin", "trunk").CombinedOutput()
	if err == nil {
		t.Fatalf("expected push to fail; output: %s", out)
	}
	if !strings.Contains(string(out), "archived") {
		t.Fatalf("expected friendly archived-message in stderr, got: %s", out)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func (e *env) runnerCheckoutToken(t *testing.T, repoID int64) string {
	t.Helper()
	ctx := context.Background()
	q := actionsdb.New()
	runner, err := q.InsertRunner(ctx, e.pool, actionsdb.InsertRunnerParams{
		Name:     "runner-" + e.privRepo,
		Labels:   []string{"ubuntu-latest"},
		Capacity: 1,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	run, err := q.InsertWorkflowRun(ctx, e.pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repoID,
		RunIndex:     1,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "CI",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "refs/heads/trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: e.userID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	job, err := q.InsertWorkflowJob(ctx, e.pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "checkout",
		JobName:        "checkout",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workflow_jobs SET runner_id = $1, status = 'running', started_at = now() WHERE id = $2`, runner.ID, job.ID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}
	token, _, err := e.runnerJWT.Mint(runnerjwt.MintParams{
		RunnerID: runner.ID,
		JobID:    job.ID,
		RunID:    run.ID,
		RepoID:   repoID,
		Purpose:  runnerjwt.PurposeCheckout,
	})
	if err != nil {
		t.Fatalf("Mint checkout token: %v", err)
	}
	return token
}

func runnerHTTPSigner(t *testing.T) *runnerjwt.Signer {
	t.Helper()
	signer, err := runnerjwt.NewFromKey(
		bytesOf(0x88, 32),
		runnerjwt.WithClock(func() time.Time { return time.Now().UTC() }),
	)
	if err != nil {
		t.Fatalf("runnerjwt.NewFromKey: %v", err)
	}
	return signer
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// TestGitHTTP_AnonCloneOrgOwnedPublic is a regression for an outage
// on 2026-05-09: pushing to https://shithub.sh/tenseleyflow/shithub.git
// returned 404 because authorizeForService only resolved owner-slugs
// as users (the comment in handler.go admitted "orgs come in S31").
// Once that landed in production with an org-owned repo, the smart-
// HTTP route became silently unusable for any org repo.
func TestGitHTTP_AnonCloneOrgOwnedPublic(t *testing.T) {
	t.Parallel()
	env := setupEnv(t)

	// Create an org owned by alice and a public repo under it.
	org, err := orgs.Create(context.Background(), orgs.Deps{
		Pool:   env.pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:  audit.NewRecorder(),
	}, orgs.CreateParams{
		Slug: "myorg", DisplayName: "MyOrg", CreatedByUserID: env.userID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	rfs, err := storage.NewRepoFS(env.root)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	rdeps := repos.Deps{
		Pool: env.pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := repos.Create(context.Background(), rdeps, repos.Params{
		OwnerOrgID: org.ID, OwnerSlug: org.Slug, ActorUserID: env.userID,
		Name: "org-public", Visibility: "public", InitReadme: true,
	}); err != nil {
		t.Fatalf("create org repo: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	out, err := gitCmd("clone", env.srv.URL+"/myorg/org-public.git", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone org-owned repo: %v\n%s", err, out)
	}
	out, err = gitCmd("-C", dst, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list = %q, want 1", got)
	}
}
