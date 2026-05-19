// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	repoSecurityAdvisoryMaxDescriptionBytes  = 65535
	repoSecurityAdvisoryMaxReferenceURLs     = 20
	repoSecurityAdvisoryMaxReferenceURLBytes = 2048
)

type repoSecurityAdvisoryGate struct {
	Allowed     bool
	FeatureKey  string
	UpgradeHref string
	UpgradeText string
}

type repoSecurityAdvisoryForm struct {
	Severity           string
	Summary            string
	Description        string
	AffectedEcosystem  string
	AffectedPackage    string
	VulnerableVersions string
	PatchedVersions    string
	GHSAID             string
	CVEID              string
	ReferenceURLsText  string
}

type repoSecurityAdvisoryView struct {
	Row                 reposdb.RepoSecurityAdvisory
	ReferenceURLs       []string
	RenderedDescription template.HTML
}

type repoSecurityAdvisoryCollaboratorView struct {
	Kind        string
	UserID      int64
	TeamID      int64
	Name        string
	DisplayName string
	Role        string
}

func (h *Handlers) repoSecurityAdvisories(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	h.renderSecurityAdvisoriesPage(w, r, row, owner.Username, "", "")
}

func (h *Handlers) repoSecurityAdvisoryNew(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	form := repoSecurityAdvisoryForm{Severity: "moderate"}
	h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "")
}

func (h *Handlers) repoSecurityAdvisoryCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	gate := h.repoSecurityAdvisoryGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, repoSecurityAdvisoryForm{Severity: "moderate"}, &gate, "new",
			"Repository security advisories require Team for private organization repositories.")
		return
	}
	form, refs, errMsg := repoSecurityAdvisoryFormFromRequest(r)
	if errMsg != "" {
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", errMsg)
		return
	}
	refJSON, err := json.Marshal(refs)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: marshal references", "repo_id", row.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: begin create", "repo_id", row.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	advisory, err := h.rq.CreateRepoSecurityAdvisory(r.Context(), tx, reposdb.CreateRepoSecurityAdvisoryParams{
		RepoID:             row.ID,
		Identifier:         repoSecurityAdvisoryIdentifier(row.ID, form),
		State:              "draft",
		Severity:           form.Severity,
		Summary:            form.Summary,
		Description:        form.Description,
		AffectedEcosystem:  form.AffectedEcosystem,
		AffectedPackage:    form.AffectedPackage,
		VulnerableVersions: form.VulnerableVersions,
		PatchedVersions:    form.PatchedVersions,
		GhsaID:             form.GHSAID,
		CveID:              form.CVEID,
		ReferenceUrls:      refJSON,
		CreatedBy:          pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: create", "repo_id", row.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}
	if _, err := h.rq.CreateRepoSecurityAdvisoryEvent(r.Context(), tx, reposdb.CreateRepoSecurityAdvisoryEventParams{
		AdvisoryID: advisory.ID,
		RepoID:     row.ID,
		EventType:  "created",
		NewState:   advisory.State,
		ActorID:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: create event", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}
	if err := h.syncRepoSecurityAdvisoryDependencyAlerts(r.Context(), tx, advisory); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: sync dependency alerts on create", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: commit create", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "new", "Could not create the advisory.")
		return
	}
	http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
}

func (h *Handlers) repoSecurityAdvisoryDetail(w http.ResponseWriter, r *http.Request) {
	row, owner, advisory, ok := h.loadRepoSecurityAdvisoryForDisclosure(w, r)
	if !ok {
		return
	}
	h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "")
}

