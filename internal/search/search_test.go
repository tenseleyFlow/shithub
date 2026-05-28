// SPDX-License-Identifier: AGPL-3.0-or-later

package search_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

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
		{"language:Go x", search.ParsedQuery{Text: "x", LanguageFilter: "Go"}},
		{"is:public topic:forge", search.ParsedQuery{
			VisibilityFilter: "public",
			TopicFilters:     []string{"forge"},
		}},
	}
	for _, c := range cases {
		got := search.ParseQuery(c.in)
		if got.Text != c.want.Text || got.Phrase != c.want.Phrase ||
			got.StateFilter != c.want.StateFilter || got.KindFilter != c.want.KindFilter ||
			got.MergedStateFilter != c.want.MergedStateFilter ||
			got.AuthorFilter != c.want.AuthorFilter || got.AssigneeFilter != c.want.AssigneeFilter ||
			got.AssigneeAnyFilter != c.want.AssigneeAnyFilter ||
			got.CommenterFilter != c.want.CommenterFilter ||
			got.MentionFilter != c.want.MentionFilter || got.OwnerFilter != c.want.OwnerFilter ||
			got.ReviewRequestedFilter != c.want.ReviewRequestedFilter ||
			got.SortFilter != c.want.SortFilter ||
			got.MilestoneFilter != c.want.MilestoneFilter || got.LanguageFilter != c.want.LanguageFilter ||
			got.VisibilityFilter != c.want.VisibilityFilter ||
			got.PathFilter != c.want.PathFilter || got.ExtensionFilter != c.want.ExtensionFilter ||
			!sameBoolPtr(got.LockedFilter, c.want.LockedFilter) ||
			!reflect.DeepEqual(got.InvolvesFilters, c.want.InvolvesFilters) ||
			!reflect.DeepEqual(got.MissingFilters, c.want.MissingFilters) ||
			!reflect.DeepEqual(got.LabelFilters, c.want.LabelFilters) ||
			!reflect.DeepEqual(got.TopicFilters, c.want.TopicFilters) {
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

func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestParseQuery_IssuePRParityQualifiers(t *testing.T) {
	t.Parallel()
	got := search.ParseQuery("type:pr assignee:* no:label no:milestone no:assignee no:project mentions:@me involves:alice involves:bob is:unmerged is:unlocked")

	if got.KindFilter != "pr" || got.MergedStateFilter != "unmerged" {
		t.Fatalf("kind/merged = %q/%q", got.KindFilter, got.MergedStateFilter)
	}
	if got.LockedFilter == nil || *got.LockedFilter {
		t.Fatalf("LockedFilter = %+v, want false", got.LockedFilter)
	}
	if !got.AssigneeAnyFilter || got.AssigneeFilter != "" {
		t.Fatalf("assignee filters = any:%v value:%q", got.AssigneeAnyFilter, got.AssigneeFilter)
	}
	if got.MentionFilter != "@me" {
		t.Fatalf("MentionFilter = %q", got.MentionFilter)
	}
	if !reflect.DeepEqual(got.InvolvesFilters, []string{"alice", "bob"}) {
		t.Fatalf("InvolvesFilters = %#v", got.InvolvesFilters)
	}
	if !reflect.DeepEqual(got.MissingFilters, []string{"label", "milestone", "assignee", "project"}) {
		t.Fatalf("MissingFilters = %#v", got.MissingFilters)
	}

	got = search.ParseQuery("type:pull-request is:merged is:locked")
	if got.KindFilter != "pr" || got.MergedStateFilter != "merged" {
		t.Fatalf("pull-request/merged = %q/%q", got.KindFilter, got.MergedStateFilter)
	}
	if got.LockedFilter == nil || !*got.LockedFilter {
		t.Fatalf("LockedFilter = %+v, want true", got.LockedFilter)
	}

	got = search.ParseQuery("type:discussion no:review")
	if got.Text != "type:discussion no:review" || got.HasContent() != true {
		t.Fatalf("unknown issue qualifiers should remain free text: %+v", got)
	}
}

func TestParseQuery_SortAndReviewRequestedQualifiers(t *testing.T) {
	t.Parallel()
	got := search.ParseQuery("type:pr review-requested:@me sort:comments updated docs")
	if got.KindFilter != "pr" {
		t.Fatalf("KindFilter = %q, want pr", got.KindFilter)
	}
	if got.ReviewRequestedFilter != "@me" {
		t.Fatalf("ReviewRequestedFilter = %q", got.ReviewRequestedFilter)
	}
	if got.SortFilter != "comments-desc" {
		t.Fatalf("SortFilter = %q, want comments-desc", got.SortFilter)
	}
	if got.Text != "updated docs" {
		t.Fatalf("Text = %q, want updated docs", got.Text)
	}

	got = search.ParseQuery("sort:created-asc sort:updated-desc")
	if got.SortFilter != "updated-desc" {
		t.Fatalf("last sort should win; got %q", got.SortFilter)
	}

	got = search.ParseQuery("sort:unsupported bug")
	if got.Text != "sort:unsupported bug" || got.SortFilter != "" {
		t.Fatalf("unsupported sort should remain free text: %+v", got)
	}
}

func TestParseQuery_RepositoryQualifiers(t *testing.T) {
	t.Parallel()
	got := search.ParseQuery("is:private fork:false archived:false topic:cli")
	if got.VisibilityFilter != "private" {
		t.Fatalf("VisibilityFilter = %q", got.VisibilityFilter)
	}
	if got.ForkFilter == nil || *got.ForkFilter {
		t.Fatalf("ForkFilter = %+v, want false", got.ForkFilter)
	}
	if got.ArchivedFilter == nil || *got.ArchivedFilter {
		t.Fatalf("ArchivedFilter = %+v, want false", got.ArchivedFilter)
	}
	if !reflect.DeepEqual(got.TopicFilters, []string{"cli"}) {
		t.Fatalf("TopicFilters = %#v", got.TopicFilters)
	}

	got = search.ParseQuery("is:fork is:archived visibility:public fork:only")
	if got.VisibilityFilter != "public" {
		t.Fatalf("VisibilityFilter = %q", got.VisibilityFilter)
	}
	if got.ForkFilter == nil || !*got.ForkFilter {
		t.Fatalf("ForkFilter = %+v, want true", got.ForkFilter)
	}
	if got.ArchivedFilter == nil || !*got.ArchivedFilter {
		t.Fatalf("ArchivedFilter = %+v, want true", got.ArchivedFilter)
	}
}

func TestParseQuery_AdvancedQualifiers(t *testing.T) {
	t.Parallel()
	got := search.ParseQuery(`repo:tenseleyFlow/shithub is:pr label:"good first issue" ` +
		`milestone:v1 author:esp path:internal/web extension:.go language:Go ` +
		`commenter:mfwolffe created:2026-05-01..2026-05-19 updated:>=2026-05-10 ` +
		`-draft -"old phrase"`)

	if got.RepoFilter == nil || got.RepoFilter.Owner != "tenseleyFlow" || got.RepoFilter.Name != "shithub" {
		t.Fatalf("RepoFilter = %+v", got.RepoFilter)
	}
	if got.KindFilter != "pr" || got.AuthorFilter != "esp" || got.CommenterFilter != "mfwolffe" {
		t.Fatalf("kind/author/commenter = %q/%q/%q", got.KindFilter, got.AuthorFilter, got.CommenterFilter)
	}
	if !reflect.DeepEqual(got.LabelFilters, []string{"good first issue"}) || got.MilestoneFilter != "v1" {
		t.Fatalf("labels/milestone = %#v/%q", got.LabelFilters, got.MilestoneFilter)
	}
	if got.PathFilter != "internal/web" || got.ExtensionFilter != "go" || got.LanguageFilter != "Go" {
		t.Fatalf("path/extension/language = %q/%q/%q", got.PathFilter, got.ExtensionFilter, got.LanguageFilter)
	}
	assertDateRange(t, got.CreatedFilter, dateUTC(2026, 5, 1), dateUTC(2026, 5, 20))
	assertDateRange(t, got.UpdatedFilter, dateUTC(2026, 5, 10), time.Time{})
	if len(got.ExcludedTerms) != 2 || got.ExcludedTerms[1].Value != "old phrase" || !got.ExcludedTerms[1].Phrase {
		t.Fatalf("ExcludedTerms = %#v", got.ExcludedTerms)
	}
	if len(got.Qualifiers) != 11 {
		t.Fatalf("Qualifiers len = %d, want 11 (%#v)", len(got.Qualifiers), got.Qualifiers)
	}
}

func assertDateRange(t *testing.T, got *search.DateRange, from, to time.Time) {
	t.Helper()
	if got == nil {
		t.Fatal("DateRange is nil")
	}
	if !from.IsZero() {
		if !got.HasFrom || !got.From.Equal(from) {
			t.Fatalf("from = %v/%v, want %v", got.From, got.HasFrom, from)
		}
	}
	if from.IsZero() && got.HasFrom {
		t.Fatalf("unexpected from = %v", got.From)
	}
	if !to.IsZero() {
		if !got.HasTo || !got.To.Equal(to) {
			t.Fatalf("to = %v/%v, want %v", got.To, got.HasTo, to)
		}
	}
	if to.IsZero() && got.HasTo {
		t.Fatalf("unexpected to = %v", got.To)
	}
}

func dateUTC(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
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

func TestSearchRepos_LanguageAndDateFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE repos SET primary_language = 'Go', created_at = '2026-05-12T00:00:00Z'
		WHERE id = $1
	`, f.orgRepo.ID); err != nil {
		t.Fatalf("seed org repo language/date: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE repos SET primary_language = 'Python', created_at = '2026-05-12T00:00:00Z'
		WHERE id = $1
	`, f.pubRepo.ID); err != nil {
		t.Fatalf("seed pub repo language/date: %v", err)
	}

	got, total, err := search.SearchRepos(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("language:Go created:2026-05-12"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos language/date: %v", err)
	}
	if total == 0 {
		t.Fatalf("language/date returned zero hits")
	}
	for _, r := range got {
		if r.ID != f.orgRepo.ID {
			t.Errorf("language/date filter leaked %s/%s", r.OwnerUsername, r.Name)
		}
	}
}

func TestSearchRepos_RepositoryQualifiers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE repos
		   SET fork_of_repo_id = $1,
		       is_archived = true,
		       archived_at = '2026-05-12T00:00:00Z'
		 WHERE id = $2
	`, f.orgRepo.ID, f.pubRepo.ID); err != nil {
		t.Fatalf("seed fork/archive: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO repo_topics (repo_id, topic) VALUES ($1, 'forge')
	`, f.pubRepo.ID); err != nil {
		t.Fatalf("seed topic: %v", err)
	}

	got, total, err := search.SearchRepos(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("is:fork archived:true topic:forge"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos repository qualifiers: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != f.pubRepo.ID {
		t.Fatalf("repository qualifiers got %d rows total=%d, want pubRepo: %+v", len(got), total, got)
	}

	anonPrivate, total, err := search.SearchRepos(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("is:private"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchRepos is:private anonymous: %v", err)
	}
	if total != 0 || len(anonPrivate) != 0 {
		t.Fatalf("anonymous is:private returned %d rows total=%d", len(anonPrivate), total)
	}

	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)
	ownerPrivate, total, err := search.SearchRepos(ctx, f.deps, alice,
		search.ParseQuery("is:private"), 20, 0)
	if err != nil {
		t.Fatalf("SearchRepos is:private owner: %v", err)
	}
	if total == 0 || len(ownerPrivate) == 0 {
		t.Fatalf("owner is:private returned zero hits")
	}
	for _, repo := range ownerPrivate {
		if repo.Visibility != "private" {
			t.Fatalf("is:private leaked non-private repo %+v", repo)
		}
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

// TestSearchIssues_SoftDeletedRepoExcluded pins I29: after a repo is
// soft-deleted, its issues must not surface in /search/issues even
// for the owner. The audit observed ghost rows on `shithub status`
// (which fans out via assignee:<me>). The fix is structural — the
// policy.VisibilityPredicate already ANDs in `r.deleted_at IS NULL`,
// so this test guards against a future regression that drops that
// clause from the predicate.
func TestSearchIssues_SoftDeletedRepoExcluded(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)

	// Assign alice to her own issue on pubRepo, then soft-delete pubRepo.
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
			UserID:           f.alice.ID,
			AssignedByUserID: pgtype.Int8{Int64: f.alice.ID, Valid: true},
		}); err != nil {
		t.Fatalf("AssignUserToIssue: %v", err)
	}

	// Before deletion: assignee:alice must surface this issue.
	pre, _, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("assignee:alice"), "issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues pre-delete: %v", err)
	}
	found := false
	for _, h := range pre {
		if h.ID == target.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pre-delete: expected target issue id=%d in assignee:alice", target.ID)
	}

	// Soft-delete pubRepo.
	if _, err := f.deps.Pool.Exec(ctx,
		`UPDATE repos SET deleted_at = now() WHERE id = $1`, f.pubRepo.ID); err != nil {
		t.Fatalf("soft-delete repo: %v", err)
	}

	// After deletion: the issue must NOT appear, even for the owner.
	got, _, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("assignee:alice"), "issue", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues post-delete: %v", err)
	}
	for _, h := range got {
		if h.ID == target.ID {
			t.Errorf("I29 regression: deleted repo's issue id=%d still appears in assignee:alice", target.ID)
		}
	}
}

