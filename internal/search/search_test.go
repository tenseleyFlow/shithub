// SPDX-License-Identifier: AGPL-3.0-or-later

package search_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/search"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestParseQuery covers the operator parser end-to-end.
func TestParseQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want search.ParsedQuery
	}{
		{"", search.ParsedQuery{}},
		{"hello world", search.ParsedQuery{Text: "hello world"}},
		{`"quoted phrase"`, search.ParsedQuery{Phrase: "quoted phrase"}},
		{"repo:alice/demo bug", search.ParsedQuery{
			Text:       "bug",
			RepoFilter: &search.RepoFilter{Owner: "alice", Name: "demo"},
		}},
		{"repo:noslash bug", search.ParsedQuery{Text: "repo:noslash bug"}},
		{"is:open broken", search.ParsedQuery{Text: "broken", StateFilter: "open"}},
		{"state:closed bug", search.ParsedQuery{Text: "bug", StateFilter: "closed"}},
		{"author:bob fix", search.ParsedQuery{Text: "fix", AuthorFilter: "bob"}},
		{"assignee:bob bug", search.ParsedQuery{Text: "bug", AssigneeFilter: "bob"}},
		{"assignee: bug", search.ParsedQuery{Text: "assignee: bug"}},
		{"user:alice", search.ParsedQuery{OwnerFilter: "alice"}},
		{"org:tenseleyflow shithub", search.ParsedQuery{Text: "shithub", OwnerFilter: "tenseleyflow"}},
		{"user: nothing", search.ParsedQuery{Text: "user: nothing"}},
		{"language:Go x", search.ParsedQuery{Text: "language:Go x"}},
	}
	for _, c := range cases {
		got := search.ParseQuery(c.in)
		if got.Text != c.want.Text || got.Phrase != c.want.Phrase ||
			got.StateFilter != c.want.StateFilter || got.AuthorFilter != c.want.AuthorFilter ||
			got.AssigneeFilter != c.want.AssigneeFilter || got.OwnerFilter != c.want.OwnerFilter {
			t.Errorf("ParseQuery(%q):\n  got  %+v\n  want %+v", c.in, got, c.want)
			continue
		}
		if (got.RepoFilter == nil) != (c.want.RepoFilter == nil) {
			t.Errorf("ParseQuery(%q): repo-filter presence mismatch", c.in)
			continue
		}
		if got.RepoFilter != nil && (*got.RepoFilter != *c.want.RepoFilter) {
			t.Errorf("ParseQuery(%q): repo-filter %+v, want %+v",
				c.in, *got.RepoFilter, *c.want.RepoFilter)
		}
	}
}

// TestParseQuery_TruncatesOverlong ensures the input cap fires.
func TestParseQuery_TruncatesOverlong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", search.MaxQueryBytes+50)
	got := search.ParseQuery(long)
	if len(got.Text) > search.MaxQueryBytes {
		t.Errorf("Text len = %d, want ≤ %d", len(got.Text), search.MaxQueryBytes)
	}
}

// fxs is a fixture for visibility tests: alice owns one public + one
// private repo, each with one issue. bob is a separate user, no
// access to the private side.
type fxs struct {
	deps    search.Deps
	alice   usersdb.User
	bob     usersdb.User
	pubRepo reposdb.Repo
	prvRepo reposdb.Repo
	orgRepo reposdb.Repo
}

