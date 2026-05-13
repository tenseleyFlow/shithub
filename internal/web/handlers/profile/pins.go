// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// profilePinLimit is the org-side cap; PRO01 keeps orgs at gh's
// visible 6. User-side caps are resolved per-principal via
// entitlements (Free=6, Pro=100).
const profilePinLimit = 6

var (
	errInvalidPinnedRepo = errors.New("invalid pinned repository")
	errTooManyPins       = errors.New("too many pinned repositories")
)

type profilePinCandidate struct {
	ID                   int64
	OwnerSlug            string
	Name                 string
	Description          string
	Visibility           string
	PrimaryLanguage      string
	PrimaryLanguageColor template.CSS
	StarCount            int64
	ForkCount            int64
	UpdatedAt            pgtype.Timestamptz
	IsPinned             bool
	PinPosition          int
}

func (h *Handlers) pinsUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawName := chi.URLParam(r, "username")
	lower := strings.ToLower(rawName)
	if authpkg.IsReserved(lower) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	viewer := middleware.CurrentUserFromContext(ctx)
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}

	if p, err := orgs.Resolve(ctx, h.d.Pool, lower); err == nil && p.Kind == orgs.PrincipalOrg {
		h.updateOrgPins(w, r, p.ID, viewer)
		return
	}
	h.updateUserPins(w, r, rawName, viewer)
}

func (h *Handlers) updateOrgPins(w http.ResponseWriter, r *http.Request, orgID int64, viewer middleware.CurrentUser) {
	ctx := r.Context()
	org, err := orgsdb.New().GetOrgByID(ctx, h.d.Pool, orgID)
	if err != nil || org.DeletedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	owner, err := orgs.IsOwner(ctx, orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, org.ID, viewer.ID)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "profile pins: org owner check", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !owner {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}

	candidates := h.publicOrgPinCandidates(ctx, org.ID, string(org.Slug))
	repoIDs, err := selectedProfilePinIDs(r.PostForm["repo_id"], candidates, profilePinLimit)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if err := h.saveOrgPins(ctx, org.ID, repoIDs); err != nil {
		h.d.Logger.ErrorContext(ctx, "profile pins: save org pins", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/"+url.PathEscape(string(org.Slug))+"#pinned", http.StatusSeeOther)
}

func (h *Handlers) updateUserPins(w http.ResponseWriter, r *http.Request, rawName string, viewer middleware.CurrentUser) {
	ctx := r.Context()
	user, err := h.q.GetUserByUsername(ctx, h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
			return
		}
		h.d.Logger.ErrorContext(ctx, "profile pins: user lookup", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusGone, "")
		return
	}
	if viewer.ID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}

	candidates := h.publicUserPinCandidates(ctx, user.ID)
	maxPins, beyondFreeAllowed := h.userProfilePinCap(ctx, user.ID)
	repoIDs, err := selectedProfilePinIDs(r.PostForm["repo_id"], candidates, maxPins)
	if errors.Is(err, errTooManyPins) {
		h.handleUserPinOverflow(w, r, user.ID, beyondFreeAllowed)
		return
	}
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if err := h.saveUserPins(ctx, user.ID, repoIDs); err != nil {
		h.d.Logger.ErrorContext(ctx, "profile pins: save user pins", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/"+url.PathEscape(user.Username)+"#pinned", http.StatusSeeOther)
}

// userProfilePinCap resolves the entitled pin cap for a user. Returns
// (cap, beyondFreeAllowed). Falls back to the Free cap if entitlement
// resolution errors so a degraded billing path can't silently raise
// the limit. beyondFreeAllowed mirrors CanUse(FeatureProfilePinsBeyondFree)
// — the caller uses it to flavor the upgrade-banner copy and tells
// the report-only logger whether the deny would have triggered.
func (h *Handlers) userProfilePinCap(ctx context.Context, userID int64) (int64, bool) {
	set, err := entitlements.ForPrincipal(ctx, entitlements.Deps{Pool: h.d.Pool}, billing.PrincipalForUser(userID))
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: load entitlements", "user_id", userID, "error", err)
		return entitlements.FreeProfilePinsCap, false
	}
	decision := set.CanUse(entitlements.FeatureProfilePinsBeyondFree)
	if decision.Allowed {
		return entitlements.ProProfilePinsCap, true
	}
	return entitlements.FreeProfilePinsCap, false
}

// handleUserPinOverflow renders the over-cap response for a personal
// profile pin submission. When the operator hasn't flipped the enforce
// flag (PRO05 report-only default), logs the would-deny and falls back
// to the pre-PRO07 400 response. With enforce on, returns 402 + an
// upgrade banner pointing at /settings/billing.
func (h *Handlers) handleUserPinOverflow(w http.ResponseWriter, r *http.Request, userID int64, beyondFreeAllowed bool) {
	// Pro users that hit this branch are over the Pro cap (100) — that's
	// a DB-sanity 400 regardless of enforcement.
	if beyondFreeAllowed {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if !h.d.BillingEnforce.UserProfilePinsBeyondFree {
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureProfilePinsBeyondFree),
			"reason", string(entitlements.ReasonUpgradeRequired),
			"required_plan", "pro")
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	decision := entitlements.Decision{
		Feature:      entitlements.FeatureProfilePinsBeyondFree,
		RequiredPlan: billing.Plan("pro"),
		Reason:       entitlements.ReasonUpgradeRequired,
	}
	banner := decision.PrincipalUpgradeBanner("Pinned repositories", billing.PrincipalForUser(userID), "")
	http.Error(w, banner.Message, banner.StatusCode)
}

func (h *Handlers) orgPinData(ctx context.Context, orgID int64, orgSlug string, repos []orgProfileRepo) ([]orgProfileRepo, []profilePinCandidate) {
	publicRepos := publicOrgProfileRepos(repos)
	pinned := pinnedOrgRepos(publicRepos)

	setID, explicit, err := h.lookupOrgPinSet(ctx, orgID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: lookup org pin set", "org_id", orgID, "error", err)
	} else if explicit {
		pinned = h.savedOrgPins(ctx, setID, publicRepos)
	}

	candidates := profilePinCandidatesFromOrgRepos(orgSlug, publicRepos)
	markPinnedCandidates(candidates, orgProfileRepoIDs(pinned))
	return pinned, candidates
}

func (h *Handlers) userPinData(ctx context.Context, user usersdb.User) ([]profilePinCandidate, []profilePinCandidate) {
	candidates := h.publicUserPinCandidates(ctx, user.ID)
	var selectedIDs []int64

	setID, explicit, err := h.lookupUserPinSet(ctx, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: lookup user pin set", "user_id", user.ID, "error", err)
	} else if explicit {
		selectedIDs = h.savedPinIDs(ctx, setID)
	}

	pinned := selectedPinCandidates(candidates, selectedIDs)
	markPinnedCandidates(candidates, selectedIDs)
	return pinned, candidates
}

func (h *Handlers) lookupOrgPinSet(ctx context.Context, orgID int64) (int64, bool, error) {
	setID, err := reposdb.New().GetProfilePinSetForOrg(ctx, h.d.Pool, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return setID, true, nil
}

func (h *Handlers) lookupUserPinSet(ctx context.Context, userID int64) (int64, bool, error) {
	setID, err := reposdb.New().GetProfilePinSetForUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return setID, true, nil
}

func (h *Handlers) savedPinIDs(ctx context.Context, setID int64) []int64 {
	rows, err := reposdb.New().ListProfilePinsForSet(ctx, h.d.Pool, setID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: list pins", "set_id", setID, "error", err)
		return nil
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.RepoID)
	}
	return out
}

func (h *Handlers) savedOrgPins(ctx context.Context, setID int64, repos []orgProfileRepo) []orgProfileRepo {
	return selectedOrgProfileRepos(repos, h.savedPinIDs(ctx, setID))
}

func (h *Handlers) publicUserPinCandidates(ctx context.Context, userID int64) []profilePinCandidate {
	rows, err := reposdb.New().ListProfilePinCandidateReposForUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: list user repos", "user_id", userID, "error", err)
		return nil
	}
	out := make([]profilePinCandidate, 0, len(rows))
	for _, row := range rows {
		if !policy.NewRepoRefFromRepo(row.Repo).IsPublic() {
			continue
		}
		out = append(out, profilePinCandidateFromRepo(row.OwnerSlug, row.Repo))
	}
	sortPinCandidates(out)
	return out
}

