// SPDX-License-Identifier: AGPL-3.0-or-later

// Package orgs wires the S30 organization web surface:
//
//	GET  /organizations/new            create form
//	POST /organizations                create submit
//	GET  /orgs/{org}/repositories                          repository list
//	GET  /{org}/people                                      members + pending invites + invite form
//	POST /{org}/people/invite                               invite by username or email
//	POST /{org}/people/{user}/role                          change role
//	POST /{org}/people/{user}/remove                        remove member
//	GET  /organizations/{org}/settings/profile              profile settings
//	GET  /organizations/{org}/settings/import               GitHub org import
//	POST /organizations/{org}/settings/import               start GitHub org import
//	GET  /organizations/{org}/imports/{importID}            GitHub org import progress
//	GET  /organizations/{org}/settings/{secrets,variables}/actions
//	POST /organizations/{org}/settings/{secrets,variables}/actions
//	GET  /invitations/{token}                               accept/decline view
//	POST /invitations/{token}/accept                        accept
//	POST /invitations/{token}/decline                       decline
//
// Profile rendering for /{org} is dispatched from the existing
// /{username} catch-all in internal/web/handlers/profile via the
// principals.Resolve lookup; this handler set only owns the org-
// specific surfaces.
package orgs

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	authemail "github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the handler set.
type Deps struct {
	Logger      *slog.Logger
	Render      *render.Renderer
	Pool        *pgxpool.Pool
	EmailSender authemail.Sender
	EmailFrom   string
	SiteName    string
	BaseURL     string
	ObjectStore storage.ObjectStore
	SecretBox   *secretbox.Box
	Audit       *audit.Recorder
}

// Handlers groups the org surface handlers.
type Handlers struct {
	d Deps
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("orgs handlers: nil Render")
	}
	if d.Pool == nil {
		return nil, errors.New("orgs handlers: nil Pool")
	}
	if d.Audit == nil {
		d.Audit = audit.NewRecorder()
	}
	return &Handlers{d: d}, nil
}

// MountCreate registers /organizations/new, POST /organizations, and
// organization settings routes under /organizations/{org}/settings/*.
// Caller wraps these in RequireUser since they require a logged-in
// actor. The /organizations prefix is on the auth-reserved list so it
// never shadows a user/org slug.
func (h *Handlers) MountCreate(r chi.Router) {
	r.Get("/organizations/new", h.newForm)
	r.Post("/organizations", h.createSubmit)
	r.Get("/organizations/{org}/settings/profile", h.settingsProfile)
	r.Post("/organizations/{org}/settings/profile", h.settingsProfileSubmit)
	r.Post("/organizations/{org}/settings/profile/avatar", h.settingsAvatarUpload)
	r.Post("/organizations/{org}/settings/profile/avatar/remove", h.settingsAvatarRemove)
	r.Post("/organizations/{org}/settings/delete", h.settingsDelete)
	r.Get("/organizations/{org}/settings/import", h.settingsImport)
	r.Post("/organizations/{org}/settings/import", h.settingsImportSubmit)
	r.Get("/organizations/{org}/imports/{importID}", h.importProgress)
	r.Get("/organizations/{org}/settings/secrets/actions", h.settingsActionsSecrets)
	r.Post("/organizations/{org}/settings/secrets/actions", h.settingsActionsSecretSet)
	r.Post("/organizations/{org}/settings/secrets/actions/{name}/delete", h.settingsActionsSecretDelete)
	r.Get("/organizations/{org}/settings/variables/actions", h.settingsActionsVariables)
	r.Post("/organizations/{org}/settings/variables/actions", h.settingsActionsVariableSet)
	r.Post("/organizations/{org}/settings/variables/actions/{name}/delete", h.settingsActionsVariableDelete)
}

// MountOrgRoutes registers the per-org surface under /{org}/people
// and /{org}/settings. Caller MUST register this before the
// /{username} catch-all so the `people` segment matches.
//
// Member-management routes live behind RequireUser at the wiring
// layer (server.go); profile-style reads stay public.
func (h *Handlers) MountOrgRoutes(r chi.Router) {
	r.Get("/{org}/people", h.peoplePage)
	r.Post("/{org}/people/invite", h.invite)
	r.Post("/{org}/people/{userID}/role", h.changeRole)
	r.Post("/{org}/people/{userID}/remove", h.removeMember)
	h.MountTeams(r)
}

// MountInvitations registers /invitations/{token}* — accept/decline.
// Authed-only; the page also shows a hint when the viewer's logged-in
// user doesn't match the invite's target email.
func (h *Handlers) MountInvitations(r chi.Router) {
	r.Get("/invitations/{token}", h.invitationView)
	r.Post("/invitations/{token}/accept", h.invitationAccept)
	r.Post("/invitations/{token}/decline", h.invitationDecline)
}

// ─── helpers ───────────────────────────────────────────────────────