func setup(t *testing.T) fxs {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	alice, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "bob", DisplayName: "Bob", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	org, err := orgs.Create(
		ctx,
		orgs.Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		orgs.CreateParams{
			Slug:            "tenseleyflow",
			DisplayName:     "tenseleyFlow",
			Description:     "workflow things",
			BillingEmail:    "org@example.test",
			CreatedByUserID: alice.ID,
		},
	)
	if err != nil {
		t.Fatalf("CreateOrg tenseleyflow: %v", err)
	}

	rq := reposdb.New()
	pubRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: alice.ID, Valid: true},
		Name:          "publicrepo",
		Description:   "a public repo sample",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo public: %v", err)
	}
	prvRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: alice.ID, Valid: true},
		Name:          "privaterepo",
		Description:   "private repo secrets here",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("CreateRepo private: %v", err)
	}
	orgRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "shithub",
		Description:   "A 1:1 reverse-engineering of GitHub. AGPLv3. Without Copilot.",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo org public: %v", err)
	}

	iq := issuesdb.New()
	for _, r := range []reposdb.Repo{pubRepo, prvRepo, orgRepo} {
		if err := iq.EnsureRepoIssueCounter(ctx, pool, r.ID); err != nil {
			t.Fatalf("EnsureRepoIssueCounter: %v", err)
		}
	}
	idep := issues.Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: pubRepo.ID, AuthorUserID: alice.ID,
		Title: "public bug report", Body: "nothing sensitive",
	}); err != nil {
		t.Fatalf("Create issue pub: %v", err)
	}
	if _, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: prvRepo.ID, AuthorUserID: alice.ID,
		Title: "private secret design", Body: "internal only",
	}); err != nil {
		t.Fatalf("Create issue prv: %v", err)
	}
	if _, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: orgRepo.ID, AuthorUserID: alice.ID,
		Title: "org public bug report", Body: "shithub project issue",
	}); err != nil {
		t.Fatalf("Create issue org: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO code_search_paths (repo_id, ref_name, path, tsv)
		VALUES ($1, 'trunk', 'README.md', to_tsvector('shithub_search', 'README shithub'))
	`, orgRepo.ID); err != nil {
		t.Fatalf("seed org code path: %v", err)
	}

	return fxs{
		deps: search.Deps{
			Pool:   pool,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		alice: alice, bob: bob, pubRepo: pubRepo, prvRepo: prvRepo, orgRepo: orgRepo,
	}
}

// TestSearchRepos_AnonymousSeesOnlyPublic guards the visibility
// boundary — the highest-stakes assertion in the search surface.
func TestSearchRepos_AnonymousSeesOnlyPublic(t *testing.T) {
	f := setup(t)
	got, _, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("repo"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	for _, r := range got {
		if r.Visibility == "private" {
			t.Errorf("anonymous saw private repo %q — visibility leak!", r.Name)
		}
	}
	// Sanity: public repo is in the results.
	found := false
	for _, r := range got {
		if r.Name == "publicrepo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected publicrepo in anon results, got %d rows", len(got))
	}
}

// TestSearchRepos_NonCollabOnPrivate matches the spec's private-
// content-stays-private contract.
func TestSearchRepos_NonCollabOnPrivate(t *testing.T) {
	f := setup(t)
	bobActor := policy.UserActor(f.bob.ID, f.bob.Username, false, false)
	got, _, err := search.SearchRepos(context.Background(), f.deps, bobActor,
		search.ParseQuery("secrets"), 20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-collab bob saw %d results for 'secrets', want 0", len(got))
	}
}

// TestSearchRepos_OwnerSeesPrivate confirms the predicate's owner
// branch.
func TestSearchRepos_OwnerSeesPrivate(t *testing.T) {
	f := setup(t)
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)
	got, _, err := search.SearchRepos(context.Background(), f.deps, alice,
		search.ParseQuery("secrets"), 20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("owner alice should see her private repo for 'secrets'")
	}
}

// TestSearchRepos_CollabSeesPrivate exercises the collaborator
// branch of the visibility predicate.
func TestSearchRepos_CollabSeesPrivate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pq := policydb.New()
	if err := pq.UpsertCollabRole(ctx, f.deps.Pool, policydb.UpsertCollabRoleParams{
		RepoID: f.prvRepo.ID, UserID: f.bob.ID, Role: policydb.CollabRoleRead,
	}); err != nil {
		t.Fatalf("UpsertCollabRole: %v", err)
	}
	bobActor := policy.UserActor(f.bob.ID, f.bob.Username, false, false)
	got, _, err := search.SearchRepos(ctx, f.deps, bobActor,
		search.ParseQuery("secrets"), 20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if len(got) == 0 {
		t.Errorf("collab bob should see private repo via 'secrets'")
	}
}

func TestSearchRepos_AnonymousFindsPublicOrgRepoByName(t *testing.T) {
	f := setup(t)
	got, total, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("shithub"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if total == 0 {
		t.Fatalf("SearchRepos total = 0, want org-owned shithub")
	}
	found := false
	for _, r := range got {
		if r.ID == f.orgRepo.ID && r.OwnerUsername == "tenseleyflow" && r.Name == "shithub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("org-owned tenseleyflow/shithub missing from %d repo results", len(got))
	}
}

func TestSearchRepos_AnonymousFindsPublicOrgRepoByOwner(t *testing.T) {
	f := setup(t)
	got, _, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("tenseleyFlow"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	for _, r := range got {
		if r.ID == f.orgRepo.ID && r.OwnerUsername == "tenseleyflow" && r.Name == "shithub" {
			return
		}
	}
	t.Fatalf("owner query did not return org-owned tenseleyflow/shithub; got %d rows", len(got))
}

// TestSearchRepos_OwnerFilterUser is the E23 regression: the
// `user:foo` qualifier must narrow results to repos owned by that
// user. Pre-fix it fell through as free text, returning zero hits.
func TestSearchRepos_OwnerFilterUser(t *testing.T) {
	f := setup(t)
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)
	got, total, err := search.SearchRepos(context.Background(), f.deps, alice,
		search.ParseQuery("user:alice"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if total < 2 { // alice owns at least pubRepo + prvRepo in the fixture
		t.Fatalf("user:alice total = %d, want ≥ 2", total)
	}
	for _, r := range got {
		if r.OwnerUsername != "alice" {
			t.Errorf("user:alice leaked %s/%s", r.OwnerUsername, r.Name)
		}
	}
}

// TestSearchRepos_OwnerFilterOrg covers the `org:` half of the alias:
// the same parser slot, matched against orgs.slug.
func TestSearchRepos_OwnerFilterOrg(t *testing.T) {
	f := setup(t)
	got, _, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("org:tenseleyflow"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	for _, r := range got {
		if r.ID == f.orgRepo.ID {
			return
		}
	}
	t.Fatalf("org:tenseleyflow did not return the org's repo; got %d rows", len(got))
}

// TestSearchRepos_OwnerFilterUnknownReturnsEmpty confirms the filter
// actually narrows — an unknown owner returns zero rows rather than
// the old "fell through to free text" behavior of all repos.
func TestSearchRepos_OwnerFilterUnknownReturnsEmpty(t *testing.T) {
	f := setup(t)
	got, total, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("user:nobody-exists"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if total != 0 || len(got) != 0 {
		t.Errorf("user:nobody-exists returned %d rows (total %d), want 0", len(got), total)
	}
}

// TestSearchIssues_AnonymousSeesOnlyPublic mirrors the repo test
// for the issue surface — issues inherit visibility from their repo.
func TestSearchIssues_AnonymousSeesOnlyPublic(t *testing.T) {
	f := setup(t)
	got, _, err := search.SearchIssues(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("secret"),
		"issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("anonymous saw %d issues for 'secret', want 0 (private leak)", len(got))
	}
}

func TestSearchIssues_StateFilter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)

	// Open a second issue and close it.
	idep := issues.Deps{Pool: f.deps.Pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	closed, _ := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: f.pubRepo.ID, AuthorUserID: f.alice.ID,
		Title: "closed bug", Body: "fixed",
	})
	if err := issues.SetState(ctx, idep, f.alice.ID, closed.ID, "closed", "completed"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	openHits, _, _ := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("is:open bug"), "", 20, 0)
	for _, h := range openHits {
		if h.State != "open" {
			t.Errorf("is:open: got state=%s", h.State)
		}
	}
	closedHits, _, _ := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("is:closed bug"), "", 20, 0)
	for _, h := range closedHits {
		if h.State != "closed" {
			t.Errorf("is:closed: got state=%s", h.State)
		}
	}
}

// TestSearchIssues_AssigneeFilter is the E10 regression: the CLI
// `shithub status` view feeds `assignee:<me>` into /api/v1/search/issues
// and expects assigned-to-me issues back. Before E10 the qualifier
// fell through as free text, so the dashboard was silently empty.
func TestSearchIssues_AssigneeFilter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)

	// The fixture's pubRepo already has one issue authored by alice
	// titled "public bug report". Find it, then assign bob.
	hits, _, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("repo:alice/publicrepo bug"), "issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues seed lookup: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("fixture changed: expected a seeded issue in alice/publicrepo")
	}
	target := hits[0]
	if err := issuesdb.New().AssignUserToIssue(ctx, f.deps.Pool,
		issuesdb.AssignUserToIssueParams{
			IssueID:          target.ID,
			UserID:           f.bob.ID,
			AssignedByUserID: pgtype.Int8{Int64: f.alice.ID, Valid: true},
		}); err != nil {
		t.Fatalf("AssignUserToIssue: %v", err)
	}

	// Bob is now an assignee. Issue is on a public repo so bob can see it.
	bob := policy.UserActor(f.bob.ID, f.bob.Username, false, false)
	got, total, err := search.SearchIssues(ctx, f.deps, bob,
		search.ParseQuery("assignee:bob"), "issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues assignee:bob: %v", err)
	}
	if total == 0 {
		t.Fatalf("assignee:bob returned zero hits — filter dropped (E10 regression)")
	}
	found := false
	for _, h := range got {
		if h.ID == target.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("assignee:bob results miss the assigned issue id=%d", target.ID)
	}

	// Negative: assignee:alice should not return the bob-assigned issue
	// (alice is the author, not an assignee).
	none, _, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("assignee:alice"), "issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues assignee:alice: %v", err)
	}
	for _, h := range none {
		if h.ID == target.ID {
			t.Errorf("assignee:alice leaked id=%d (bob is the only assignee)", target.ID)
		}
	}
}

func TestSearchIssues_RepoFilter(t *testing.T) {
	f := setup(t)
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)
	got, _, err := search.SearchIssues(context.Background(), f.deps, alice,
		search.ParseQuery("repo:alice/publicrepo bug"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	for _, h := range got {
		if h.OwnerUsername != "alice" || h.RepoName != "publicrepo" {
			t.Errorf("repo: filter let through %s/%s", h.OwnerUsername, h.RepoName)
		}
	}
}

func TestSearchIssues_RepoFilterMatchesOrgOwner(t *testing.T) {
	f := setup(t)
	got, _, err := search.SearchIssues(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("repo:tenseleyFlow/shithub bug"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected org repo issue results")
	}
	for _, h := range got {
		if h.OwnerUsername != "tenseleyflow" || h.RepoName != "shithub" {
			t.Errorf("repo: filter let through %s/%s", h.OwnerUsername, h.RepoName)
		}
	}
}

func TestSearchCode_RepoFilterMatchesOrgOwner(t *testing.T) {
	f := setup(t)
	got, total, err := search.SearchCode(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("repo:tenseleyFlow/shithub README"), 20, 0)
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if total == 0 {
		t.Fatalf("SearchCode total = 0, want org-owned path hit")
	}
	for _, h := range got {
		if h.RepoID == f.orgRepo.ID && h.OwnerUsername == "tenseleyflow" && h.RepoName == "shithub" {
			return
		}
	}
	t.Fatalf("org-owned code hit missing from %d results", len(got))
}

func TestSearchUsers_ExcludesSuspended(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.deps.Pool.Exec(ctx,
		"UPDATE users SET suspended_at = now() WHERE id = $1", f.bob.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	got, _, err := search.SearchUsers(ctx, f.deps, search.ParseQuery("bob"), 20, 0)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	for _, u := range got {
		if u.Username == "bob" {
			t.Errorf("suspended bob in user search results")
		}
	}
}

// TestSearchRepos_EmptyQuery surfaces the typed error so handlers
// can render a friendly empty state rather than a SQL error.
func TestSearchRepos_EmptyQuery(t *testing.T) {
	f := setup(t)
	_, _, err := search.SearchRepos(context.Background(), f.deps,
		policy.AnonymousActor(), search.ParsedQuery{}, 20, 0)
	if !errors.Is(err, search.ErrEmptyQuery) {
		t.Errorf("expected ErrEmptyQuery, got %v", err)
	}
}