func (h *Handlers) renderSecurityAdvisoryDetail(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug string, advisory reposdb.RepoSecurityAdvisory, errMsg string) {
	canManage := h.canManageRepoSecurityAdvisories(r.Context(), row, middleware.CurrentUserFromContext(r.Context()))
	events, _ := h.rq.ListRepoSecurityAdvisoryEvents(r.Context(), h.d.Pool, advisory.ID)
	collaborators, err := h.rq.ListRepoSecurityAdvisoryCollaborators(r.Context(), h.d.Pool, advisory.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: list collaborators", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		collaborators = nil
	}
	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = advisory.Identifier + " · " + row.Name
	data["Advisory"] = h.repoSecurityAdvisoryView(r.Context(), row, ownerSlug, advisory)
	data["Events"] = events
	data["Collaborators"] = repoSecurityAdvisoryCollaboratorViews(collaborators)
	data["CanManageAdvisories"] = canManage
	data["WriteGate"] = h.repoSecurityAdvisoryGate(r.Context(), row, ownerSlug)
	data["Error"] = errMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_advisory_detail", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory detail render", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
	}
}

func (h *Handlers) repoSecurityAdvisoryEdit(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return
	}
	h.renderSecurityAdvisoryForm(w, r, row, owner.Username, repoSecurityAdvisoryFormFromRow(advisory), nil, "edit", "")
}

func (h *Handlers) repoSecurityAdvisoryUpdate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return
	}
	gate := h.repoSecurityAdvisoryGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, repoSecurityAdvisoryFormFromRow(advisory), &gate, "edit",
			"Repository security advisories require Team for private organization repositories.")
		return
	}
	form, refs, errMsg := repoSecurityAdvisoryFormFromRequest(r)
	if errMsg != "" {
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", errMsg)
		return
	}
	refJSON, err := json.Marshal(refs)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: marshal update references", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: begin update", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	updated, err := h.rq.UpdateRepoSecurityAdvisory(r.Context(), tx, reposdb.UpdateRepoSecurityAdvisoryParams{
		RepoID:             row.ID,
		Identifier:         advisory.Identifier,
		Severity:           form.Severity,
		Summary:            form.Summary,
		Description:        form.Description,
		AffectedEcosystem:  form.AffectedEcosystem,
		AffectedPackage:    form.AffectedPackage,
		VulnerableVersions: form.VulnerableVersions,
		PatchedVersions:    form.PatchedVersions,
		GhsaID:             form.GHSAID,
		CveID:              form.CVEID,
		ReferenceUrls:      refJSON,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: update", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}
	if _, err := h.rq.CreateRepoSecurityAdvisoryEvent(r.Context(), tx, reposdb.CreateRepoSecurityAdvisoryEventParams{
		AdvisoryID: updated.ID,
		RepoID:     row.ID,
		EventType:  "updated",
		OldState:   advisory.State,
		NewState:   updated.State,
		ActorID:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: update event", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}
	if err := h.syncRepoSecurityAdvisoryDependencyAlerts(r.Context(), tx, updated); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: sync dependency alerts on update", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: commit update", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.renderSecurityAdvisoryForm(w, r, row, owner.Username, form, nil, "edit", "Could not update the advisory.")
		return
	}
	http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, updated.Identifier), http.StatusSeeOther)
}

func (h *Handlers) repoSecurityAdvisoryState(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return
	}
	gate := h.repoSecurityAdvisoryGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	newState, eventType, ok := repoSecurityAdvisoryStateAction(r.PostFormValue("action"))
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid advisory state")
		return
	}
	message := strings.TrimSpace(r.PostFormValue("message"))
	if len(message) > 500 {
		message = message[:500]
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: begin state", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	updated, err := h.rq.SetRepoSecurityAdvisoryState(r.Context(), tx, reposdb.SetRepoSecurityAdvisoryStateParams{
		RepoID:     row.ID,
		Identifier: advisory.Identifier,
		State:      newState,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: set state", "repo_id", row.ID, "advisory_id", advisory.ID, "state", newState, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, err := h.rq.CreateRepoSecurityAdvisoryEvent(r.Context(), tx, reposdb.CreateRepoSecurityAdvisoryEventParams{
		AdvisoryID: updated.ID,
		RepoID:     row.ID,
		EventType:  eventType,
		OldState:   advisory.State,
		NewState:   updated.State,
		Message:    message,
		ActorID:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: state event", "repo_id", row.ID, "advisory_id", advisory.ID, "state", newState, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.syncRepoSecurityAdvisoryDependencyAlerts(r.Context(), tx, updated); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: sync dependency alerts on state", "repo_id", row.ID, "advisory_id", advisory.ID, "state", newState, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory: commit state", "repo_id", row.ID, "advisory_id", advisory.ID, "state", newState, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, updated.Identifier), http.StatusSeeOther)
}