func TestSearchIssues_LabelMilestoneCommenterAndOwnerFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	var targetID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		SELECT id FROM issues WHERE repo_id = $1 AND title = 'org public bug report'
	`, f.orgRepo.ID).Scan(&targetID); err != nil {
		t.Fatalf("lookup org issue: %v", err)
	}
	var labelID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		INSERT INTO labels (repo_id, name, color, description)
		VALUES ($1, 'parity', '0969da', 'Parity work')
		RETURNING id
	`, f.orgRepo.ID).Scan(&labelID); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	var milestoneID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		INSERT INTO milestones (repo_id, title, description)
		VALUES ($1, 'v1', 'Version one')
		RETURNING id
	`, f.orgRepo.ID).Scan(&milestoneID); err != nil {
		t.Fatalf("seed milestone: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO issue_labels (issue_id, label_id, applied_by_user_id)
		VALUES ($1, $2, $3)
	`, targetID, labelID, f.alice.ID); err != nil {
		t.Fatalf("seed issue label: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE issues SET milestone_id = $1 WHERE id = $2
	`, milestoneID, targetID); err != nil {
		t.Fatalf("attach milestone: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO issue_comments (issue_id, author_user_id, body)
		VALUES ($1, $2, 'I can reproduce this')
	`, targetID, f.bob.ID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	got, total, err := search.SearchIssues(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("org:tenseleyflow label:parity milestone:v1 commenter:bob is:issue"),
		"", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues structured filters: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != targetID {
		t.Fatalf("structured filters got %d rows total=%d, want issue %d: %+v", len(got), total, targetID, got)
	}
}

func TestSearchIssues_MissingMetadataAndAssigneeAnyFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	var targetID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		SELECT id FROM issues WHERE repo_id = $1 AND title = 'org public bug report'
	`, f.orgRepo.ID).Scan(&targetID); err != nil {
		t.Fatalf("lookup org issue: %v", err)
	}
	var labelID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		INSERT INTO labels (repo_id, name, color, description)
		VALUES ($1, 'triaged', '0969da', 'Triaged')
		RETURNING id
	`, f.orgRepo.ID).Scan(&labelID); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	var milestoneID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		INSERT INTO milestones (repo_id, title, description)
		VALUES ($1, 'v2', 'Version two')
		RETURNING id
	`, f.orgRepo.ID).Scan(&milestoneID); err != nil {
		t.Fatalf("seed milestone: %v", err)
	}
	var projectID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		INSERT INTO repo_projects (repo_id, title, description, created_by_user_id)
		VALUES ($1, 'Roadmap', 'Roadmap project', $2)
		RETURNING id
	`, f.orgRepo.ID, f.alice.ID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO issue_labels (issue_id, label_id, applied_by_user_id) VALUES ($1, $2, $3)
	`, targetID, labelID, f.alice.ID); err != nil {
		t.Fatalf("seed issue label: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE issues SET milestone_id = $1 WHERE id = $2
	`, milestoneID, targetID); err != nil {
		t.Fatalf("seed milestone link: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO issue_assignees (issue_id, user_id, assigned_by_user_id) VALUES ($1, $2, $3)
	`, targetID, f.bob.ID, f.alice.ID); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO repo_project_items (project_id, issue_id, added_by_user_id) VALUES ($1, $2, $3)
	`, projectID, targetID, f.alice.ID); err != nil {
		t.Fatalf("seed project item: %v", err)
	}

	assigned, total, err := search.SearchIssues(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("assignee:* is:issue"),
		"", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues assignee:*: %v", err)
	}
	if total == 0 || !issueResultsContain(assigned, targetID) {
		t.Fatalf("assignee:* missing assigned target %d: total=%d got=%+v", targetID, total, assigned)
	}

	missing, total, err := search.SearchIssues(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("no:label no:milestone no:assignee no:project is:issue"),
		"", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues no:*: %v", err)
	}
	if total == 0 {
		t.Fatalf("no:* returned zero rows; fixture should still have unannotated public issues")
	}
	if issueResultsContain(missing, targetID) {
		t.Fatalf("no:* returned annotated issue %d: %+v", targetID, missing)
	}
}

