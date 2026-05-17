// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountRulesets registers the S50 §9 rulesets REST surface. Three
// read-only endpoints synthesizing gh's modern rulesets shape from
// our existing `branch_protection_rules` rows. One row → one
// ruleset (named after its pattern); rules are emitted as a typed
// array (`pull_request`, `non_fast_forward`, `deletion`,
// `required_signatures`, `required_status_checks`).
//
//	GET /api/v1/repos/{o}/{r}/rulesets
//	GET /api/v1/repos/{o}/{r}/rulesets/{id}
//	GET /api/v1/repos/{o}/{r}/rules/branches/{branch}  rules applying to a branch
//	GET /api/v1/repos/{o}/{r}/rules/tags/{tag}          rules applying to a tag
//
// All endpoints require `repo:read` and gate on `ActionRepoRead`.
// Mirrors gh's response shape — clients pinned to gh's documented
// rulesets surface work without per-field shims.
func (h *Handlers) mountRulesets(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/rulesets", h.rulesetsList)
		r.Get("/api/v1/repos/{owner}/{repo}/rulesets/{id}", h.rulesetGet)
		// Branches can contain `/`; wildcard segment, same as the
		// branches single-get route in branches.go.
		r.Get("/api/v1/repos/{owner}/{repo}/rules/branches/*", h.rulesForBranch)
		r.Get("/api/v1/repos/{owner}/{repo}/rules/tags/*", h.rulesForTag)
	})
}

// rulesetResponse mirrors gh's `Ruleset` shape. The fields gh emits
// but we don't synthesize (linked rule actors, multi-actor bypass)
// stay absent; clients that key on those should treat missing as
// "feature not configured."
type rulesetResponse struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Target      string            `json:"target"`
	SourceType  string            `json:"source_type"`
	Source      string            `json:"source"`
	Enforcement string            `json:"enforcement"`
	Conditions  rulesetConditions `json:"conditions"`
	Rules       []rulesetRule     `json:"rules"`
	CreatedAt   string            `json:"created_at,omitempty"`
	UpdatedAt   string            `json:"updated_at,omitempty"`
}

type rulesetConditions struct {
	RefName rulesetRefName `json:"ref_name"`
}