func (h *Handlers) repoSecurityAdvisoryCollaboratorAdd(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return
	}
	gate := h.repoSecurityAdvisoryGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Could not parse collaborator form.")
		return
	}
	rawSubject := strings.TrimSpace(r.PostFormValue("collaborator"))
	role := normalizeRepoSecurityAdvisoryCollaboratorRole(r.PostFormValue("role"))
	viewer := middleware.CurrentUserFromContext(r.Context())

	eventMessage, err := h.addRepoSecurityAdvisoryCollaborator(r.Context(), row, advisory, rawSubject, role, viewer.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "security-advisory: add collaborator", "repo_id", row.ID, "advisory_id", advisory.ID, "subject", rawSubject, "error", err)
		h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, err.Error())
		return
	}
	if _, err := h.rq.CreateRepoSecurityAdvisoryEvent(r.Context(), h.d.Pool, reposdb.CreateRepoSecurityAdvisoryEventParams{
		AdvisoryID: advisory.ID,
		RepoID:     row.ID,
		EventType:  "collaborator_added",
		NewState:   advisory.State,
		Message:    eventMessage,
		ActorID:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "security-advisory: collaborator add event", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
	}
	http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
}

func (h *Handlers) repoSecurityAdvisoryCollaboratorRemove(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return
	}
	gate := h.repoSecurityAdvisoryGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Could not parse collaborator form.")
		return
	}
	kind := strings.TrimSpace(r.PostFormValue("subject_type"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("subject_id")), 10, 64)
	if err != nil || id <= 0 {
		h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Choose a collaborator to remove.")
		return
	}
	message := ""
	switch kind {
	case "user":
		if err := h.rq.RemoveRepoSecurityAdvisoryUserCollaborator(r.Context(), h.d.Pool, reposdb.RemoveRepoSecurityAdvisoryUserCollaboratorParams{
			AdvisoryID: advisory.ID,
			UserID:     pgtype.Int8{Int64: id, Valid: true},
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "security-advisory: remove user collaborator", "repo_id", row.ID, "advisory_id", advisory.ID, "user_id", id, "error", err)
			h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Could not remove collaborator.")
			return
		}
		message = "Removed user collaborator."
	case "team":
		if err := h.rq.RemoveRepoSecurityAdvisoryTeamCollaborator(r.Context(), h.d.Pool, reposdb.RemoveRepoSecurityAdvisoryTeamCollaboratorParams{
			AdvisoryID: advisory.ID,
			TeamID:     pgtype.Int8{Int64: id, Valid: true},
		}); err != nil {
			h.d.Logger.WarnContext(r.Context(), "security-advisory: remove team collaborator", "repo_id", row.ID, "advisory_id", advisory.ID, "team_id", id, "error", err)
			h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Could not remove collaborator.")
			return
		}
		message = "Removed team collaborator."
	default:
		h.renderSecurityAdvisoryDetail(w, r, row, owner.Username, advisory, "Choose a collaborator to remove.")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := h.rq.CreateRepoSecurityAdvisoryEvent(r.Context(), h.d.Pool, reposdb.CreateRepoSecurityAdvisoryEventParams{
		AdvisoryID: advisory.ID,
		RepoID:     row.ID,
		EventType:  "collaborator_removed",
		NewState:   advisory.State,
		Message:    message,
		ActorID:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "security-advisory: collaborator remove event", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
	}
	http.Redirect(w, r, repoSecurityAdvisoryPath(owner.Username, row.Name, advisory.Identifier), http.StatusSeeOther)
}

func (h *Handlers) renderSecurityAdvisoriesPage(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug, errMsg, successMsg string) {
	advisories, err := h.rq.ListRepoSecurityAdvisories(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisories: list", "repo_id", row.ID, "error", err)
		advisories = nil
	}
	canManage := h.canManageRepoSecurityAdvisories(r.Context(), row, middleware.CurrentUserFromContext(r.Context()))
	viewer := middleware.CurrentUserFromContext(r.Context())
	stateFilter := normalizeRepoSecurityAdvisoryStateFilter(r.URL.Query().Get("state"))
	views := make([]repoSecurityAdvisoryView, 0, len(advisories))
	stateCounts := map[string]int{"draft": 0, "published": 0, "withdrawn": 0, "archived": 0}
	for _, advisory := range advisories {
		if advisory.State != "published" && !canManage && !h.userCanAccessRepoSecurityAdvisory(r.Context(), advisory.ID, viewer.ID) {
			continue
		}
		stateCounts[advisory.State]++
		if stateFilter != "" && advisory.State != stateFilter {
			continue
		}
		views = append(views, h.repoSecurityAdvisoryView(r.Context(), row, ownerSlug, advisory))
	}
	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = "Security advisories · " + row.Name
	data["Advisories"] = views
	data["StateFilter"] = stateFilter
	data["StateCounts"] = stateCounts
	data["CanManageAdvisories"] = canManage
	data["WriteGate"] = h.repoSecurityAdvisoryGate(r.Context(), row, ownerSlug)
	data["Error"] = errMsg
	data["Success"] = successMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_advisories", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisories page render", "repo_id", row.ID, "error", err)
	}
}

