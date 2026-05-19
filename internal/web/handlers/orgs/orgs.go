// SPDX-License-Identifier: AGPL-3.0-or-later

// Package orgs wires the S30 organization web surface:
//
//	GET  /organizations/plan           plan selection
//	GET  /organizations/new            create form
//	POST /organizations                create submit
//	GET  /orgs/{org}/repositories                          repository list
//	GET  /{org}/security                                   security overview
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
//	GET  /invitations                                       logged-in invite inbox
//	POST /invitations/id/{invitationID}/accept              accept from inbox
//	POST /invitations/id/{invitationID}/decline             decline from inbox
//
// Profile rendering for /{org} is dispatched from the existing
// /{username} catch-all in internal/web/handlers/profile via the
// principals.Resolve lookup; this handler set only owns the org-
// specific surfaces.
package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	authemail "github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps wires the handler set.
type Deps struct {
	Logger                *slog.Logger
	Render                *render.Renderer
	Pool                  *pgxpool.Pool
	EmailSender           authemail.Sender
	EmailFrom             string
	SiteName              string
	BaseURL               string
	ObjectStore           storage.ObjectStore
	SecretBox             *secretbox.Box
	Audit                 *audit.Recorder
	BillingEnabled        bool
	BillingGracePeriod    time.Duration
	Stripe                stripebilling.Remote
	StripeSuccessURL      string
	StripeCancelURL       string
	StripePortalReturnURL string
	// PRO04: price IDs surface to the webhook handler for cross-kind
	// misroute guarding. Wiring populates these from the same config
	// that constructs the Stripe client; either may be empty when the
	// operator has enabled only one tier.
	StripeTeamPriceID string
	StripeProPriceID  string
}

