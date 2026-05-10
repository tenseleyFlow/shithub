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
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

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
	repoIDs, err := selectedProfilePinIDs(r.PostForm["repo_id"], candidates)
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

	candidates := h.publicUserPinCandidates(ctx, user.ID, user.Username)
	repoIDs, err := selectedProfilePinIDs(r.PostForm["repo_id"], candidates)
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
	candidates := h.publicUserPinCandidates(ctx, user.ID, user.Username)
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

func (h *Handlers) publicUserPinCandidates(ctx context.Context, userID int64, ownerSlug string) []profilePinCandidate {
	rows, err := reposdb.New().ListReposForOwnerUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "profile pins: list user repos", "user_id", userID, "error", err)
		return nil
	}
	out := make([]profilePinCandidate, 0, len(rows))
	for _, row := range rows {
		if row.Visibility != reposdb.RepoVisibilityPublic {
			continue
		}
		out = append(out, profilePinCandidateFromRepo(ownerSlug, row))
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
		if row.Visibility != reposdb.RepoVisibilityPublic {
			continue
		}
		out = append(out, profilePinCandidateFromRepo(orgSlug, row))
	}
	sortPinCandidates(out)
	return out
}

func selectedProfilePinIDs(values []string, candidates []profilePinCandidate) ([]int64, error) {
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
	if len(out) > profilePinLimit {
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
		if repo.Visibility == string(reposdb.RepoVisibilityPublic) {
			out = append(out, repo)
		}
	}
	return out
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

func profilePinsRemaining(candidates []profilePinCandidate) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.IsPinned {
			count++
		}
	}
	if count >= profilePinLimit {
		return 0
	}
	return profilePinLimit - count
}