func (h *Handlers) renderSecurityAdvisoryForm(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug string, form repoSecurityAdvisoryForm, gate *repoSecurityAdvisoryGate, mode, errMsg string) {
	if form.Severity == "" {
		form.Severity = "moderate"
	}
	if gate == nil {
		g := h.repoSecurityAdvisoryGate(r.Context(), row, ownerSlug)
		gate = &g
	}
	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = "New security advisory · " + row.Name
	if mode == "edit" {
		data["Title"] = "Edit security advisory · " + row.Name
		if advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID); ok {
			data["Advisory"] = h.repoSecurityAdvisoryView(r.Context(), row, ownerSlug, advisory)
		}
	}
	data["Form"] = form
	data["Mode"] = mode
	data["WriteGate"] = *gate
	data["Error"] = errMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_advisory_form", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "security-advisory form render", "repo_id", row.ID, "mode", mode, "error", err)
	}
}

func (h *Handlers) loadRepoSecurityAdvisory(w http.ResponseWriter, r *http.Request, repoID int64) (reposdb.RepoSecurityAdvisory, bool) {
	identifier := strings.TrimSpace(chi.URLParam(r, "identifier"))
	if identifier == "" || len(identifier) > 120 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.RepoSecurityAdvisory{}, false
	}
	advisory, err := h.rq.GetRepoSecurityAdvisoryByIdentifier(r.Context(), h.d.Pool, reposdb.GetRepoSecurityAdvisoryByIdentifierParams{
		RepoID:     repoID,
		Identifier: identifier,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.ErrorContext(r.Context(), "security-advisory: load", "repo_id", repoID, "identifier", identifier, "error", err)
		}
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.RepoSecurityAdvisory{}, false
	}
	return advisory, true
}

func (h *Handlers) loadRepoSecurityAdvisoryForDisclosure(w http.ResponseWriter, r *http.Request) (reposdb.Repo, usersdb.User, reposdb.RepoSecurityAdvisory, bool) {
	row, owner, ok := h.loadRepoByRoute(w, r)
	if !ok {
		return reposdb.Repo{}, usersdb.User{}, reposdb.RepoSecurityAdvisory{}, false
	}
	advisory, ok := h.loadRepoSecurityAdvisory(w, r, row.ID)
	if !ok {
		return reposdb.Repo{}, usersdb.User{}, reposdb.RepoSecurityAdvisory{}, false
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := viewer.PolicyActor()
	repoRef := policy.NewRepoRefFromRepo(row)
	readDecision := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoRead, repoRef)
	advisoryCollaborator := h.userCanAccessRepoSecurityAdvisory(r.Context(), advisory.ID, viewer.ID)
	if !readDecision.Allow && !advisoryCollaborator {
		h.d.Render.HTTPError(w, r, policy.Maybe404(readDecision, repoRef, actor), "")
		return reposdb.Repo{}, usersdb.User{}, reposdb.RepoSecurityAdvisory{}, false
	}
	if advisory.State != "published" && !h.canManageRepoSecurityAdvisories(r.Context(), row, viewer) && !advisoryCollaborator {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, reposdb.RepoSecurityAdvisory{}, false
	}
	return row, owner, advisory, true
}