// BillingPriceIDs returns the configured (team, pro) Stripe price
// IDs. The webhook handler uses these to refuse cross-kind
// misroutes (Pro price on an org subject or Team on a user).
func (d Deps) BillingPriceIDs() (team, pro string) {
	return d.StripeTeamPriceID, d.StripeProPriceID
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

// MountCreate registers /organizations/plan, /organizations/new, POST /organizations, and
// organization settings routes under /organizations/{org}/settings/*.
// Caller wraps these in RequireUser since they require a logged-in
// actor. The /organizations prefix is on the auth-reserved list so it
// never shadows a user/org slug.
func (h *Handlers) MountCreate(r chi.Router) {
	r.Get("/organizations/plan", h.planSelection)
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
	r.Get("/organizations/{org}/settings/scheduled-reminders", h.settingsScheduledReminders)
	r.Post("/organizations/{org}/settings/scheduled-reminders", h.settingsScheduledReminderCreate)
	r.Post("/organizations/{org}/settings/scheduled-reminders/{reminderID}", h.settingsScheduledReminderUpdate)
	r.Post("/organizations/{org}/settings/scheduled-reminders/{reminderID}/pause", h.settingsScheduledReminderPause)
	r.Post("/organizations/{org}/settings/scheduled-reminders/{reminderID}/resume", h.settingsScheduledReminderResume)
	r.Post("/organizations/{org}/settings/scheduled-reminders/{reminderID}/delete", h.settingsScheduledReminderDelete)
	r.Get("/organizations/{org}/settings/secrets/actions", h.settingsActionsSecrets)
	r.Post("/organizations/{org}/settings/secrets/actions", h.settingsActionsSecretSet)
	r.Post("/organizations/{org}/settings/secrets/actions/{name}/delete", h.settingsActionsSecretDelete)
	r.Get("/organizations/{org}/settings/variables/actions", h.settingsActionsVariables)
	r.Post("/organizations/{org}/settings/variables/actions", h.settingsActionsVariableSet)
	r.Post("/organizations/{org}/settings/variables/actions/{name}/delete", h.settingsActionsVariableDelete)
	if h.billingConfigured() {
		r.Get("/organizations/{org}/settings/billing", h.settingsBilling)
		r.Get("/organizations/{org}/settings/billing/licensing", h.settingsBillingLicensing)
		r.Get("/organizations/{org}/settings/billing/seats/add", h.billingSeatsAdd)
		r.Post("/organizations/{org}/settings/billing/seats/add", h.billingSeatsAddSubmit)
		r.Get("/organizations/{org}/settings/billing/seats/remove", h.billingSeatsRemove)
		r.Post("/organizations/{org}/settings/billing/seats/remove", h.billingSeatsRemoveSubmit)
		r.Post("/organizations/{org}/billing/checkout", h.billingCheckout)
		r.Post("/organizations/{org}/billing/portal", h.billingPortal)
		r.Post("/organizations/{org}/billing/seat-drift/repair", h.billingSeatDriftRepair)
		r.Post("/organizations/{org}/billing/quota-overrides", h.billingQuotaOverrideSave)
		r.Post("/organizations/{org}/billing/quota-overrides/delete", h.billingQuotaOverrideDelete)
		r.Get("/organizations/{org}/billing/success", h.billingSuccess)
		r.Get("/organizations/{org}/billing/cancel", h.billingCancel)
	}
}

// MountOrgRoutes registers the per-org surface under /{org}/people
// and /{org}/settings. Caller MUST register this before the
// /{username} catch-all so the `people` segment matches.
//
// Member-management routes live behind RequireUser at the wiring
// layer (server.go); profile-style reads stay public.
func (h *Handlers) MountOrgRoutes(r chi.Router) {
	r.Get("/{org}/security", h.securityOverview)
	r.Get("/{org}/people", h.peoplePage)
	r.Post("/{org}/people/invite", h.invite)
	r.Post("/{org}/people/{userID}/role", h.changeRole)
	r.Post("/{org}/people/{userID}/remove", h.removeMember)
	h.MountTeams(r)
}

// MountInvitations registers the logged-in invitation inbox plus
// token-backed email invite accept/decline routes.
func (h *Handlers) MountInvitations(r chi.Router) {
	r.Get("/invitations", h.invitationsInbox)
	r.Post("/invitations/id/{invitationID}/accept", h.invitationAcceptByID)
	r.Post("/invitations/id/{invitationID}/decline", h.invitationDeclineByID)
	r.Get("/invitations/{token}", h.invitationView)
	r.Post("/invitations/{token}/accept", h.invitationAccept)
	r.Post("/invitations/{token}/decline", h.invitationDecline)
}

func (h *Handlers) MountBillingWebhook(r chi.Router) {
	if !h.billingConfigured() {
		return
	}
	r.Post("/stripe/webhook", h.billingWebhook)
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

func (h *Handlers) billingConfigured() bool {
	return h.d.BillingEnabled && h.d.Stripe != nil
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

func (h *Handlers) planSelection(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next=/organizations/plan", http.StatusSeeOther)
		return
	}
	h.renderPlanSelection(w, r, "")
}

func (h *Handlers) newForm(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next=/organizations/new", http.StatusSeeOther)
		return
	}
	requestedPlan := requestedOrgCreatePlan(r.URL.Query().Get("plan"))
	if h.billingConfigured() && requestedPlan == "" {
		h.renderPlanSelection(w, r, "")
		return
	}
	plan := normalizeOrgCreatePlan(requestedPlan, h.billingConfigured())
	if plan == orgCreatePlanEnterprise {
		h.renderPlanSelection(w, r, "Enterprise organizations are contact-sales only today.")
		return
	}
	form := orgCreateForm{SelectedTier: plan}
	if plan == orgCreatePlanTeam {
		form.SeatCount = teamSeatCountFromQuery(r.URL.Query())
	}
	h.renderNewForm(w, r, form, "")
}

type orgCreateForm struct {
	SelectedTier string
	Slug         string
	DisplayName  string
	BillingEmail string
	SeatCount    string
	GitHubOrg    string
	GitHubToken  string
	AcceptTerms  bool
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
		SelectedTier: normalizeOrgCreatePlan(r.PostFormValue("plan"), h.billingConfigured()),
		Slug:         strings.TrimSpace(r.PostFormValue("slug")),
		DisplayName:  strings.TrimSpace(r.PostFormValue("display_name")),
		BillingEmail: strings.TrimSpace(r.PostFormValue("billing_email")),
		SeatCount:    strings.TrimSpace(r.PostFormValue("seat_count")),
		GitHubOrg:    strings.TrimSpace(r.PostFormValue("github_org")),
		GitHubToken:  strings.TrimSpace(r.PostFormValue("github_token")),
		AcceptTerms:  r.PostFormValue("accept_terms") != "",
	}
	if form.SelectedTier == orgCreatePlanEnterprise {
		h.renderPlanSelection(w, r, "Enterprise organizations are contact-sales only today.")
		return
	}
	if !form.AcceptTerms {
		h.renderNewForm(w, r, form.withoutToken(), "You must accept the terms to create an organization.")
		return
	}
	seatCount := 0
	if form.SelectedTier == orgCreatePlanTeam {
		var err error
		seatCount, err = parseTeamSeatCount(form.SeatCount)
		if err != nil {
			h.renderNewForm(w, r, form.withoutToken(), "Choose at least 1 licensed seat for Team.")
			return
		}
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
		if form.SelectedTier == orgCreatePlanTeam && h.billingConfigured() {
			h.redirectToTeamCheckout(w, r, row, seatCount)
			return
		}
		http.Redirect(w, r, "/organizations/"+row.Slug+"/imports/"+strconv.FormatInt(imp.ID, 10), http.StatusSeeOther)
		return
	}
	if form.SelectedTier == orgCreatePlanTeam && h.billingConfigured() {
		h.redirectToTeamCheckout(w, r, row, seatCount)
		return
	}
	http.Redirect(w, r, "/"+row.Slug, http.StatusSeeOther)
}