func (h *Handlers) deps() orgs.Deps {
	return orgs.Deps{
		Pool:        h.d.Pool,
		Logger:      h.d.Logger,
		EmailSender: h.d.EmailSender,
		EmailFrom:   h.d.EmailFrom,
		SiteName:    h.d.SiteName,
		BaseURL:     h.d.BaseURL,
	}
}

// orgFromSlug resolves the org from a {org} URL param, with an
// existence-leak-safe 404 path.
func (h *Handlers) orgFromSlug(w http.ResponseWriter, r *http.Request) (orgsdb.Org, bool) {
	slug := chi.URLParam(r, "org")
	row, err := orgsdb.New().GetOrgBySlug(r.Context(), h.d.Pool, slug)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return orgsdb.Org{}, false
	}
	return row, true
}

func parseUserIDParam(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// ─── create ────────────────────────────────────────────────────────

func (h *Handlers) newForm(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next=/organizations/new", http.StatusSeeOther)
		return
	}
	h.renderNewForm(w, r, orgCreateForm{}, "")
}

type orgCreateForm struct {
	Slug         string
	DisplayName  string
	BillingEmail string
	GitHubOrg    string
	GitHubToken  string
}

func (h *Handlers) createSubmit(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next=/organizations/new", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	form := orgCreateForm{
		Slug:         strings.TrimSpace(r.PostFormValue("slug")),
		DisplayName:  strings.TrimSpace(r.PostFormValue("display_name")),
		BillingEmail: strings.TrimSpace(r.PostFormValue("billing_email")),
		GitHubOrg:    strings.TrimSpace(r.PostFormValue("github_org")),
		GitHubToken:  strings.TrimSpace(r.PostFormValue("github_token")),
	}
	if form.GitHubOrg != "" {
		if _, err := orgs.NormalizeGitHubOrg(form.GitHubOrg); err != nil {
			h.renderNewForm(w, r, form, "GitHub organization must be a valid organization name or github.com organization URL.")
			return
		}
		if form.GitHubToken != "" && h.d.SecretBox == nil {
			h.renderNewForm(w, r, form.withoutToken(), "GitHub token imports require the server secret key to be configured.")
			return
		}
	}

	row, err := orgs.Create(r.Context(), h.deps(), orgs.CreateParams{
		Slug:            form.Slug,
		DisplayName:     form.DisplayName,
		BillingEmail:    form.BillingEmail,
		CreatedByUserID: viewer.ID,
	})
	if err != nil {
		h.renderNewForm(w, r, form.withoutToken(), friendlyOrgErr(err))
		return
	}
	if form.GitHubOrg != "" {
		imp, err := orgs.StartGitHubImport(r.Context(), orgs.ImportDeps{
			Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger,
		}, orgs.StartGitHubImportParams{
			OrgID: row.ID, SourceOrg: form.GitHubOrg,
			RequestedByUserID: viewer.ID, Token: form.GitHubToken,
		})
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: start GitHub import after create", "error", err, "org_id", row.ID)
			http.Redirect(w, r, "/organizations/"+row.Slug+"/settings/import?notice=start-failed", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/organizations/"+row.Slug+"/imports/"+strconv.FormatInt(imp.ID, 10), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/"+row.Slug, http.StatusSeeOther)
}

func (f orgCreateForm) withoutToken() orgCreateForm {
	f.GitHubToken = ""
	return f
}