func (h *Handlers) loadRepoByRoute(w http.ResponseWriter, r *http.Request) (reposdb.Repo, usersdb.User, bool) {
	ownerName := chi.URLParam(r, "owner")
	repoName := chi.URLParam(r, "repo")
	principal, err := orgs.Resolve(r.Context(), h.d.Pool, ownerName)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	var (
		row   reposdb.Repo
		owner usersdb.User
	)
	switch principal.Kind {
	case orgs.PrincipalUser:
		owner, err = h.uq.GetUserByID(r.Context(), h.d.Pool, principal.ID)
		if err != nil {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return reposdb.Repo{}, usersdb.User{}, false
		}
		row, err = h.rq.GetRepoByOwnerUserAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
			Name:        repoName,
		})
	case orgs.PrincipalOrg:
		row, err = h.rq.GetRepoByOwnerOrgAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:       repoName,
		})
		owner = usersdb.User{ID: principal.ID, Username: principal.Slug}
	default:
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return reposdb.Repo{}, usersdb.User{}, false
	}
	return row, owner, true
}

func (h *Handlers) userCanAccessRepoSecurityAdvisory(ctx context.Context, advisoryID int64, userID int64) bool {
	if advisoryID == 0 || userID == 0 {
		return false
	}
	ok, err := h.rq.UserCanAccessRepoSecurityAdvisory(ctx, h.d.Pool, reposdb.UserCanAccessRepoSecurityAdvisoryParams{
		AdvisoryID: advisoryID,
		UserID:     pgtype.Int8{Int64: userID, Valid: true},
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "security-advisory: collaborator access check", "advisory_id", advisoryID, "user_id", userID, "error", err)
		return false
	}
	return ok
}

func (h *Handlers) repoSecurityAdvisoryView(ctx context.Context, row reposdb.Repo, ownerSlug string, advisory reposdb.RepoSecurityAdvisory) repoSecurityAdvisoryView {
	viewer := middleware.CurrentUserFromContext(ctx)
	rendered, _, _, err := mdrender.Render(ctx, []byte(advisory.Description), mdrender.Options{
		Repo: &mdrender.RepoContext{
			OwnerUsername: ownerSlug,
			RepoName:      row.Name,
			RepoID:        row.ID,
		},
		ViewerUserID: viewer.ID,
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "security-advisory: markdown render failed", "repo_id", row.ID, "advisory_id", advisory.ID, "error", err)
		rendered = nil
	}
	return repoSecurityAdvisoryView{
		Row:                 advisory,
		ReferenceURLs:       repoSecurityAdvisoryReferenceURLs(advisory.ReferenceUrls),
		RenderedDescription: template.HTML(rendered), //nolint:gosec // sanitized by internal/markdown Render.
	}
}