func (h *Handlers) redirectToTeamCheckout(w http.ResponseWriter, r *http.Request, org orgsdb.Org, seatCount int) {
	sessionURL, err := h.startBillingCheckout(r, org, seatCount)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: start team checkout after create", "org_id", org.ID, "error", err)
		http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=team-checkout-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, sessionURL, http.StatusSeeOther)
}

func (f orgCreateForm) withoutToken() orgCreateForm {
	f.GitHubToken = ""
	return f
}

func (h *Handlers) renderNewForm(w http.ResponseWriter, r *http.Request, form orgCreateForm, errMsg string) {
	seatPreview := orgCreateTeamSeatPreview(form.SeatCount)
	if err := h.d.Render.RenderPage(w, r, "orgs/new", map[string]any{
		"Title":        orgCreateTitle(form.SelectedTier),
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"Slug":         form.Slug,
		"Form":         form,
		"SeatPreview":  seatPreview,
		"OrgURLPrefix": orgCreateURLPrefix(r, h.d.BaseURL),
		"Error":        errMsg,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/new", "error", err)
	}
}

func orgCreateURLPrefix(r *http.Request, configured string) string {
	configured = strings.TrimRight(strings.TrimSpace(configured), "/")
	if configured != "" {
		return configured
	}
	scheme := "https"
	if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")) {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

const (
	orgCreatePlanFree       = "free"
	orgCreatePlanTeam       = "team"
	orgCreatePlanEnterprise = "enterprise"
)

func requestedOrgCreatePlan(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case orgCreatePlanFree, orgCreatePlanTeam, orgCreatePlanEnterprise:
		return strings.ToLower(strings.TrimSpace(raw))
	case "business":
		return orgCreatePlanTeam
	default:
		return ""
	}
}

func normalizeOrgCreatePlan(raw string, billingConfigured bool) string {
	switch requestedOrgCreatePlan(raw) {
	case orgCreatePlanTeam:
		if billingConfigured {
			return orgCreatePlanTeam
		}
	case orgCreatePlanEnterprise:
		if billingConfigured {
			return orgCreatePlanEnterprise
		}
	}
	return orgCreatePlanFree
}

func orgCreateTitle(plan string) string {
	if plan == orgCreatePlanTeam {
		return "Set up your organization"
	}
	return "New organization"
}

const (
	teamSeatMonthlyPriceUSD = 4
	defaultTeamSignupSeats  = 5
)

type orgCreateSeatPreview struct {
	Count     int
	CountText string
	TotalText string
	SeatNoun  string
}

type peopleNotice struct {
	Message    string
	ActionText string
	ActionHref string
}

func orgCreateTeamSeatPreview(raw string) orgCreateSeatPreview {
	count, err := parseTeamSeatCount(raw)
	if err != nil {
		count = 1
	}
	seatNoun := "seat"
	if count != 1 {
		seatNoun = "seats"
	}
	return orgCreateSeatPreview{
		Count:     count,
		CountText: strconv.Itoa(count),
		TotalText: fmt.Sprintf("$%d", count*teamSeatMonthlyPriceUSD),
		SeatNoun:  seatNoun,
	}
}

func parseTeamSeatCount(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultTeamSignupSeats, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, fmt.Errorf("seat count must be at least 1")
	}
	return n, nil
}

