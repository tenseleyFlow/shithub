// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// reposList renders the paginated, filterable repo list.
func (h *Handlers) reposList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	deletedOnly := q.Get("deleted") == "1"
	archivedOnly := q.Get("archived") == "1"
	visibility := q.Get("visibility")
	const perPage = 50
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	params := admindb.ListReposForAdminParams{
		NamePrefix:   pgtype.Text{String: prefix, Valid: prefix != ""},
		DeletedOnly:  pgtype.Bool{Bool: true, Valid: deletedOnly},
		ArchivedOnly: pgtype.Bool{Bool: true, Valid: archivedOnly},
		Limit:        perPage,
		Offset:       int32((page - 1) * perPage),
	}
	if visibility == "public" || visibility == "private" {
		params.VisibilityFilter = admindb.NullRepoVisibility{
			RepoVisibility: admindb.RepoVisibility(visibility), Valid: true,
		}
	}
	rows, _ := h.aq.ListReposForAdmin(r.Context(), h.d.Pool, params)
	h.d.Render.RenderPage(w, r, "admin/repos_list", map[string]any{
		"Title":        "Repositories",
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"AdminActive":  "repos",
		"Repos":        rows,
		"Q":            prefix,
		"DeletedOnly":  deletedOnly,
		"ArchivedOnly": archivedOnly,
		"Visibility":   visibility,
		"Page":         page,
		"NextPage":     page + 1,
		"PrevPage":     page - 1,
		"HasMore":      len(rows) == perPage,
		"Notice":       adminNotice(q.Get("notice")),
	})
}

// repoView renders the per-repo admin page with action buttons.
func (h *Handlers) repoView(w http.ResponseWriter, r *http.Request) {
	row, ok := h.loadRepo(w, r)
	if !ok {
		return
	}
	notice := r.URL.Query().Get("notice")
	h.d.Render.RenderPage(w, r, "admin/repo_view", map[string]any{
		"Title":       row.Name + " · admin",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "repos",
		"Repo":        row,
		"Notice":      adminNotice(notice),
	})
}

// repoForceArchive forces is_archived=true without owner consent.
// Used for emergency takedown — the normal Archive flow is owner-only.
// Idempotent: a no-op when the repo is already archived (still audited).
func (h *Handlers) repoForceArchive(w http.ResponseWriter, r *http.Request) {
	row, ok := h.loadRepo(w, r)
	if !ok {
		return
	}
	if err := reposdb.New().ArchiveRepo(r.Context(), h.d.Pool, row.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "force-archive failed")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminRepoForceArchived, audit.TargetRepo, row.ID, nil)
	http.Redirect(w, r, "/admin/repos/"+strconv.FormatInt(row.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// repoForceUnarchive forces is_archived=false. Separate route from
// repoForceArchive (split per SR2 H8 — pre-fix the single route was
// a toggle, so clicking on an already-archived repo silently
// un-archived it with a misleading audit row).
func (h *Handlers) repoForceUnarchive(w http.ResponseWriter, r *http.Request) {
	row, ok := h.loadRepo(w, r)
	if !ok {
		return
	}
	if err := reposdb.New().UnarchiveRepo(r.Context(), h.d.Pool, row.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "force-unarchive failed")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminRepoForceUnarchived, audit.TargetRepo, row.ID, nil)
	http.Redirect(w, r, "/admin/repos/"+strconv.FormatInt(row.ID, 10)+"?notice=saved", http.StatusSeeOther)
}

// repoForceDelete bypasses the soft-delete grace by setting deleted_at
// to a time well past the grace window, then hard-delete sweeps it on
// the next lifecycle:sweep tick.
func (h *Handlers) repoForceDelete(w http.ResponseWriter, r *http.Request) {
	row, ok := h.loadRepo(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	if confirm != row.Name {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "confirmation text didn't match the repo name")
		return
	}
	if err := reposdb.New().AdminForceDeleteRepo(r.Context(), h.d.Pool, row.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "force-delete failed")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminRepoForceDeleted, audit.TargetRepo, row.ID,
		map[string]any{"name": row.Name})
	http.Redirect(w, r, "/admin/repos?notice=saved", http.StatusSeeOther)
}

// loadRepo parses {id} and fetches.
func (h *Handlers) loadRepo(w http.ResponseWriter, r *http.Request) (reposdb.Repo, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "bad id")
		return reposdb.Repo{}, false
	}
	row, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, false
	}
	return row, true
}