func (h *Handlers) addRepoSecurityAdvisoryCollaborator(ctx context.Context, row reposdb.Repo, advisory reposdb.RepoSecurityAdvisory, rawSubject, role string, actorID int64) (string, error) {
	subject := normalizeRepoSecurityAdvisoryCollaboratorSubject(rawSubject)
	if subject == "" {
		return "", errors.New("enter a username or team slug")
	}
	if role == "" {
		role = "read"
	}
	addedBy := pgtype.Int8{Int64: actorID, Valid: actorID != 0}
	if strings.HasPrefix(subject, "team:") {
		if !row.OwnerOrgID.Valid {
			return "", errors.New("team collaborators require an organization-owned repository")
		}
		teamSlug := strings.TrimPrefix(subject, "team:")
		team, err := orgsdb.New().GetTeamByOrgAndSlug(ctx, h.d.Pool, orgsdb.GetTeamByOrgAndSlugParams{
			OrgID: row.OwnerOrgID.Int64,
			Slug:  teamSlug,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", errors.New("team collaborator not found")
			}
			return "", err
		}
		if _, err := h.rq.AddRepoSecurityAdvisoryTeamCollaborator(ctx, h.d.Pool, reposdb.AddRepoSecurityAdvisoryTeamCollaboratorParams{
			AdvisoryID: advisory.ID,
			TeamID:     pgtype.Int8{Int64: team.ID, Valid: true},
			Role:       role,
			AddedBy:    addedBy,
		}); err != nil {
			return "", err
		}
		return "Added team " + team.Slug + " as " + role + " collaborator.", nil
	}
	user, err := h.uq.GetUserByUsername(ctx, h.d.Pool, subject)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && row.OwnerOrgID.Valid {
			team, teamErr := orgsdb.New().GetTeamByOrgAndSlug(ctx, h.d.Pool, orgsdb.GetTeamByOrgAndSlugParams{
				OrgID: row.OwnerOrgID.Int64,
				Slug:  subject,
			})
			if teamErr == nil {
				if _, err := h.rq.AddRepoSecurityAdvisoryTeamCollaborator(ctx, h.d.Pool, reposdb.AddRepoSecurityAdvisoryTeamCollaboratorParams{
					AdvisoryID: advisory.ID,
					TeamID:     pgtype.Int8{Int64: team.ID, Valid: true},
					Role:       role,
					AddedBy:    addedBy,
				}); err != nil {
					return "", err
				}
				return "Added team " + team.Slug + " as " + role + " collaborator.", nil
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errors.New("collaborator not found")
		}
		return "", err
	}
	if row.OwnerUserID.Valid && row.OwnerUserID.Int64 == user.ID {
		return "", errors.New("the repository owner already has advisory access")
	}
	if _, err := h.rq.AddRepoSecurityAdvisoryUserCollaborator(ctx, h.d.Pool, reposdb.AddRepoSecurityAdvisoryUserCollaboratorParams{
		AdvisoryID: advisory.ID,
		UserID:     pgtype.Int8{Int64: user.ID, Valid: true},
		Role:       role,
		AddedBy:    addedBy,
	}); err != nil {
		return "", err
	}
	return "Added " + user.Username + " as " + role + " collaborator.", nil
}

func repoSecurityAdvisoryCollaboratorViews(rows []reposdb.ListRepoSecurityAdvisoryCollaboratorsRow) []repoSecurityAdvisoryCollaboratorView {
	views := make([]repoSecurityAdvisoryCollaboratorView, 0, len(rows))
	for _, row := range rows {
		view := repoSecurityAdvisoryCollaboratorView{Role: row.Role}
		if row.UserID.Valid {
			view.Kind = "user"
			view.UserID = row.UserID.Int64
			view.Name = row.Username.String
		} else if row.TeamID.Valid {
			view.Kind = "team"
			view.TeamID = row.TeamID.Int64
			view.Name = row.TeamSlug.String
			view.DisplayName = row.TeamDisplayName.String
		}
		if view.Name != "" {
			views = append(views, view)
		}
	}
	return views
}