func teamSeatCountFromQuery(values url.Values) string {
	raw := values.Get("seat_count")
	count, err := parseTeamSeatCount(raw)
	if err != nil {
		count = defaultTeamSignupSeats
	}
	return strconv.Itoa(count)
}

func (h *Handlers) renderPlanSelection(w http.ResponseWriter, r *http.Request, errMsg string) {
	view := newOrgPlanSelectionView(h.billingConfigured())
	if err := h.d.Render.RenderPage(w, r, "orgs/new_plan", map[string]any{
		"Title":             "Pick a plan for your organization",
		"Error":             errMsg,
		"BillingConfigured": h.billingConfigured(),
		"PlanView":          view,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/new_plan", "error", err)
	}
}

type orgPlanSelectionView struct {
	DefaultSeats    int
	DefaultSeatText string
	PlanCards       []orgPlanCard
	FeatureSections []orgPlanFeatureSection
}

type orgPlanCard struct {
	Name        string
	Description string
	Price       string
	PriceSuffix string
	CTA         string
	Href        string
	Disabled    bool
	Featured    bool
	SeatRange   string
	Features    []orgPlanFeature
}

type orgPlanFeature struct {
	Title       string
	Description string
}

type orgPlanFeatureSection struct {
	Name string
	Rows []orgPlanFeatureRow
}

type orgPlanFeatureRow struct {
	Name        string
	Description string
	Free        string
	Team        string
	Enterprise  string
	Owner       string
	OwnerPath   string
	State       string
}

func newOrgPlanSelectionView(billingConfigured bool) orgPlanSelectionView {
	teamHref := "/organizations/new?plan=team&seat_count=" + strconv.Itoa(defaultTeamSignupSeats)
	if !billingConfigured {
		teamHref = ""
	}
	return orgPlanSelectionView{
		DefaultSeats:    defaultTeamSignupSeats,
		DefaultSeatText: strconv.Itoa(defaultTeamSignupSeats),
		PlanCards: []orgPlanCard{
			{
				Name:        "Free",
				Description: "The basics for individuals and organizations",
				Price:       "$0",
				PriceSuffix: "USD per user/month",
				CTA:         "Create a free organization",
				Href:        "/organizations/new?plan=free&seat_count=1",
				SeatRange:   "Recommended for 1-4 seats",
				Features: []orgPlanFeature{
					{Title: "Public and private repositories", Description: "Host code and collaborate without a card."},
					{Title: "Basic issue, pull request, and repository workflows", Description: "The core forge loop stays available to Free organizations."},
					{Title: "Basic branch protection", Description: "Use the baseline protections already shipped for org repositories."},
					{Title: "Community support", Description: "Use public documentation and instance support channels."},
				},
			},
			{
				Name:        "Team",
				Description: "Advanced collaboration for organizations",
				Price:       "$4",
				PriceSuffix: "USD per licensed seat/month",
				CTA:         "Continue with Team",
				Href:        teamHref,
				Disabled:    !billingConfigured,
				Featured:    true,
				SeatRange:   "Recommended for 5-10 seats",
				Features: []orgPlanFeature{
					{Title: "Everything included in Free, plus...", Description: "Paid capacity and controls for private organization work."},
					{Title: "Licensed seats", Description: "Buy the number of organization seats you need before inviting members."},
					{Title: "Private-repo governance controls", Description: "Team-only rules are enforced through the entitlement layer as they ship."},
					{Title: "Billing and licensing settings", Description: "Owners manage plan state, invoices, and seat changes from shithub."},
				},
			},
			{
				Name:        "Enterprise",
				Description: "Security, compliance, and flexible deployment",
				Price:       "$21",
				PriceSuffix: "USD starting point",
				CTA:         "Contact sales",
				Href:        "/organizations/new?plan=enterprise",
				SeatRange:   "Recommended for 11+ seats",
				Features: []orgPlanFeature{
					{Title: "Everything included in Team, plus...", Description: "Enterprise remains a contact-sales planning surface in v1."},
					{Title: "Compliance and identity planning", Description: "SAML, SCIM, managed users, and contracts are not self-serve yet."},
					{Title: "Custom support expectations", Description: "Use the contact-sales path for needs beyond Team."},
				},
			},
		},
		FeatureSections: orgPlanFeatureSections(),
	}
}

func orgPlanFeatureSections() []orgPlanFeatureSection {
	return []orgPlanFeatureSection{
		{
			Name: "Code management",
			Rows: []orgPlanFeatureRow{
				{
					Name: "Public repositories", Description: "Host open-source projects in public organization repositories.",
					Free: "Unlimited", Team: "Unlimited", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Shipped baseline",
				},
				{
					Name: "Private repositories", Description: "Host private organization repositories and collaborate with controlled access.",
					Free: "Included", Team: "Included", Enterprise: "Contact sales", Owner: "SP18",
					OwnerPath: ".docs/sprints/PAYMENTS/SP18-private-repo-governance-rules.md", State: "Baseline shipped",
				},
				{
					Name: "Branch and tag rules", Description: "Enforce branch restrictions, tag protection, and required status checks across organization repositories.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP18",
					OwnerPath: ".docs/sprints/PAYMENTS/SP18-private-repo-governance-rules.md", State: "Shipped",
				},
				{
					Name: "Code owners", Description: "Request reviews from responsible owners when protected files change.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP19",
					OwnerPath: ".docs/sprints/PAYMENTS/SP19-codeowners-team-reviewers-auto-assignment.md", State: "Shipped",
				},
				{
					Name: "Repository insights", Description: "Analyze Pulse, contributors, traffic, commits, code frequency, and fork network data.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP24",
					OwnerPath: ".docs/sprints/PAYMENTS/SP24-repository-insights-suite.md", State: "Shipped",
				},
			},
		},
		{
			Name: "Code workflow",
			Rows: []orgPlanFeatureRow{
				{
					Name: "Actions minutes", Description: "Run CI jobs with usage accounting and plan-aware quotas.",
					Free: "2,000 min/month", Team: "3,000 min/month", Enterprise: "Contact sales", Owner: "SP23",
					OwnerPath: ".docs/sprints/PAYMENTS/SP23-actions-environments-and-quota-parity.md", State: "Shipped",
				},
				{
					Name: "Environment deployment branches and secrets", Description: "Protect deployments with environment-scoped controls.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP23",
					OwnerPath: ".docs/sprints/PAYMENTS/SP23-actions-environments-and-quota-parity.md", State: "Shipped",
				},
				{
					Name: "Repository projects", Description: "Organize issues and pull requests with repository project views.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Shipped",
				},
				{
					Name: "Wikis", Description: "Create long-form repository documentation pages.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Shipped",
				},
				{
					Name: "Pages", Description: "Publish static project sites from organization repositories.",
					Free: "Public repositories", Team: "Planned", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Planned",
				},
				{
					Name: "Packages storage", Description: "Host generic repository packages with plan-aware storage limits.",
					Free: "500 MB", Team: "2 GB", Enterprise: "Contact sales", Owner: "SP22",
					OwnerPath: ".docs/sprints/PAYMENTS/SP22-packages-product-and-storage-quotas.md", State: "Shipped baseline",
				},
			},
		},
		{
			Name: "Collaboration",
			Rows: []orgPlanFeatureRow{
				{
					Name: "Collaborators for public repositories", Description: "Invite collaborators to public organization repositories.",
					Free: "Unlimited", Team: "Unlimited", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Shipped baseline",
				},
				{
					Name: "Private organization collaborators", Description: "Collaborate privately with billing-aware limits and licensed seats.",
					Free: "Limited", Team: "Billed by licensed seat", Enterprise: "Contact sales", Owner: "SP06a",
					OwnerPath: ".docs/sprints/PAYMENTS/SP06a-private-collaboration-limits.md", State: "Shipped",
				},
				{
					Name: "Multiple reviewers in pull requests", Description: "Require more than one reviewer before private-repo changes merge.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP18",
					OwnerPath: ".docs/sprints/PAYMENTS/SP18-private-repo-governance-rules.md", State: "Shipped",
				},
				{
					Name: "Team pull request reviewers", Description: "Request teams for review and satisfy the request when a team member reviews.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP19",
					OwnerPath: ".docs/sprints/PAYMENTS/SP19-codeowners-team-reviewers-auto-assignment.md", State: "Shipped",
				},
				{
					Name: "Automatic code review assignment", Description: "Automatically request matching CODEOWNERS users and teams on pull request updates.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP19",
					OwnerPath: ".docs/sprints/PAYMENTS/SP19-codeowners-team-reviewers-auto-assignment.md", State: "Shipped",
				},
				{
					Name: "Multiple issue and pull request assignees", Description: "Assign more than one person to a private organization issue or pull request.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP21",
					OwnerPath: ".docs/sprints/PAYMENTS/SP21-pages-wikis-projects-collaboration.md", State: "Shipped",
				},
				{
					Name: "Scheduled reminders", Description: "Remind teams about pending pull requests.",
					Free: "Upgrade", Team: "Planned", Enterprise: "Contact sales", Owner: "SP20",
					OwnerPath: ".docs/sprints/PAYMENTS/SP20-scheduled-reminders.md", State: "Planned",
				},
			},
		},
		{
			Name: "Security and compliance",
			Rows: []orgPlanFeatureRow{
				{
					Name: "Security overview", Description: "See organization security posture and dependency risk in one place.",
					Free: "Upgrade", Team: "Included", Enterprise: "Contact sales", Owner: "SP25",
					OwnerPath: ".docs/sprints/PAYMENTS/SP25-security-overview-dependency-advisories.md", State: "Baseline shipped",
				},
				{
					Name: "Secret scanning and push protection", Description: "Detect exposed secrets and block risky pushes.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP26",
					OwnerPath: ".docs/sprints/PAYMENTS/SP26-secret-protection.md", State: "Baseline shipped",
				},
				{
					Name: "Code security", Description: "Import SARIF reports, review code scanning alerts, and run security campaigns.",
					Free: "Public repositories", Team: "Included", Enterprise: "Contact sales", Owner: "SP27",
					OwnerPath: ".docs/sprints/PAYMENTS/SP27-code-security.md", State: "Baseline shipped",
				},
				{
					Name: "Required 2FA and audit log", Description: "Set stronger organization security posture and review activity.",
					Free: "Upgrade", Team: "Planned", Enterprise: "Contact sales", Owner: "SP29",
					OwnerPath: ".docs/sprints/PAYMENTS/SP29-platform-security-compliance-integrations.md", State: "Planned",
				},
			},
		},
		{
			Name: "Support and deployment",
			Rows: []orgPlanFeatureRow{
				{
					Name: "Community support", Description: "Use public docs and community support channels.",
					Free: "Included", Team: "Included", Enterprise: "Contact sales", Owner: "SP30",
					OwnerPath: ".docs/sprints/PAYMENTS/SP30-standard-support-and-billing-ops.md", State: "Baseline shipped",
				},
				{
					Name: "Standard support", Description: "Billing-aware support intake for paying organizations.",
					Free: "Upgrade", Team: "Planned", Enterprise: "Contact sales", Owner: "SP30",
					OwnerPath: ".docs/sprints/PAYMENTS/SP30-standard-support-and-billing-ops.md", State: "Planned",
				},
				{
					Name: "Enterprise account, SAML, SCIM, and managed users", Description: "Enterprise identity and account management remain contact-sales only.",
					Free: "-", Team: "-", Enterprise: "Contact sales", Owner: "SP09",
					OwnerPath: ".docs/sprints/PAYMENTS/SP09-enterprise-stub.md", State: "Deferred",
				},
			},
		},
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
	var viewerPendingInvite *orgInvitationInboxRow
	isOwner := false
	if !viewer.IsAnonymous() {
		isOwner, _ = orgs.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
		if isOwner {
			pending, _ = q.ListPendingInvitationsForOrg(r.Context(), h.d.Pool, org.ID)
		}
		viewerInvites, err := h.listViewerPendingInvitations(r.Context(), viewer.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: list viewer invitations", "user_id", viewer.ID, "error", err)
		}
		for i := range viewerInvites {
			if viewerInvites[i].OrgID == org.ID {
				viewerPendingInvite = &viewerInvites[i]
				break
			}
		}
	}
	navCounts := h.orgNavCounts(r.Context(), org.ID, -1)
	notice := peopleNoticeMessage(r.URL.Query().Get("notice"), org.Slug, h.billingConfigured())
	// PRO-EXT_SR2-15: collect Pro-member set for the badge.
	proUsernames := make(map[string]bool, len(filteredMembers))
	for _, m := range filteredMembers {
		if orgbilling.IsProUserPlan(orgbilling.UserPlan(m.Plan)) {
			proUsernames[m.Username] = true
		}
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/people", map[string]any{
		"Title":               org.Slug + " · people",
		"CSRFToken":           middleware.CSRFTokenForRequest(r),
		"Org":                 org,
		"AvatarURL":           "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":        "people",
		"RepoCount":           navCounts.RepoCount,
		"Members":             filteredMembers,
		"ProUsernames":        proUsernames,
		"MemberCount":         navCounts.MemberCount,
		"TeamCount":           navCounts.TeamCount,
		"Pending":             pending,
		"ViewerPendingInvite": viewerPendingInvite,
		"PendingCount":        len(pending),
		"Query":               query,
		"HasQuery":            query != "",
		"IsOwner":             isOwner,
		"CanManagePeople":     isOwner,
		"Notice":              notice.Message,
		"NoticeActionText":    notice.ActionText,
		"NoticeActionHref":    notice.ActionHref,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/people", "error", err)
	}
}

func peopleNoticeMessage(code, orgSlug string, billingConfigured bool) peopleNotice {
	switch code {
	case "private-collab-upgrade":
		notice := peopleNotice{Message: "Free organizations can have up to 3 private collaborators. Upgrade to Team to add more private collaborators."}
		if billingConfigured {
			notice.ActionText = "Manage billing"
			notice.ActionHref = "/organizations/" + orgSlug + "/settings/billing"
		}
		return notice
	case "team-seat-limit":
		return peopleNotice{
			Message:    "This Team organization has no available seats for another member.",
			ActionText: "Add seats",
			ActionHref: orgBillingSeatAddPath(orgSlug),
		}
	default:
		return peopleNotice{}
	}
}

func splitInviteTarget(raw string) (username, email string) {
	target := strings.TrimSpace(raw)
	trimmedUser := strings.TrimLeft(target, "@")
	if strings.HasPrefix(target, "@") && !strings.Contains(trimmedUser, "@") {
		return trimmedUser, ""
	}
	if strings.Contains(target, "@") {
		return "", target
	}
	return target, ""
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
	if h.billingConfigured() {
		licenseState, err := orgbilling.GetTeamLicenseState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: team seat check before invite", "org_id", org.ID, "error", err)
		} else if orgbilling.IsTeamLicenseState(licenseState) && licenseState.AvailableSeats <= 0 {
			http.Redirect(w, r, "/"+org.Slug+"/people?notice=team-seat-limit", http.StatusSeeOther)
			return
		}
	}
	p.TargetUsername, p.TargetEmail = splitInviteTarget(target)
	if _, err := orgs.Invite(r.Context(), h.deps(), p); err != nil {
		h.d.Logger.WarnContext(r.Context(), "orgs: invite failed",
			"org", org.Slug, "target", target, "error", err)
		if errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
			http.Redirect(w, r, "/"+org.Slug+"/people?notice=private-collab-upgrade", http.StatusSeeOther)
			return
		}
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
		if errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
			http.Redirect(w, r, "/"+org.Slug+"/people?notice=private-collab-upgrade", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/"+org.Slug+"/people", http.StatusSeeOther)
}

// ─── invitations ───────────────────────────────────────────────────

type orgInvitationInboxRow struct {
	ID             int64
	OrgID          int64
	OrgSlug        string
	OrgDisplayName string
	Role           orgsdb.OrgRole
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

func (h *Handlers) invitationsInbox(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	invites, err := h.listViewerPendingInvitations(r.Context(), viewer.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: list invitations inbox", "user_id", viewer.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/invitations", map[string]any{
		"Title":       "Organization invitations",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"Invitations": invites,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "orgs: render", "tpl", "orgs/invitations", "error", err)
	}
}

func (h *Handlers) listViewerPendingInvitations(ctx context.Context, userID int64) ([]orgInvitationInboxRow, error) {
	q := orgsdb.New()
	seen := map[int64]bool{}
	invites := []orgInvitationInboxRow{}
	userRows, err := q.ListPendingInvitationsForUser(ctx, h.d.Pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return nil, err
	}
	for _, row := range userRows {
		if seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		invites = append(invites, orgInvitationInboxRow{
			ID:             row.ID,
			OrgID:          row.OrgID,
			OrgSlug:        row.OrgSlug,
			OrgDisplayName: row.OrgDisplayName,
			Role:           row.Role,
			CreatedAt:      pgTime(row.CreatedAt),
			ExpiresAt:      pgTime(row.ExpiresAt),
		})
	}
	emails, err := usersdb.New().ListUserEmailsForUser(ctx, h.d.Pool, userID)
	if err != nil {
		return nil, err
	}
	for _, email := range emails {
		if !email.Verified {
			continue
		}
		emailRows, err := q.ListPendingInvitationsForEmail(ctx, h.d.Pool, pgtype.Text{String: strings.ToLower(email.Email), Valid: true})
		if err != nil {
			return nil, err
		}
		for _, row := range emailRows {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			invites = append(invites, orgInvitationInboxRow{
				ID:             row.ID,
				OrgID:          row.OrgID,
				OrgSlug:        row.OrgSlug,
				OrgDisplayName: row.OrgDisplayName,
				Role:           row.Role,
				CreatedAt:      pgTime(row.CreatedAt),
				ExpiresAt:      pgTime(row.ExpiresAt),
			})
		}
	}
	sort.SliceStable(invites, func(i, j int) bool {
		return invites[i].CreatedAt.After(invites[j].CreatedAt)
	})
	return invites, nil
}

func pgTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

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

func (h *Handlers) invitationAcceptByID(w http.ResponseWriter, r *http.Request) {
	h.invitationActionByID(w, r, true)
}

func (h *Handlers) invitationDeclineByID(w http.ResponseWriter, r *http.Request) {
	h.invitationActionByID(w, r, false)
}

func (h *Handlers) invitationActionByID(w http.ResponseWriter, r *http.Request, accept bool) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}
	invitationID, err := strconv.ParseInt(chi.URLParam(r, "invitationID"), 10, 64)
	if err != nil || invitationID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	inv, err := orgsdb.New().GetOrgInvitationByID(r.Context(), h.d.Pool, invitationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "orgs: lookup invitation by id", "id", invitationID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.performInvitationAction(w, r, inv, accept, "/invitations")
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
	h.performInvitationAction(w, r, inv, accept, "")
}

func (h *Handlers) performInvitationAction(w http.ResponseWriter, r *http.Request, inv orgsdb.OrgInvitation, accept bool, declineRedirect string) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if accept {
		if err := orgs.AcceptInvitation(r.Context(), h.deps(), inv, viewer.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: accept invitation",
				"id", inv.ID, "error", err)
			if errors.Is(err, entitlements.ErrPrivateCollaborationLimitExceeded) {
				h.d.Render.HTTPError(w, r, http.StatusPaymentRequired, "")
				return
			}
			if errors.Is(err, orgs.ErrUnauthorizedAcceptor) ||
				errors.Is(err, orgs.ErrInvitationExpired) ||
				errors.Is(err, orgs.ErrInvitationConsumed) {
				h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
				return
			}
			h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
			return
		}
	} else {
		if err := orgs.DeclineInvitation(r.Context(), h.deps(), inv, viewer.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "orgs: decline invitation",
				"id", inv.ID, "error", err)
			if errors.Is(err, orgs.ErrUnauthorizedAcceptor) ||
				errors.Is(err, orgs.ErrInvitationExpired) ||
				errors.Is(err, orgs.ErrInvitationConsumed) {
				h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
				return
			}
		}
		if declineRedirect != "" {
			http.Redirect(w, r, declineRedirect, http.StatusSeeOther)
			return
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