func TestSearchIssues_MentionsInvolvesAndAtMeFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	var targetID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		SELECT id FROM issues WHERE repo_id = $1 AND title = 'org public bug report'
	`, f.orgRepo.ID).Scan(&targetID); err != nil {
		t.Fatalf("lookup org issue: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE issues SET body = 'please take a look @bob' WHERE id = $1
	`, targetID); err != nil {
		t.Fatalf("seed mention: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO issue_comments (issue_id, author_user_id, body)
		VALUES ($1, $2, 'I can help with this')
	`, targetID, f.bob.ID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	bob := policy.UserActor(f.bob.ID, f.bob.Username, false, false)
	for _, raw := range []string{"mentions:@me", "commenter:@me", "involves:@me"} {
		got, total, err := search.SearchIssues(ctx, f.deps, bob,
			search.ParseQuery(raw), "", 20, 0)
		if err != nil {
			t.Fatalf("SearchIssues %s: %v", raw, err)
		}
		if total == 0 || !issueResultsContain(got, targetID) {
			t.Fatalf("%s missing target %d: total=%d got=%+v", raw, targetID, total, got)
		}
	}

	anonHits, total, err := search.SearchIssues(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("mentions:@me"),
		"", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues anonymous @me: %v", err)
	}
	if total != 0 || len(anonHits) != 0 {
		t.Fatalf("anonymous @me returned %d rows total=%d", len(anonHits), total)
	}
}

func TestSearchIssues_MergedStateFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := policy.UserActor(f.alice.ID, f.alice.Username, false, false)
	idep := issues.Deps{Pool: f.deps.Pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	mergedPR, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: f.pubRepo.ID, AuthorUserID: f.alice.ID,
		Title: "merged branch", Body: "ready to merge", Kind: "pr",
	})
	if err != nil {
		t.Fatalf("Create merged PR issue: %v", err)
	}
	unmergedPR, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID: f.pubRepo.ID, AuthorUserID: f.alice.ID,
		Title: "unmerged branch", Body: "still open", Kind: "pr",
	})
	if err != nil {
		t.Fatalf("Create unmerged PR issue: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO pull_requests (issue_id, base_ref, head_ref, head_repo_id, base_oid, head_oid, merged_at)
		VALUES ($1, 'trunk', 'merged-branch', $3, 'base', 'merged-head', now()),
		       ($2, 'trunk', 'unmerged-branch', $3, 'base', 'unmerged-head', NULL)
	`, mergedPR.ID, unmergedPR.ID, f.pubRepo.ID); err != nil {
		t.Fatalf("seed pull_requests: %v", err)
	}

	merged, total, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("is:merged branch"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues is:merged: %v", err)
	}
	if total == 0 || !issueResultsContain(merged, mergedPR.ID) || issueResultsContain(merged, unmergedPR.ID) {
		t.Fatalf("is:merged got total=%d rows=%+v", total, merged)
	}

	unmerged, total, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("is:unmerged branch"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues is:unmerged: %v", err)
	}
	if total == 0 || !issueResultsContain(unmerged, unmergedPR.ID) || issueResultsContain(unmerged, mergedPR.ID) {
		t.Fatalf("is:unmerged got total=%d rows=%+v", total, unmerged)
	}

	conflict, total, err := search.SearchIssues(ctx, f.deps, alice,
		search.ParseQuery("type:issue is:merged branch"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues conflicting kind/merged: %v", err)
	}
	if total != 0 || len(conflict) != 0 {
		t.Fatalf("type:issue is:merged returned %d rows total=%d", len(conflict), total)
	}
}

func TestSearchIssues_ReviewRequestedFilter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	idep := issues.Deps{Pool: f.deps.Pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	pr, err := issues.Create(ctx, idep, issues.CreateParams{
		RepoID:       f.pubRepo.ID,
		AuthorUserID: f.alice.ID,
		Title:        "review requested branch",
		Body:         "needs review",
		Kind:         "pr",
	})
	if err != nil {
		t.Fatalf("Create PR issue: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO pull_requests (issue_id, base_ref, head_ref, head_repo_id, base_oid, head_oid)
		VALUES ($1, 'trunk', 'review-requested-branch', $2, 'base', 'head')
	`, pr.ID, f.pubRepo.ID); err != nil {
		t.Fatalf("seed pull_request: %v", err)
	}
	if _, err := f.deps.Pool.Exec(ctx, `
		INSERT INTO pr_review_requests (pr_issue_id, requested_user_id, requested_by_user_id)
		VALUES ($1, $2, $3)
	`, pr.ID, f.bob.ID, f.alice.ID); err != nil {
		t.Fatalf("seed review request: %v", err)
	}

	bob := policy.UserActor(f.bob.ID, f.bob.Username, false, false)
	got, total, err := search.SearchIssues(ctx, f.deps, bob,
		search.ParseQuery("review-requested:@me"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues review-requested:@me: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != pr.ID || got[0].Kind != "pr" {
		t.Fatalf("review-requested:@me got %d rows total=%d, want PR %d: %+v", len(got), total, pr.ID, got)
	}

	conflict, total, err := search.SearchIssues(ctx, f.deps, bob,
		search.ParseQuery("is:issue review-requested:@me"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues issue/review-requested conflict: %v", err)
	}
	if total != 0 || len(conflict) != 0 {
		t.Fatalf("is:issue review-requested:@me returned %d rows total=%d", len(conflict), total)
	}

	if _, err := f.deps.Pool.Exec(ctx, `
		UPDATE pr_review_requests SET dismissed_at = now() WHERE pr_issue_id = $1
	`, pr.ID); err != nil {
		t.Fatalf("dismiss review request: %v", err)
	}
	got, total, err = search.SearchIssues(ctx, f.deps, bob,
		search.ParseQuery("review-requested:@me"), "", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues dismissed review-requested:@me: %v", err)
	}
	if total != 0 || len(got) != 0 {
		t.Fatalf("dismissed review request returned %d rows total=%d", len(got), total)
	}
}

func TestSearchIssues_SortByComments(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	var pubID, orgID int64
	if err := f.deps.Pool.QueryRow(ctx, `
		SELECT id FROM issues WHERE repo_id = $1 AND title = 'public bug report'
	`, f.pubRepo.ID).Scan(&pubID); err != nil {
		t.Fatalf("lookup public issue: %v", err)
	}
	if err := f.deps.Pool.QueryRow(ctx, `
		SELECT id FROM issues WHERE repo_id = $1 AND title = 'org public bug report'
	`, f.orgRepo.ID).Scan(&orgID); err != nil {
		t.Fatalf("lookup org issue: %v", err)
	}
	for _, issueID := range []int64{pubID, orgID, orgID, orgID} {
		if _, err := f.deps.Pool.Exec(ctx, `
			INSERT INTO issue_comments (issue_id, author_user_id, body)
			VALUES ($1, $2, 'comment for sorting')
		`, issueID, f.alice.ID); err != nil {
			t.Fatalf("seed comment for issue %d: %v", issueID, err)
		}
	}

	got, total, err := search.SearchIssues(ctx, f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("is:issue sort:comments-desc"),
		"", 20, 0)
	if err != nil {
		t.Fatalf("SearchIssues sort:comments-desc: %v", err)
	}
	if total < 2 || len(got) < 2 {
		t.Fatalf("sort:comments-desc got %d rows total=%d, want public fixture issues", len(got), total)
	}
	if got[0].ID != orgID {
		t.Fatalf("sort:comments-desc first result id=%d, want noisier org issue %d; rows=%+v", got[0].ID, orgID, got)
	}
	for _, result := range got {
		if result.RepoName == "privaterepo" {
			t.Fatalf("sort:comments-desc leaked private issue: %+v", got)
		}
	}
}

func issueResultsContain(results []search.IssueResult, id int64) bool {
	for _, result := range results {
		if result.ID == id {
			return true
		}
	}
	return false
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

func TestSearchCode_PathAndExtensionQualifiers(t *testing.T) {
	f := setup(t)
	got, total, err := search.SearchCode(context.Background(), f.deps,
		policy.AnonymousActor(),
		search.ParseQuery("repo:tenseleyFlow/shithub path:readme extension:md"),
		20, 0)
	if err != nil {
		t.Fatalf("SearchCode path/extension: %v", err)
	}
	if total != 1 || len(got) != 1 {
		t.Fatalf("path/extension got %d rows total=%d, want one README hit", len(got), total)
	}
	if got[0].Path != "README.md" || got[0].OwnerUsername != "tenseleyflow" || got[0].RepoName != "shithub" {
		t.Fatalf("path/extension hit = %+v", got[0])
	}
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