func (h *Handlers) repoSecurityAdvisoryGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoSecurityAdvisoryGate {
	gate := repoSecurityAdvisoryGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureSecurityAdvisories),
		UpgradeHref: "/organizations/" + ownerSlug + "/settings/billing",
		UpgradeText: "Upgrade to Team",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate || !row.OwnerOrgID.Valid {
		return gate
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(row.OwnerOrgID.Int64),
		entitlements.FeatureSecurityAdvisories)
	if err != nil {
		gate.Allowed = false
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

func (h *Handlers) syncRepoSecurityAdvisoryDependencyAlerts(ctx context.Context, db reposdb.DBTX, advisory reposdb.RepoSecurityAdvisory) error {
	if strings.TrimSpace(advisory.AffectedEcosystem) == "" || strings.TrimSpace(advisory.AffectedPackage) == "" {
		return nil
	}
	source := repoSecurityAdvisoryDependencySource(advisory.RepoID)
	withdrawnAt := pgtype.Timestamptz{}
	if advisory.State != "published" {
		withdrawnAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	if _, err := h.rq.UpsertDependencyAdvisory(ctx, db, reposdb.UpsertDependencyAdvisoryParams{
		Source:          source,
		ExternalID:      advisory.Identifier,
		Ecosystem:       advisory.AffectedEcosystem,
		PackageName:     advisory.AffectedPackage,
		AffectedRange:   advisory.VulnerableVersions,
		PatchedVersions: advisory.PatchedVersions,
		Severity:        advisory.Severity,
		Summary:         advisory.Summary,
		Description:     advisory.Description,
		ReferenceUrls:   advisory.ReferenceUrls,
		PublishedAt:     advisory.PublishedAt,
		WithdrawnAt:     withdrawnAt,
	}); err != nil {
		return err
	}
	params := reposdb.RefreshDependencyAlertsForAdvisoryParams{
		Source:     source,
		ExternalID: advisory.Identifier,
	}
	if advisory.State == "published" {
		if err := h.rq.RefreshDependencyAlertsForAdvisory(ctx, db, params); err != nil {
			return err
		}
	}
	return h.rq.ResolveStaleDependencyAlertsForAdvisory(ctx, db, reposdb.ResolveStaleDependencyAlertsForAdvisoryParams(params))
}

func (h *Handlers) canManageRepoSecurityAdvisories(ctx context.Context, row reposdb.Repo, viewer middleware.CurrentUser) bool {
	if viewer.IsAnonymous() {
		return false
	}
	decision := policy.Can(ctx, policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoSettingsGeneral, policy.NewRepoRefFromRepo(row))
	return decision.Allow
}

func repoSecurityAdvisoryFormFromRequest(r *http.Request) (repoSecurityAdvisoryForm, []string, string) {
	if err := r.ParseForm(); err != nil {
		return repoSecurityAdvisoryForm{}, nil, "Could not read the advisory form."
	}
	rawSeverity := strings.TrimSpace(r.PostFormValue("severity"))
	form := repoSecurityAdvisoryForm{
		Severity:           normalizeRepoSecurityAdvisorySeverity(rawSeverity),
		Summary:            strings.TrimSpace(r.PostFormValue("summary")),
		Description:        strings.TrimSpace(r.PostFormValue("description")),
		AffectedEcosystem:  strings.TrimSpace(r.PostFormValue("affected_ecosystem")),
		AffectedPackage:    strings.TrimSpace(r.PostFormValue("affected_package")),
		VulnerableVersions: strings.TrimSpace(r.PostFormValue("vulnerable_versions")),
		PatchedVersions:    strings.TrimSpace(r.PostFormValue("patched_versions")),
		GHSAID:             strings.ToUpper(strings.TrimSpace(r.PostFormValue("ghsa_id"))),
		CVEID:              strings.ToUpper(strings.TrimSpace(r.PostFormValue("cve_id"))),
		ReferenceURLsText:  strings.TrimSpace(r.PostFormValue("reference_urls")),
	}
	if form.Severity == "" {
		if rawSeverity != "" {
			return form, nil, "Choose a valid severity."
		}
		form.Severity = "moderate"
	}
	switch {
	case form.Summary == "":
		return form, nil, "Summary is required."
	case len(form.Summary) > 500:
		return form, nil, "Summary is too long."
	case len(form.Description) > repoSecurityAdvisoryMaxDescriptionBytes:
		return form, nil, "Description is too long."
	case form.AffectedEcosystem == "":
		return form, nil, "Affected ecosystem is required."
	case len(form.AffectedEcosystem) > 40:
		return form, nil, "Affected ecosystem is too long."
	case form.AffectedPackage == "":
		return form, nil, "Affected package is required."
	case len(form.AffectedPackage) > 512:
		return form, nil, "Affected package is too long."
	case len(form.VulnerableVersions) > 255:
		return form, nil, "Vulnerable versions is too long."
	case len(form.PatchedVersions) > 255:
		return form, nil, "Patched versions is too long."
	case len(form.GHSAID) > 120:
		return form, nil, "GHSA identifier is too long."
	case len(form.CVEID) > 120:
		return form, nil, "CVE identifier is too long."
	}
	refs, errMsg := repoSecurityAdvisoryParseReferenceURLs(form.ReferenceURLsText)
	if errMsg != "" {
		return form, nil, errMsg
	}
	return form, refs, ""
}

func repoSecurityAdvisoryFormFromRow(row reposdb.RepoSecurityAdvisory) repoSecurityAdvisoryForm {
	return repoSecurityAdvisoryForm{
		Severity:           row.Severity,
		Summary:            row.Summary,
		Description:        row.Description,
		AffectedEcosystem:  row.AffectedEcosystem,
		AffectedPackage:    row.AffectedPackage,
		VulnerableVersions: row.VulnerableVersions,
		PatchedVersions:    row.PatchedVersions,
		GHSAID:             row.GhsaID,
		CVEID:              row.CveID,
		ReferenceURLsText:  strings.Join(repoSecurityAdvisoryReferenceURLs(row.ReferenceUrls), "\n"),
	}
}

func repoSecurityAdvisoryIdentifier(repoID int64, form repoSecurityAdvisoryForm) string {
	if form.GHSAID != "" {
		return form.GHSAID
	}
	if form.CVEID != "" {
		return form.CVEID
	}
	return fmt.Sprintf("SHSA-%d-%d", repoID, time.Now().UTC().UnixNano())
}

func repoSecurityAdvisoryPath(owner, repoName, identifier string) string {
	return "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/security/advisories/" + url.PathEscape(identifier)
}

func repoSecurityAdvisoryDependencySource(repoID int64) string {
	return fmt.Sprintf("repo-security-advisory:%d", repoID)
}

func normalizeRepoSecurityAdvisoryCollaboratorRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "write", "admin":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "read"
	}
}

