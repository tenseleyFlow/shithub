// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// apiRuleset mirrors handlers/api.rulesetResponse for decoding.
type apiRuleset struct {
	ID          int64                `json:"id"`
	Name        string               `json:"name"`
	Target      string               `json:"target"`
	SourceType  string               `json:"source_type"`
	Source      string               `json:"source"`
	Enforcement string               `json:"enforcement"`
	Conditions  apiRulesetConditions `json:"conditions"`
	Rules       []apiRulesetRule     `json:"rules"`
}

type apiRulesetConditions struct {
	RefName apiRulesetRefName `json:"ref_name"`
}

type apiRulesetRefName struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type apiRulesetRule struct {
	Type       string         `json:"type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type rulesetsEnv struct {
	pool   *pgxpool.Pool
	router http.Handler
	rfs    *storage.RepoFS
	token  string
	owner  string
	repo   string
}

func newRulesetsEnv(t *testing.T, ownerUsername string) rulesetsEnv {
	t.Helper()
	pool, router, rfs, token, owner, repoName := seedBranchesEnv(t, ownerUsername)
	return rulesetsEnv{pool: pool, router: router, rfs: rfs, token: token, owner: owner, repo: repoName}
}

func (e rulesetsEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	return rr
}

// repoID looks the repo row up via the existing repos REST endpoint;
// keeps the test independent of internal package wiring.
func (e rulesetsEnv) repoID(t *testing.T) int64 {
	t.Helper()
	rr := e.get(t, fmt.Sprintf("/api/v1/repos/%s/%s", e.owner, e.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("repo lookup status %d; body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode repo id: %v", err)
	}
	return got.ID
}

// seedRule upserts a protection rule and (optionally) layers review
// settings on top. Returns the rule ID.
func seedRule(t *testing.T, pool *pgxpool.Pool, p reposdb.UpsertBranchProtectionRuleParams, reviewCount int32, requireCodeOwner bool) int64 {
	t.Helper()
	rq := reposdb.New()
	id, err := rq.UpsertBranchProtectionRule(context.Background(), pool, p)
	if err != nil {
		t.Fatalf("UpsertBranchProtectionRule: %v", err)
	}
	if reviewCount > 0 || requireCodeOwner {
		if err := rq.UpdateBranchProtectionReviewSettings(context.Background(), pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
			ID:                     id,
			RequiredReviewCount:    reviewCount,
			RequireCodeOwnerReview: requireCodeOwner,
		}); err != nil {
			t.Fatalf("UpdateBranchProtectionReviewSettings: %v", err)
		}
	}
	return id
}

func TestRulesets_ListEmpty(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected empty list, got %d", len(listed))
	}
}

func TestRulesets_ListProjectsRule(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	id := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               env.repoID(t),
		Pattern:              "trunk",
		PreventForcePush:     true,
		PreventDeletion:      true,
		RequirePrForPush:     false,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 2, true)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1; payload=%+v", len(listed), listed)
	}
	rs := listed[0]
	if rs.ID != id {
		t.Errorf("id: got %d, want %d", rs.ID, id)
	}
	if rs.Name != "Pattern: trunk" {
		t.Errorf("name: %q", rs.Name)
	}
	if rs.Target != "branch" || rs.SourceType != "Repository" || rs.Enforcement != "active" {
		t.Errorf("envelope fields: %+v", rs)
	}
	wantSource := strings.ToLower(env.owner) + "/" + env.repo
	if rs.Source != wantSource {
		t.Errorf("source: got %q, want %q", rs.Source, wantSource)
	}
	if len(rs.Conditions.RefName.Include) != 1 || rs.Conditions.RefName.Include[0] != "refs/heads/trunk" {
		t.Errorf("conditions.ref_name.include: %+v", rs.Conditions.RefName.Include)
	}

	have := map[string]apiRulesetRule{}
	for _, r := range rs.Rules {
		have[r.Type] = r
	}
	if _, ok := have["non_fast_forward"]; !ok {
		t.Errorf("missing non_fast_forward rule; rules=%+v", rs.Rules)
	}
	if _, ok := have["deletion"]; !ok {
		t.Errorf("missing deletion rule; rules=%+v", rs.Rules)
	}
	pr, ok := have["pull_request"]
	if !ok {
		t.Fatalf("missing pull_request rule; rules=%+v", rs.Rules)
	}
	if v, _ := pr.Parameters["required_approving_review_count"].(float64); int(v) != 2 {
		t.Errorf("pull_request.required_approving_review_count: %v", pr.Parameters["required_approving_review_count"])
	}
	if v, _ := pr.Parameters["require_code_owner_review"].(bool); !v {
		t.Errorf("pull_request.require_code_owner_review: %v", pr.Parameters["require_code_owner_review"])
	}
}

func TestRulesets_ListProjectsTagRule(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	id := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               env.repoID(t),
		Pattern:              "v*",
		Target:               "tag",
		PreventForcePush:     true,
		PreventDeletion:      true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1; payload=%+v", len(listed), listed)
	}
	rs := listed[0]
	if rs.ID != id || rs.Target != "tag" {
		t.Fatalf("tag ruleset shape: %+v want id=%d target=tag", rs, id)
	}
	if len(rs.Conditions.RefName.Include) != 1 || rs.Conditions.RefName.Include[0] != "refs/tags/v*" {
		t.Fatalf("conditions.ref_name.include=%+v want refs/tags/v*", rs.Conditions.RefName.Include)
	}
	have := map[string]bool{}
	for _, r := range rs.Rules {
		have[r.Type] = true
	}
	if !have["non_fast_forward"] || !have["deletion"] {
		t.Fatalf("tag rules should expose movement/deletion protection; rules=%+v", rs.Rules)
	}
	if have["pull_request"] || have["required_status_checks"] {
		t.Fatalf("tag rules must not expose branch-only PR/check rules; rules=%+v", rs.Rules)
	}
}

func TestRulesets_GetSingle(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	id := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: env.repoID(t), Pattern: "release/*",
		PreventForcePush:     true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets/%d", env.owner, env.repo, id))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != id || got.Name != "Pattern: release/*" {
		t.Errorf("shape: %+v", got)
	}
}

func TestRulesets_GetUnknownReturns404(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets/9999", env.owner, env.repo))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRulesets_GetCrossRepoLeak404(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	// Spin up a second repo for the same owner and put a rule on it.
	// Hitting the first repo's URL with the second repo's rule id
	// must 404 — same status as "doesn't exist" to keep existence
	// non-discoverable across repo boundaries.
	otherID := seedSecondRepoForOwner(t, env, "demo2")
	id := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: otherID, Pattern: "trunk",
		PreventDeletion:      true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets/%d", env.owner, env.repo, id))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-repo status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRulesets_RulesForBranchListsAllMatches(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	repoID := env.repoID(t)
	idWild := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: repoID, Pattern: "release/*",
		PreventForcePush:     true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)
	idExact := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: repoID, Pattern: "release/v1.0",
		PreventDeletion:      true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rules/branches/release/v1.0", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len: got %d, want 2 (wildcard + exact); payload=%+v", len(listed), listed)
	}
	seen := map[int64]bool{}
	for _, rs := range listed {
		seen[rs.ID] = true
	}
	if !seen[idWild] || !seen[idExact] {
		t.Errorf("missing expected rule id; seen=%v", seen)
	}
}

func TestRulesets_RulesForBranchNoMatch(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID: env.repoID(t), Pattern: "release/*",
		PreventForcePush:     true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rules/branches/feature/x", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected no rules; got %+v", listed)
	}
}

func TestRulesets_RulesForTagListsTagMatchesOnly(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	repoID := env.repoID(t)
	tagID := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               repoID,
		Pattern:              "v*",
		Target:               "tag",
		PreventForcePush:     true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)
	branchID := seedRule(t, env.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               repoID,
		Pattern:              "v*",
		Target:               "branch",
		PreventDeletion:      true,
		AllowedPusherUserIds: []int64{},
		CreatedByUserID:      pgtype.Int8{Valid: false},
	}, 0, false)

	rr := env.get(t, fmt.Sprintf("/api/v1/repos/%s/%s/rules/tags/v1.0.0", env.owner, env.repo))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiRuleset
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1; payload=%+v", len(listed), listed)
	}
	if listed[0].ID != tagID {
		t.Fatalf("matched id=%d want tag rule %d; branch rule was %d", listed[0].ID, tagID, branchID)
	}
}

func TestRulesets_RequiresReadScope(t *testing.T) {
	env := newRulesetsEnv(t, "alice")
	// Mint a user:read-only token for a different actor; rulesets
	// list requires repo:read so this must 403.
	otherID := seedRepoCreatorUser(t, env.pool, "carol")
	wrongScope := mintRunnerAPIPAT(t, env.pool, otherID, string(pat.ScopeUserRead))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/rulesets", env.owner, env.repo), nil)
	req.Header.Set("Authorization", "Bearer "+wrongScope)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// seedSecondRepoForOwner creates a second repo under the existing
// owner; returns the new repo's id. Used by the cross-repo leak
// test to confirm rulesets/{id} doesn't disclose rules belonging
// to a different repo under the same owner.
func seedSecondRepoForOwner(t *testing.T, env rulesetsEnv, name string) int64 {
	t.Helper()
	// Look up the existing owner — seedRepoCreatorUser is NOT
	// idempotent (it inserts) so we resolve by username instead.
	user, err := usersdb.New().GetUserByUsername(context.Background(), env.pool, env.owner)
	if err != nil {
		t.Fatalf("GetUserByUsername %q: %v", env.owner, err)
	}
	creatorID := user.ID
	row, err := repos.Create(context.Background(), repos.Deps{
		Pool:    env.pool,
		RepoFS:  env.rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   creatorID,
		OwnerUserID:   creatorID,
		OwnerUsername: env.owner,
		Name:          name,
		Description:   "secondary repo",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("repos.Create %q: %v", name, err)
	}
	return row.Repo.ID
}
