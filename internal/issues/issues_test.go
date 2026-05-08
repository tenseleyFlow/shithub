// SPDX-License-Identifier: AGPL-3.0-or-later

package issues_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// setup spins a fresh test DB, creates a user + repo, seeds the issue
// counter, and returns the things the issue tests need.
func setup(t *testing.T) (*pgxpool.Pool, issues.Deps, int64, int64) {
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

	deps := issues.Deps{
		Pool:    pool,
		Limiter: throttle.NewLimiter(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return pool, deps, user.ID, repo.ID
}

func TestCreate_AllocatesSequentialNumbers(t *testing.T) {
	_, deps, uid, rid := setup(t)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		got, err := issues.Create(ctx, deps, issues.CreateParams{
			RepoID: rid, AuthorUserID: uid, Title: "title", Body: "body",
		})
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if got.Number != i {
			t.Errorf("issue %d: got number %d, want %d", i, got.Number, i)
		}
	}
}

func TestCreate_ConcurrentRaceForUniqueNumbers(t *testing.T) {
	_, deps, uid, rid := setup(t)
	ctx := context.Background()
	const N = 8
	got := make([]int64, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			row, err := issues.Create(ctx, deps, issues.CreateParams{
				RepoID: rid, AuthorUserID: uid, Title: "race", Body: "body",
			})
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			got[i] = row.Number
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("duplicate number %d in %v", n, got)
		}
		seen[n] = true
		if n < 1 || n > N {
			t.Errorf("number %d out of [1,%d] range", n, N)
		}
	}
}

func TestCreate_RejectsEmptyTitle(t *testing.T) {
	_, deps, uid, rid := setup(t)
	ctx := context.Background()
	_, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "   ", Body: "body",
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected ErrEmptyTitle, got %v", err)
	}
}

func TestCreate_RendersHTMLAndSanitizesScripts(t *testing.T) {
	pool, deps, uid, rid := setup(t)
	ctx := context.Background()
	row, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID:       rid,
		AuthorUserID: uid,
		Title:        "xss",
		Body:         `before <script>alert("xss")</script> after **bold**`,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	iq := issuesdb.New()
	got, err := iq.GetIssueByID(ctx, pool, row.ID)
	if err != nil {
		t.Fatalf("GetIssueByID: %v", err)
	}
	if !got.BodyHtmlCached.Valid {
		t.Fatalf("expected cached body html")
	}
	html := got.BodyHtmlCached.String
	if strings.Contains(strings.ToLower(html), "<script") {
		t.Errorf("sanitized html should not contain <script>: %q", html)
	}
	if !strings.Contains(html, "<strong>") {
		t.Errorf("markdown should render bold: %q", html)
	}
}

func TestAddComment_LockedRejectsNonCollab(t *testing.T) {
	pool, deps, uid, rid := setup(t)
	ctx := context.Background()
	row, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "lock-me", Body: "",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := issues.SetLock(ctx, deps, uid, row.ID, true, "spam"); err != nil {
		t.Fatalf("SetLock: %v", err)
	}
	uq := usersdb.New()
	other, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "carol", DisplayName: "Carol", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err = issues.AddComment(ctx, deps, issues.CommentCreateParams{
		IssueID:      row.ID,
		AuthorUserID: other.ID,
		Body:         "let me in",
		IsCollab:     false,
	})
	if err == nil {
		t.Fatalf("expected ErrIssueLocked, got nil")
	}
}

// TestAddComment_LockedAllowsCollab is the positive companion to the
// previous test: when the orchestrator is told `IsCollab=true` the
// locked gate must yield. This guards the policy contract — a triage+
// collaborator is allowed to post past a lock so they can wrap up
// drive-by spam threads. (S00-S25 audit, finding C3.)
func TestAddComment_LockedAllowsCollab(t *testing.T) {
	pool, deps, uid, rid := setup(t)
	ctx := context.Background()
	row, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "lock-me", Body: "",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := issues.SetLock(ctx, deps, uid, row.ID, true, "spam"); err != nil {
		t.Fatalf("SetLock: %v", err)
	}
	uq := usersdb.New()
	collab, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "tessie", DisplayName: "Tessie", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	c, err := issues.AddComment(ctx, deps, issues.CommentCreateParams{
		IssueID:      row.ID,
		AuthorUserID: collab.ID,
		Body:         "wrapping up the thread",
		IsCollab:     true,
	})
	if err != nil {
		t.Fatalf("expected lock bypass for collab, got %v", err)
	}
	if c.ID == 0 {
		t.Errorf("returned comment had zero ID")
	}
}

func TestSetState_EmitsEvent(t *testing.T) {
	pool, deps, uid, rid := setup(t)
	ctx := context.Background()
	row, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "close-me", Body: "",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := issues.SetState(ctx, deps, uid, row.ID, "closed", "completed"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := issues.SetState(ctx, deps, uid, row.ID, "open", ""); err != nil {
		t.Fatalf("SetState reopen: %v", err)
	}
	iq := issuesdb.New()
	events, err := iq.ListIssueEvents(ctx, pool, row.ID)
	if err != nil {
		t.Fatalf("ListIssueEvents: %v", err)
	}
	kinds := []string{}
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"closed", "reopened"}
	if len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Errorf("got events %v, want %v", kinds, want)
	}
}

func TestCreate_CrossReferenceCreatesEventOnTarget(t *testing.T) {
	pool, deps, uid, rid := setup(t)
	ctx := context.Background()
	first, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "first", Body: "no refs",
	})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if _, err := issues.Create(ctx, deps, issues.CreateParams{
		RepoID: rid, AuthorUserID: uid, Title: "second", Body: "fixes #" + itoa(first.Number),
	}); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	iq := issuesdb.New()
	events, err := iq.ListIssueEvents(ctx, pool, first.ID)
	if err != nil {
		t.Fatalf("ListIssueEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "referenced" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected `referenced` event on target issue, got %#v", events)
	}
}

// itoa minimizes deps in tests.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	out := make([]byte, 0, 4)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		out = append([]byte{byte('0' + v%10)}, out...)
		v /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}