func (h *Handlers) renderNewForm(w http.ResponseWriter, r *http.Request, form orgCreateForm, errMsg string) {
	if err := h.d.Render.RenderPage(w, r, "orgs/new", map[string]any{
		"Title":     "New organization",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Slug":      form.Slug,
		"Form":      form,
		"Error":     errMsg,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/new", "error", err)
	}
}

// ─── people ────────────────────────────────────────────────────────

func (h *Handlers) peoplePage(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	q := orgsdb.New()
	members, err := q.ListOrgMembers(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: list members", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	filteredMembers := filterOrgMembers(members, query)
	var pending []orgsdb.ListPendingInvitationsForOrgRow
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
		if isOwner {
			pending, _ = q.ListPendingInvitationsForOrg(r.Context(), h.d.Pool, org.ID)
		}
	}
	navCounts := h.orgNavCounts(r.Context(), org.ID, -1)
	if err := h.d.Render.RenderPage(w, r, "orgs/people", map[string]any{
		"Title":           org.Slug + " · people",
		"CSRFToken":       middleware.CSRFTokenForRequest(r),
		"Org":             org,
		"AvatarURL":       "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":    "people",
		"RepoCount":       navCounts.RepoCount,
		"Members":         filteredMembers,
		"MemberCount":     navCounts.MemberCount,
		"TeamCount":       navCounts.TeamCount,
		"Pending":         pending,
		"PendingCount":    len(pending),
		"Query":           query,
		"HasQuery":        query != "",
		"IsOwner":         isOwner,
		"CanManagePeople": isOwner,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/people", "error", err)
	}
}

func filterOrgMembers(members []orgsdb.ListOrgMembersRow, query string) []orgsdb.ListOrgMembersRow {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return members
	}
	out := make([]orgsdb.ListOrgMembersRow, 0, len(members))
	for _, member := range members {
		if strings.Contains(strings.ToLower(member.Username), query) ||
			strings.Contains(strings.ToLower(member.DisplayName), query) ||
			strings.Contains(strings.ToLower(string(member.Role)), query) {
			out = append(out, member)
		}
	}
	return out
}

func (h *Handlers) invite(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	// Suspended owners are denied with the same 403 as non-owners
	// (SR2 C4). Org/team mutations don't currently route through
	// policy.Can; this short-circuit mirrors the suspended-actor
	// gate every other write surface enforces.
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	owner, err := orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	if err != nil || !owner {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	role := r.PostFormValue("role")
	if role == "" {
		role = "member"
	}
	p := orgs.InviteParams{
		OrgID:           org.ID,
		InvitedByUserID: viewer.ID,
		Role:            role,
	}
	if strings.Contains(target, "@") {
		p.TargetEmail = target
	} else {
		p.TargetUsername = target
	}
	if _, err := orgs.Invite(r.Context(), h.deps(), p); err != nil {
		h.d.Logger.WarnContext(r.Context(), "orgs: invite failed",
			"org", org.Slug, "target", target, "error", err)
	}
	http.Redirect(w, r, "/"+org.Slug+"/people", http.StatusSeeOther)
}

func (h *Handlers) changeRole(w http.ResponseWriter, r *http.Request) {
	h.memberMutate(w, r, func(orgID, userID int64) error {
		role := r.PostFormValue("role")
		return orgs.ChangeRole(r.Context(), h.deps(), orgID, userID, role)
	})
}

func (h *Handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	h.memberMutate(w, r, func(orgID, userID int64) error {
		return orgs.RemoveMember(r.Context(), h.deps(), orgID, userID)
	})
}

// memberMutate is the shared owner-check + redirect wrapper for the
// member-management POSTs. Centralizes the policy gate so each route
// is one line.
func (h *Handlers) memberMutate(w http.ResponseWriter, r *http.Request, action func(orgID, userID int64) error) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	// Suspended owners denied like non-owners (SR2 C4).
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	owner, _ := orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	if !owner {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	uid, err := parseUserIDParam(chi.URLParam(r, "userID"))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	if err := action(org.ID, uid); err != nil {
		h.d.Logger.WarnContext(r.Context(), "orgs: member mutation",
			"org", org.Slug, "user_id", uid, "error", err)
	}
	http.Redirect(w, r, "/"+org.Slug+"/people", http.StatusSeeOther)
}

// ─── invitations ───────────────────────────────────────────────────

func (h *Handlers) invitationView(w http.ResponseWriter, r *http.Request) {
	tok := chi.URLParam(r, "token")
	inv, err := orgs.LookupInvitationByToken(r.Context(), h.deps(), tok)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	org, err := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, inv.OrgID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/invitation", map[string]any{
		"Title":      "Organization invitation",
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Org":        org,
		"Invitation": inv,
		"Token":      tok,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/invitation", "error", err)
	}
}

func (h *Handlers) invitationAccept(w http.ResponseWriter, r *http.Request) {
	h.invitationAction(w, r, true)
}

func (h *Handlers) invitationDecline(w http.ResponseWriter, r *http.Request) {
	h.invitationAction(w, r, false)
}

func (h *Handlers) invitationAction(w http.ResponseWriter, r *http.Request, accept bool) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
		return
	}
	// Suspended users can't act on invitations either way (SR2 C4).
	// Joining an org while suspended would let them participate in
	// org-scoped actions; declining is harmless but the consistent
	// gate makes the surface uniform.
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	tok := chi.URLParam(r, "token")
	inv, err := orgs.LookupInvitationByToken(r.Context(), h.deps(), tok)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if accept {
		if err := orgs.AcceptInvitation(r.Context(), h.deps(), inv, viewer.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: accept invitation",
				"id", inv.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
			return
		}
	} else {
		if err := orgs.DeclineInvitation(r.Context(), h.deps(), inv, viewer.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: decline invitation",
				"id", inv.ID, "error", err)
		}
	}
	org, _ := orgsdb.New().GetOrgByID(r.Context(), h.d.Pool, inv.OrgID)
	http.Redirect(w, r, "/"+org.Slug, http.StatusSeeOther)
}

// friendlyOrgErr maps orchestrator errors to user-facing strings.
// Unknown errors collapse to a generic message — the underlying err
// is logged at the call site.
func friendlyOrgErr(err error) string {
	switch {
	case errors.Is(err, orgs.ErrEmptySlug):
		return "Slug is required."
	case errors.Is(err, orgs.ErrSlugTooLong):
		return "Slug too long (max 39 characters)."
	case errors.Is(err, orgs.ErrSlugInvalid):
		return "Slug must be lowercase letters, digits, or hyphens; cannot start or end with a hyphen."
	case errors.Is(err, orgs.ErrSlugReserved):
		return "That slug is reserved. Try another."
	case errors.Is(err, orgs.ErrSlugTaken):
		return "That slug is already in use. Try another."
	}
	return "Something went wrong creating the organization."
}