func (h *Handlers) publicOrgPinCandidates(ctx context.Context, orgID int64, orgSlug string) []profilePinCandidate {
	rows, err := reposdb.New().ListReposForOwnerOrg(ctx, h.d.Pool, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: list org repos", "org_id", orgID, "error", err)
		return nil
	}
	out := make([]profilePinCandidate, 0, len(rows))
	for _, row := range rows {
		if !policy.NewRepoRefFromRepo(row).IsPublic() {
			continue
		}
		out = append(out, profilePinCandidateFromRepo(orgSlug, row))
	}
	sortPinCandidates(out)
	return out
}

func selectedProfilePinIDs(values []string, candidates []profilePinCandidate, maxPins int64) ([]int64, error) {
	allowed := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate.ID] = struct{}{}
	}
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		repoID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || repoID <= 0 {
			return nil, errInvalidPinnedRepo
		}
		if _, ok := allowed[repoID]; !ok {
			return nil, errInvalidPinnedRepo
		}
		if _, ok := seen[repoID]; ok {
			continue
		}
		seen[repoID] = struct{}{}
		out = append(out, repoID)
	}
	if int64(len(out)) > maxPins {
		return nil, errTooManyPins
	}
	return out, nil
}

func (h *Handlers) saveUserPins(ctx context.Context, userID int64, repoIDs []int64) error {
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := reposdb.New()
	setID, err := q.UpsertProfilePinSetForUser(ctx, tx, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return err
	}
	if err := replaceProfilePins(ctx, q, tx, setID, repoIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (h *Handlers) saveOrgPins(ctx context.Context, orgID int64, repoIDs []int64) error {
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := reposdb.New()
	setID, err := q.UpsertProfilePinSetForOrg(ctx, tx, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		return err
	}
	if err := replaceProfilePins(ctx, q, tx, setID, repoIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func replaceProfilePins(ctx context.Context, q *reposdb.Queries, tx pgx.Tx, setID int64, repoIDs []int64) error {
	if err := q.DeleteProfilePinsForSet(ctx, tx, setID); err != nil {
		return err
	}
	for i, repoID := range repoIDs {
		if err := q.InsertProfilePin(ctx, tx, reposdb.InsertProfilePinParams{
			SetID:    setID,
			RepoID:   repoID,
			Position: int32(i + 1),
		}); err != nil {
			return err
		}
	}
	return nil
}

func profilePinCandidatesFromOrgRepos(ownerSlug string, repos []orgProfileRepo) []profilePinCandidate {
	out := make([]profilePinCandidate, 0, len(repos))
	for _, repo := range repos {
		out = append(out, profilePinCandidate{
			ID:                   repo.ID,
			OwnerSlug:            ownerSlug,
			Name:                 repo.Name,
			Description:          repo.Description,
			Visibility:           repo.Visibility,
			PrimaryLanguage:      repo.PrimaryLanguage,
			PrimaryLanguageColor: repo.PrimaryLanguageColor,
			StarCount:            repo.StarCount,
			ForkCount:            repo.ForkCount,
		})
	}
	sortPinCandidates(out)
	return out
}

func profilePinCandidateFromRepo(ownerSlug string, repo reposdb.Repo) profilePinCandidate {
	language := pgTextStringOrEmpty(repo.PrimaryLanguage)
	return profilePinCandidate{
		ID:                   repo.ID,
		OwnerSlug:            ownerSlug,
		Name:                 repo.Name,
		Description:          repo.Description,
		Visibility:           string(repo.Visibility),
		PrimaryLanguage:      language,
		PrimaryLanguageColor: template.CSS(orgLanguageColor(language)), //nolint:gosec // CSS value comes from server-side constants.
		StarCount:            repo.StarCount,
		ForkCount:            repo.ForkCount,
		UpdatedAt:            repo.UpdatedAt,
	}
}

func publicOrgProfileRepos(repos []orgProfileRepo) []orgProfileRepo {
	out := make([]orgProfileRepo, 0, len(repos))
	for _, repo := range repos {
		if orgProfileRepoRef(repo).IsPublic() {
			out = append(out, repo)
		}
	}
	return out
}

func orgProfileRepoRef(repo orgProfileRepo) policy.RepoRef {
	return policy.RepoRef{
		ID:         repo.ID,
		Visibility: repo.Visibility,
		IsArchived: repo.IsArchived,
	}
}

func orgProfileRepoIDs(repos []orgProfileRepo) []int64 {
	out := make([]int64, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repo.ID)
	}
	return out
}

func selectedOrgProfileRepos(repos []orgProfileRepo, repoIDs []int64) []orgProfileRepo {
	byID := make(map[int64]orgProfileRepo, len(repos))
	for _, repo := range repos {
		byID[repo.ID] = repo
	}
	out := make([]orgProfileRepo, 0, len(repoIDs))
	for _, repoID := range repoIDs {
		if repo, ok := byID[repoID]; ok {
			out = append(out, repo)
		}
	}
	return out
}

func selectedPinCandidates(candidates []profilePinCandidate, repoIDs []int64) []profilePinCandidate {
	byID := make(map[int64]profilePinCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	out := make([]profilePinCandidate, 0, len(repoIDs))
	for _, repoID := range repoIDs {
		if candidate, ok := byID[repoID]; ok {
			out = append(out, candidate)
		}
	}
	return out
}

func markPinnedCandidates(candidates []profilePinCandidate, repoIDs []int64) {
	positions := make(map[int64]int, len(repoIDs))
	for i, repoID := range repoIDs {
		positions[repoID] = i + 1
	}
	for i := range candidates {
		if pos, ok := positions[candidates[i].ID]; ok {
			candidates[i].IsPinned = true
			candidates[i].PinPosition = pos
		}
	}
	sortPinCandidates(candidates)
}

func sortPinCandidates(candidates []profilePinCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.IsPinned != right.IsPinned {
			return left.IsPinned
		}
		if left.IsPinned && left.PinPosition != right.PinPosition {
			return left.PinPosition < right.PinPosition
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
}

// profilePinsRemaining reports how many additional pins the owner
// can add given the entitled cap. PRO08 C5: the cap is now a
// parameter — for orgs, callers pass profilePinLimit; for users,
// callers resolve via userProfilePinCap which reads entitlements.
// Previously the function hard-coded profilePinLimit and would
// under-count a Pro user's remaining slots.
func profilePinsRemaining(candidates []profilePinCandidate, cap int64) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.IsPinned {
			count++
		}
	}
	if int64(count) >= cap {
		return 0
	}
	return int(cap) - count
}