func normalizeRepoSecurityAdvisoryCollaboratorSubject(raw string) string {
	subject := strings.ToLower(strings.TrimSpace(raw))
	subject = strings.TrimPrefix(subject, "@")
	subject = strings.TrimPrefix(subject, "/")
	if strings.HasPrefix(subject, "teams/") {
		return "team:" + strings.TrimPrefix(subject, "teams/")
	}
	if strings.HasPrefix(subject, "team/") {
		return "team:" + strings.TrimPrefix(subject, "team/")
	}
	return subject
}

func normalizeRepoSecurityAdvisorySeverity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low", "moderate", "high", "critical":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func normalizeRepoSecurityAdvisoryStateFilter(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "draft", "published", "withdrawn", "archived":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func repoSecurityAdvisoryStateAction(raw string) (state, eventType string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "publish":
		return "published", "published", true
	case "withdraw":
		return "withdrawn", "withdrawn", true
	case "archive":
		return "archived", "archived", true
	case "reopen":
		return "draft", "reopened", true
	default:
		return "", "", false
	}
}

func repoSecurityAdvisoryParseReferenceURLs(raw string) ([]string, string) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, ""
	}
	out := []string{}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(raw, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		if len(ref) > repoSecurityAdvisoryMaxReferenceURLBytes {
			return nil, "Reference URLs must be 2048 characters or less."
		}
		u, err := url.Parse(ref)
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, "Reference URLs must be valid http or https URLs."
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
		if len(out) > repoSecurityAdvisoryMaxReferenceURLs {
			return nil, "Security advisories can include at most 20 reference URLs."
		}
	}
	return out, ""
}

func repoSecurityAdvisoryReferenceURLs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var refs []string
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil
	}
	return refs
}