type rulesetRefName struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// rulesetRule mirrors gh's `RepositoryRule` discriminated-union. The
// `parameters` payload depends on `type`; we emit it as an arbitrary
// map so each rule type can carry its own shape without forcing a
// fat struct.
type rulesetRule struct {
	Type       string         `json:"type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// presentRuleset projects one `BranchProtectionRule` row to gh's
// ruleset shape. The owner/repo pair is needed for the `source`
// field; the caller resolves them once and threads through.
func presentRuleset(rule reposdb.BranchProtectionRule, ownerRepo string) rulesetResponse {
	target := rulesetTarget(rule)
	refPrefix := "refs/heads/"
	if target == "tag" {
		refPrefix = "refs/tags/"
	}
	out := rulesetResponse{
		ID:          rule.ID,
		Name:        "Pattern: " + rule.Pattern,
		Target:      target,
		SourceType:  "Repository",
		Source:      ownerRepo,
		Enforcement: "active",
		Conditions: rulesetConditions{
			RefName: rulesetRefName{
				Include: []string{refPrefix + rule.Pattern},
				Exclude: []string{},
			},
		},
		Rules: buildRulesetRules(rule),
	}
	if rule.CreatedAt.Valid {
		out.CreatedAt = rule.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	if rule.UpdatedAt.Valid {
		out.UpdatedAt = rule.UpdatedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}

// buildRulesetRules emits the typed rules array. Each protection
// column maps to one gh rule type; empty / unset columns are
// skipped so clients see only the rules an admin actually
// configured.
func buildRulesetRules(rule reposdb.BranchProtectionRule) []rulesetRule {
	out := make([]rulesetRule, 0, 5)
	if rulesetTarget(rule) == "branch" &&
		(rule.RequirePrForPush || rule.RequiredReviewCount > 0 || rule.RequireCodeOwnerReview || rule.DismissStaleReviewsOnPush) {
		out = append(out, rulesetRule{
			Type: "pull_request",
			Parameters: map[string]any{
				"required_approving_review_count": rule.RequiredReviewCount,
				"dismiss_stale_reviews_on_push":   rule.DismissStaleReviewsOnPush,
				"require_code_owner_review":       rule.RequireCodeOwnerReview,
			},
		})
	}
	if rule.PreventForcePush {
		out = append(out, rulesetRule{Type: "non_fast_forward"})
	}
	if rule.PreventDeletion {
		out = append(out, rulesetRule{Type: "deletion"})
	}
	if rule.RequireSignedCommits {
		out = append(out, rulesetRule{Type: "required_signatures"})
	}
	if rulesetTarget(rule) == "branch" && (len(rule.StatusChecksRequired) > 0 || rule.DismissStaleStatusChecksOnPush) {
		// gh's payload uses an array of `{context, integration_id}`
		// objects; we don't track integrations, so emit `{context}`
		// only. Clients that key on `integration_id` will see absent
		// which gh allows for unscoped contexts.
		checks := make([]map[string]any, 0, len(rule.StatusChecksRequired))
		for _, c := range rule.StatusChecksRequired {
			checks = append(checks, map[string]any{"context": c})
		}
		out = append(out, rulesetRule{
			Type: "required_status_checks",
			Parameters: map[string]any{
				"required_status_checks":               checks,
				"strict_required_status_checks_policy": rule.DismissStaleStatusChecksOnPush,
			},
		})
	}
	return out
}

func rulesetTarget(rule reposdb.BranchProtectionRule) string {
	if rule.Target == "tag" {
		return "tag"
	}
	return "branch"
}

// sourceFor returns `<owner>/<repo>` for the ruleset `source`
// field. Pulled from the chi path params — the routing layer
// already used them for the repo lookup, no extra DB hit needed.
func sourceFor(r *http.Request, repo *reposdb.Repo) string {
	owner := strings.ToLower(chi.URLParam(r, "owner"))
	if owner == "" {
		return repo.Name
	}
	return owner + "/" + repo.Name
}

func (h *Handlers) rulesetsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rules, err := reposdb.New().ListBranchProtectionRules(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list rulesets", "error", err, "repo_id", repo.ID)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	source := sourceFor(r, repo)
	out := make([]rulesetResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, presentRuleset(rule, source))
	}
	// Stable ordering by id so clients see a deterministic list;
	// the underlying query orders too, but the defensive sort
	// makes the contract independent of the query's ORDER BY.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) rulesetGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "ruleset not found")
		return
	}
	rule, err := reposdb.New().GetBranchProtectionRule(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "ruleset not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get ruleset", "error", err, "id", id)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	// Cross-repo lookup safety: 404 if the ruleset belongs to a
	// different repo. Same status as "doesn't exist" so the
	// response doesn't leak existence across repo boundaries.
	if rule.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "ruleset not found")
		return
	}
	writeJSON(w, http.StatusOK, presentRuleset(rule, sourceFor(r, repo)))
}

func (h *Handlers) rulesForBranch(w http.ResponseWriter, r *http.Request) {
	h.rulesForRef(w, r, "branch", chi.URLParam(r, "*"))
}

func (h *Handlers) rulesForTag(w http.ResponseWriter, r *http.Request) {
	h.rulesForRef(w, r, "tag", chi.URLParam(r, "*"))
}

func (h *Handlers) rulesForRef(w http.ResponseWriter, r *http.Request, target, name string) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	if name == "" {
		writeAPIError(w, http.StatusNotFound, target+" not specified")
		return
	}
	rules, err := reposdb.New().ListBranchProtectionRules(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list rulesets for ref", "error", err, "repo_id", repo.ID, "target", target)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	source := sourceFor(r, repo)
	// Return EVERY matching rule, not just the longest-match. gh's
	// /rules/branches/{branch} endpoint lists every applicable rule;
	// the longest-match heuristic our pre-receive enforcer uses is
	// an internal precedence detail, not a contract surface.
	out := make([]rulesetResponse, 0)
	for _, rule := range rules {
		if rulesetTarget(rule) != target {
			continue
		}
		match, mErr := filepath.Match(rule.Pattern, name)
		if mErr != nil || !match {
			continue
		}
		out = append(out, presentRuleset(rule, source))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, out)
}
