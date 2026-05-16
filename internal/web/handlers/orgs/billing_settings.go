// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	orgdomain "github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type billingSummaryItem struct {
	Label  string
	Value  string
	Detail string
}

type billingInvoiceView struct {
	Number           string
	StatusLabel      string
	StatusClass      string
	AmountLabel      string
	PeriodLabel      string
	DueLabel         string
	HostedInvoiceURL string
	InvoicePDFURL    string
}

type billingSeatBreakdown struct {
	UsedSeats      int
	LicensedSeats  int64
	AvailableSeats int64
	PendingInvites int
	SnapshotLabel  string
}

type billingSeatConsumerView struct {
	Username     string
	DisplayName  string
	AvatarURL    string
	RoleLabel    string
	JoinedLabel  string
	StatusLabel  string
	StatusDetail string
}

type billingPendingSeatInviteView struct {
	Target       string
	RoleLabel    string
	InvitedBy    string
	CreatedLabel string
	StatusLabel  string
	StatusDetail string
}

type billingSeatActionMenu struct {
	CanManage    bool
	CanAddSeats  bool
	CanRemove    bool
	AddHref      string
	RemoveHref   string
	DisabledNote string
}

type billingSeatChangeForm struct {
	Mode               string
	Title              string
	Heading            string
	Description        string
	Action             string
	InputName          string
	InputLabel         string
	InputHelp          string
	SubmitLabel        string
	CurrentSeats       int
	UsedSeats          int
	AvailableSeats     int
	ChangeSeats        int
	NewTotal           int
	ProrationLabel     string
	NextBillLabel      string
	MonthlyDeltaLabel  string
	CanSubmit          bool
	SubscriptionStatus string
}

type billingPrivateCollaborationBreakdown struct {
	Count      int64
	LimitLabel string
	Detail     string
}

type billingUsageBreakdown struct {
	Available       bool
	PeriodLabel     string
	CalculatedLabel string
	Alert           billingAlert
	Rows            []billingUsageRow
}

type billingUsageRow struct {
	Key          string
	Label        string
	UsedLabel    string
	LimitLabel   string
	PercentLabel string
	PercentValue int64
	Detail       string
	StatusClass  string
}

type billingAlert struct {
	Class      string
	Message    string
	ActionText string
	ActionHref string
}

type billingQuotaOverrideView struct {
	Kind      string
	Limit     string
	Note      string
	UpdatedAt string
}

var errSeatChangeUnavailable = errors.New("org billing: seat changes unavailable")

type billingQuotaOverrideForm struct {
	KindValue  string
	KindLabel  string
	LimitValue string
	Unlimited  bool
	Note       string
	UnitLabel  string
	Help       string
}

type billingDebugView struct {
	StripeCustomerID         string
	StripeSubscriptionID     string
	StripeSubscriptionItemID string
	LastWebhookEventID       string
	LastWebhookEventType     string
	LastWebhookStatus        string
	LastWebhookReceivedAt    string
	LastWebhookProcessedAt   string
	LastWebhookAttempts      int32
	LastWebhookError         string
	QuotaOverrides           []billingQuotaOverrideView
	QuotaOverrideForms       []billingQuotaOverrideForm
	SeatDrift                billingSeatDriftView
}

type billingSeatDriftView struct {
	Available          bool
	Detected           bool
	CanRepair          bool
	LocalLicensedSeats int64
	StripeSeats        int64
	UsedSeats          int
	Status             string
	Detail             string
}

func (h *Handlers) settingsBilling(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.loadOrgBillingSettingsViewer(w, r)
	if !ok {
		return
	}
	h.renderSettingsBilling(w, r, org, "", billingNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsBillingLicensing(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.loadOrgBillingSettingsViewer(w, r)
	if !ok {
		return
	}
	h.renderBillingLicensing(w, r, org, "", billingLicensingNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) billingSeatsAdd(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderBillingSeatChange(w, r, org, "add", 1, "", "")
}

func (h *Handlers) billingSeatsAddSubmit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	added, err := parsePositiveSeatDelta(r.PostFormValue("additional_seats"))
	if err != nil {
		h.renderBillingSeatChange(w, r, org, "add", 1, "Enter at least 1 seat to add.", "")
		return
	}
	if err := h.applyTeamSeatChange(r, org, added, "add"); err != nil {
		h.renderBillingSeatChange(w, r, org, "add", added, billingSeatChangeError(err, "add"), "")
		return
	}
	http.Redirect(w, r, orgBillingLicensingPath(org.Slug)+"?notice=seats-added", http.StatusSeeOther)
}

func (h *Handlers) billingSeatsRemove(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderBillingSeatChange(w, r, org, "remove", 1, "", "")
}

func (h *Handlers) billingSeatsRemoveSubmit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	removed, err := parsePositiveSeatDelta(r.PostFormValue("remove_seats"))
	if err != nil {
		h.renderBillingSeatChange(w, r, org, "remove", 1, "Enter at least 1 seat to remove.", "")
		return
	}
	if err := h.applyTeamSeatChange(r, org, removed, "remove"); err != nil {
		h.renderBillingSeatChange(w, r, org, "remove", removed, billingSeatChangeError(err, "remove"), "")
		return
	}
	http.Redirect(w, r, orgBillingLicensingPath(org.Slug)+"?notice=seats-removed", http.StatusSeeOther)
}

func (h *Handlers) billingCheckout(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.billingConfigured() {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	seatCount, err := h.checkoutSeatCountFromRequest(r, org.ID)
	if err != nil {
		h.renderSettingsBilling(w, r, org, "Choose at least 1 licensed seat for Team.", "")
		return
	}
	sessionURL, err := h.startBillingCheckout(r, org, seatCount)
	if err != nil {
		metrics.BillingCheckoutSessionsTotal.WithLabelValues("org", "failure").Inc()
		h.d.Logger.ErrorContext(r.Context(), "org billing: create checkout", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not start checkout right now.", "")
		return
	}
	metrics.BillingCheckoutSessionsTotal.WithLabelValues("org", "success").Inc()
	http.Redirect(w, r, sessionURL, http.StatusSeeOther)
}

func (h *Handlers) checkoutSeatCountFromRequest(r *http.Request, orgID int64) (int, error) {
	raw := strings.TrimSpace(r.PostFormValue("seat_count"))
	if raw == "" {
		return orgbilling.DefaultTeamCheckoutLicensedSeats(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgID)
	}
	return parseTeamSeatCount(raw)
}

func (h *Handlers) startBillingCheckout(r *http.Request, org orgsdb.Org, seatCount int) (string, error) {
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		return "", fmt.Errorf("load billing state: %w", err)
	}
	state, err = h.ensureStripeCustomer(r, org, state)
	if err != nil {
		return "", fmt.Errorf("ensure stripe customer: %w", err)
	}
	if _, err := orgbilling.RecordPendingTeamCheckoutSeats(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, seatCount); err != nil {
		return "", fmt.Errorf("record pending team seats: %w", err)
	}
	session, err := h.d.Stripe.CreateCheckoutSession(r.Context(), stripebilling.CheckoutInput{
		OrgID:      org.ID,
		OrgSlug:    org.Slug,
		CustomerID: state.StripeCustomerID.String,
		SeatCount:  int64(seatCount),
		SuccessURL: h.billingReturnURL(org.Slug, h.d.StripeSuccessURL, "/organizations/"+org.Slug+"/billing/success"),
		CancelURL:  h.billingReturnURL(org.Slug, h.d.StripeCancelURL, "/organizations/"+org.Slug+"/billing/cancel"),
	})
	if err != nil {
		return "", fmt.Errorf("create stripe checkout session: %w", err)
	}
	return session.URL, nil
}

func (h *Handlers) billingPortal(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if !h.billingConfigured() {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: load state for portal", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !state.StripeCustomerID.Valid || strings.TrimSpace(state.StripeCustomerID.String) == "" {
		h.renderSettingsBilling(w, r, org, "Billing portal is unavailable until this organization has a Stripe customer record.", "")
		return
	}
	session, err := h.d.Stripe.CreatePortalSession(r.Context(), stripebilling.PortalInput{
		CustomerID: state.StripeCustomerID.String,
		ReturnURL:  h.billingReturnURL(org.Slug, h.d.StripePortalReturnURL, orgBillingSettingsPath(org.Slug)),
	})
	if err != nil {
		metrics.BillingPortalSessionsTotal.WithLabelValues("org", "failure").Inc()
		h.d.Logger.ErrorContext(r.Context(), "org billing: create portal session", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not open the Stripe billing portal right now.", "")
		return
	}
	metrics.BillingPortalSessionsTotal.WithLabelValues("org", "success").Inc()
	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

func (h *Handlers) billingSeatDriftRepair(w http.ResponseWriter, r *http.Request) {
	org, viewer, ok := h.loadOrgBillingSettingsViewer(w, r)
	if !ok {
		return
	}
	if !viewer.IsSiteAdmin {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	state, licenseState, ok := h.loadBillingLicenseState(w, r, org)
	if !ok {
		return
	}
	drift := h.billingSeatDriftView(r, state, licenseState)
	if !drift.Available {
		h.renderSettingsBilling(w, r, org, drift.Detail, "")
		return
	}
	if !drift.Detected {
		http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=seat-drift-none", http.StatusSeeOther)
		return
	}
	if drift.StripeSeats < int64(licenseState.UsedSeats) {
		h.renderSettingsBilling(w, r, org, "Stripe currently reports fewer seats than this organization is using. Add seats in Stripe first, then repair local state.", "")
		return
	}
	if drift.StripeSeats < 1 || drift.StripeSeats > maxIntValue() {
		h.renderSettingsBilling(w, r, org, "Stripe returned an invalid seat quantity. Fix the Stripe subscription item before repairing local state.", "")
		return
	}
	before := licenseState.LicensedSeats
	if _, err := orgbilling.SetTeamLicensedSeats(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, int(drift.StripeSeats), "stripe_repair"); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: repair seat drift", "org_id", org.ID, "stripe_seats", drift.StripeSeats, "error", err)
		h.renderSettingsBilling(w, r, org, billingSeatRepairError(err), "")
		return
	}
	h.recordBillingSeatDriftRepairAudit(r, viewer, org.ID, before, int(drift.StripeSeats), licenseState.UsedSeats, state)
	http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=seat-drift-repaired", http.StatusSeeOther)
}

func (h *Handlers) billingQuotaOverrideSave(w http.ResponseWriter, r *http.Request) {
	org, viewer, ok := h.loadOrgBillingSettingsViewer(w, r)
	if !ok {
		return
	}
	if !viewer.IsSiteAdmin {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	kind, ok := parseBillingQuotaKind(r.PostFormValue("kind"))
	if !ok {
		h.renderSettingsBilling(w, r, org, "Choose a supported quota override.", "")
		return
	}
	unlimited := r.PostFormValue("unlimited") != ""
	limit, err := parseBillingQuotaLimit(r.PostFormValue("limit_value"), unlimited)
	if err != nil {
		h.renderSettingsBilling(w, r, org, "Quota override limit must be a non-negative integer, or mark the quota as unlimited.", "")
		return
	}
	_, err = orgbilling.UpsertOrgQuotaOverride(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgbilling.QuotaOverrideInput{
		OrgID:           org.ID,
		Kind:            kind,
		LimitValue:      limit,
		Unlimited:       unlimited,
		Note:            r.PostFormValue("note"),
		CreatedByUserID: viewer.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: save quota override", "org_id", org.ID, "kind", kind, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not save quota override right now.", "")
		return
	}
	http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=quota-override-saved", http.StatusSeeOther)
}

func (h *Handlers) billingQuotaOverrideDelete(w http.ResponseWriter, r *http.Request) {
	org, viewer, ok := h.loadOrgBillingSettingsViewer(w, r)
	if !ok {
		return
	}
	if !viewer.IsSiteAdmin {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}
	kind, ok := parseBillingQuotaKind(r.PostFormValue("kind"))
	if !ok {
		h.renderSettingsBilling(w, r, org, "Choose a supported quota override.", "")
		return
	}
	if _, err := orgbilling.DeleteOrgQuotaOverride(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, kind); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: delete quota override", "org_id", org.ID, "kind", kind, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not clear quota override right now.", "")
		return
	}
	http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=quota-override-cleared", http.StatusSeeOther)
}

func (h *Handlers) billingSuccess(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderBillingResult(w, r, org, billingResultSuccess)
}

func (h *Handlers) billingCancel(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderBillingResult(w, r, org, billingResultCanceled)
}

const (
	billingResultSuccess  = "success"
	billingResultCanceled = "canceled"
)

func (h *Handlers) renderBillingResult(w http.ResponseWriter, r *http.Request, org orgsdb.Org, result string) {
	heading := "Checkout complete"
	seatCount, err := orgbilling.DefaultTeamCheckoutLicensedSeats(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: load pending checkout seats", "org_id", org.ID, "error", err)
		seatCount = 1
	}
	seatNoun := "seat"
	if seatCount != 1 {
		seatNoun = "seats"
	}
	message := fmt.Sprintf("Stripe accepted checkout for %d licensed %s. Team activation finishes after shithub receives and processes the signed Stripe webhook.", seatCount, seatNoun)
	if result == billingResultCanceled {
		heading = "Checkout canceled"
		message = fmt.Sprintf("No Team subscription was activated. The organization stays on Free; checkout can be resumed for %d licensed %s.", seatCount, seatNoun)
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/billing_result", map[string]any{
		"Title":            heading,
		"CSRFToken":        middleware.CSRFTokenForRequest(r),
		"Org":              org,
		"AvatarURL":        "/avatars/" + url.PathEscape(org.Slug),
		"Result":           result,
		"Heading":          heading,
		"Message":          message,
		"PendingSeatCount": seatCount,
		"BillingPath":      orgBillingSettingsPath(org.Slug),
	})
}

func (h *Handlers) loadOrgBillingSettingsViewer(w http.ResponseWriter, r *http.Request) (orgsdb.Org, middleware.CurrentUser, bool) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return orgsdb.Org{}, middleware.CurrentUser{}, false
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
		return orgsdb.Org{}, middleware.CurrentUser{}, false
	}
	if viewer.IsSuspended {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return orgsdb.Org{}, middleware.CurrentUser{}, false
	}
	if viewer.IsSiteAdmin {
		return org, viewer, true
	}
	owner, _ := orgdomain.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	if !owner {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return orgsdb.Org{}, middleware.CurrentUser{}, false
	}
	return org, viewer, true
}

func (h *Handlers) renderBillingLicensing(w http.ResponseWriter, r *http.Request, org orgsdb.Org, errMsg, notice string) {
	state, licenseState, ok := h.loadBillingLicenseState(w, r, org)
	if !ok {
		return
	}
	consumers, err := orgbilling.ListTeamSeatConsumers(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: list seat consumers", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	pending, err := orgsdb.New().ListPendingInvitationsForOrg(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: list pending invitations for licensing", "org_id", org.ID, "error", err)
		pending = nil
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	canManageBilling := false
	if !viewer.IsAnonymous() {
		canManageBilling, _ = orgdomain.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_billing_licensing", map[string]any{
		"Title":              org.Slug + " - billing and licensing",
		"CSRFToken":          middleware.CSRFTokenForRequest(r),
		"Org":                org,
		"AvatarURL":          "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":       "settings",
		"OrgSettingsActive":  "billing",
		"BillingEnabled":     h.d.BillingEnabled,
		"Error":              errMsg,
		"Notice":             notice,
		"BillingAlert":       billingAlertForState(state, org.Slug),
		"CurrentPlan":        billingPlanLabel(state.Plan),
		"SubscriptionStatus": billingStatusLabel(state.SubscriptionStatus),
		"Seats":              billingSeatBreakdown{UsedSeats: licenseState.UsedSeats, LicensedSeats: int64(licenseState.LicensedSeats), AvailableSeats: int64(licenseState.AvailableSeats), PendingInvites: len(pending), SnapshotLabel: billingSeatDetail(licenseState)},
		"SeatMenu":           billingSeatActionMenuForOrg(org.Slug, state, licenseState, canManageBilling),
		"Consumers":          billingSeatConsumerViews(consumers),
		"PendingInvites":     billingPendingSeatInviteViews(pending),
		"BillingPath":        orgBillingSettingsPath(org.Slug),
		"LicensingPath":      orgBillingLicensingPath(org.Slug),
	})
}

func (h *Handlers) renderBillingSeatChange(w http.ResponseWriter, r *http.Request, org orgsdb.Org, mode string, delta int, errMsg, notice string) {
	state, licenseState, ok := h.loadBillingLicenseState(w, r, org)
	if !ok {
		return
	}
	if !orgbilling.IsTeamState(state) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if delta < 1 {
		delta = 1
	}
	form := billingSeatChangeFormForOrg(org.Slug, mode, delta, state, licenseState)
	if mode == "remove" && licenseState.AvailableSeats <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if form.CanSubmit {
		preview, err := h.previewTeamSeatChange(r, state, licenseState, form.NewTotal, time.Now().UTC().Unix())
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "org billing: preview seat change", "org_id", org.ID, "mode", mode, "new_total", form.NewTotal, "error", err)
			form.CanSubmit = false
			if errMsg == "" {
				errMsg = "Could not preview this seat change with Stripe right now."
			}
		} else {
			applyTeamSeatPreviewLabels(&form, preview)
		}
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_billing_seats", map[string]any{
		"Title":             form.Title,
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"AvatarURL":         "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":      "settings",
		"OrgSettingsActive": "billing",
		"BillingEnabled":    h.d.BillingEnabled,
		"Error":             errMsg,
		"Notice":            notice,
		"BillingAlert":      billingAlertForState(state, org.Slug),
		"Form":              form,
		"BillingPath":       orgBillingSettingsPath(org.Slug),
		"LicensingPath":     orgBillingLicensingPath(org.Slug),
	})
}

func (h *Handlers) loadBillingLicenseState(w http.ResponseWriter, r *http.Request, org orgsdb.Org) (orgbilling.State, orgbilling.TeamLicenseState, bool) {
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: load state", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return orgbilling.State{}, orgbilling.TeamLicenseState{}, false
	}
	licenseState, err := orgbilling.GetTeamLicenseState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: load seat license state", "org_id", org.ID, "error", err)
		available := int(state.LicensedSeats) - int(state.UsedSeats)
		if available < 0 {
			available = 0
		}
		licenseState = orgbilling.TeamLicenseState{
			OrgID:          org.ID,
			Plan:           state.Plan,
			LicensedSeats:  int(state.LicensedSeats),
			UsedSeats:      int(state.UsedSeats),
			AvailableSeats: available,
			SeatSnapshotAt: state.SeatSnapshotAt,
		}
	}
	return state, licenseState, true
}

func (h *Handlers) renderSettingsBilling(w http.ResponseWriter, r *http.Request, org orgsdb.Org, errMsg, notice string) {
	state, licenseState, ok := h.loadBillingLicenseState(w, r, org)
	if !ok {
		return
	}
	pendingInviteCount, err := orgbilling.CountPendingOrgInvitations(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: count pending invitations", "org_id", org.ID, "error", err)
	}
	privateCollab := h.billingPrivateCollaborationBreakdown(r, org.ID)
	usage := h.billingUsageBreakdown(r, org.ID)
	invoices, err := orgbilling.ListInvoicesForOrg(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, 10)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: list invoices", "org_id", org.ID, "error", err)
		invoices = nil
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	canManageBilling := false
	if !viewer.IsAnonymous() {
		canManageBilling, _ = orgdomain.IsOwner(r.Context(), h.deps(), org.ID, viewer.ID)
	}
	debug := billingDebugView{}
	if viewer.IsSiteAdmin {
		debug = h.billingDebugView(r, org.ID, state, licenseState)
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_billing", map[string]any{
		"Title":                 org.Slug + " - billing and plans",
		"CSRFToken":             middleware.CSRFTokenForRequest(r),
		"Org":                   org,
		"AvatarURL":             "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":          "settings",
		"OrgSettingsActive":     "billing",
		"BillingEnabled":        h.d.BillingEnabled,
		"Error":                 errMsg,
		"Notice":                notice,
		"BillingAlert":          billingAlertForState(state, org.Slug),
		"Summary":               billingSummary(state, licenseState),
		"Seats":                 billingSeatBreakdown{UsedSeats: licenseState.UsedSeats, LicensedSeats: int64(licenseState.LicensedSeats), AvailableSeats: int64(licenseState.AvailableSeats), PendingInvites: pendingInviteCount, SnapshotLabel: billingSeatDetail(licenseState)},
		"PrivateCollaboration":  privateCollab,
		"Usage":                 usage,
		"CanUseBillingControls": canManageBilling,
		"CanStartCheckout":      canManageBilling && h.billingConfigured(),
		// Gate on StripeSubscriptionID, not StripeCustomerID. A
		// customer record exists from the moment a Checkout Session
		// is minted; the subscription id only lands after
		// customer.subscription.created. Gating on the customer id
		// surfaced "Manage or cancel" buttons for orgs that abandoned
		// checkout without paying.
		"CanManageSubscription": canManageBilling && h.billingConfigured() && state.StripeSubscriptionID.Valid && strings.TrimSpace(state.StripeSubscriptionID.String) != "",
		"GracePeriodLabel":      formatGracePeriod(h.d.BillingGracePeriod),
		"Invoices":              billingInvoiceViews(invoices),
		"IsSiteAdmin":           viewer.IsSiteAdmin,
		"Debug":                 debug,
	})
}

func (h *Handlers) billingPrivateCollaborationBreakdown(r *http.Request, orgID int64) billingPrivateCollaborationBreakdown {
	usage, err := entitlements.PrivateCollaborationUsageForOrg(r.Context(), entitlements.Deps{Pool: h.d.Pool}, orgID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: private collaboration usage", "org_id", orgID, "error", err)
		return billingPrivateCollaborationBreakdown{
			LimitLabel: "Unavailable",
			Detail:     "Private collaborator usage could not be calculated right now.",
		}
	}
	if usage.Unlimited {
		return billingPrivateCollaborationBreakdown{
			Count:      usage.Count,
			LimitLabel: "Unlimited",
			Detail:     "Team billing allows unlimited effective private collaborators while the subscription is active or in grace.",
		}
	}
	return billingPrivateCollaborationBreakdown{
		Count:      usage.Count,
		LimitLabel: fmt.Sprintf("%d", usage.Limit),
		Detail:     "Free organizations can add up to 3 unique people with effective access to private org repositories. Public collaboration is not counted.",
	}
}

func (h *Handlers) billingUsageBreakdown(r *http.Request, orgID int64) billingUsageBreakdown {
	entitlementSet, err := entitlements.ForOrg(r.Context(), entitlements.Deps{Pool: h.d.Pool}, orgID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: load usage entitlements", "org_id", orgID, "error", err)
		return unavailableBillingUsage("Usage limits could not be calculated right now.")
	}
	counters, err := orgbilling.GetOrgUsageCounters(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.WarnContext(r.Context(), "org billing: load usage counters", "org_id", orgID, "error", err)
		}
		return unavailableBillingUsage("Usage counters have not been recorded for this organization yet.")
	}
	storageLimit, err := entitlementSet.Limit(entitlements.LimitOrgStorageQuota)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: storage limit", "org_id", orgID, "error", err)
		return unavailableBillingUsage("Storage limits could not be calculated right now.")
	}
	actionsLimit, err := entitlementSet.Limit(entitlements.LimitOrgActionsMinutesQuota)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: actions minutes limit", "org_id", orgID, "error", err)
		return unavailableBillingUsage("Actions limits could not be calculated right now.")
	}

	totalStorage := counters.RepoStorageBytes + counters.ObjectStorageBytes
	periodLabel := "Current month"
	if counters.ActionsPeriodStart.Valid && counters.ActionsPeriodEnd.Valid {
		periodLabel = counters.ActionsPeriodStart.Time.Format("Jan 2") + " - " + counters.ActionsPeriodEnd.Time.Add(-time.Nanosecond).Format("Jan 2, 2006")
	}
	usage := billingUsageBreakdown{
		Available:       true,
		PeriodLabel:     periodLabel,
		CalculatedLabel: formatOptionalTime(counters.CalculatedAt),
		Rows: []billingUsageRow{
			billingUsageRowForLimit(
				"storage",
				"Storage",
				totalStorage,
				storageLimit,
				"Repository storage "+formatBytes(counters.RepoStorageBytes)+"; object storage "+formatBytes(counters.ObjectStorageBytes)+" including "+formatBytes(counters.ActionsLogBytes)+" of Actions logs and "+formatBytes(counters.ActionsArtifactBytes)+" of artifacts.",
			),
			billingUsageRowForLimit(
				"actions-minutes",
				"Actions minutes",
				counters.ActionsMinutesUsed,
				actionsLimit,
				"Completed and canceled workflow job runtime rounded up to whole minutes for the monthly period.",
			),
		},
	}
	usage.Alert = billingUsageAlert(usage.Rows)
	return usage
}

func unavailableBillingUsage(message string) billingUsageBreakdown {
	return billingUsageBreakdown{
		Available: false,
		Alert: billingAlert{
			Class:   "shithub-flash-notice",
			Message: message,
		},
		Rows: []billingUsageRow{
			{Key: "storage", Label: "Storage", UsedLabel: "Unavailable", LimitLabel: "Unavailable", PercentLabel: "—", Detail: "Usage data is unavailable.", StatusClass: "is-muted"},
			{Key: "actions-minutes", Label: "Actions minutes", UsedLabel: "Unavailable", LimitLabel: "Unavailable", PercentLabel: "—", Detail: "Usage data is unavailable.", StatusClass: "is-muted"},
		},
	}
}

func billingUsageRowForLimit(key, label string, used int64, limit entitlements.LimitValue, detail string) billingUsageRow {
	if used < 0 {
		used = 0
	}
	row := billingUsageRow{
		Key:          key,
		Label:        label,
		UsedLabel:    billingUsageAmountLabel(used, limit.Unit),
		LimitLabel:   billingLimitLabel(limit),
		PercentLabel: "—",
		Detail:       detail,
		StatusClass:  "is-muted",
	}
	if !limit.Defined || limit.Unlimited || limit.Value <= 0 {
		if limit.Unlimited {
			row.PercentLabel = "Unlimited"
			row.StatusClass = "is-ok"
		}
		return row
	}
	row.PercentValue = cappedPercentValue(used, limit.Value)
	row.PercentLabel = formatPercent(used, limit.Value)
	switch {
	case used > limit.Value:
		row.StatusClass = "is-over"
	case row.PercentValue >= 95:
		row.StatusClass = "is-danger"
	case row.PercentValue >= 80:
		row.StatusClass = "is-warning"
	default:
		row.StatusClass = "is-ok"
	}
	if limit.Overridden {
		row.Detail += " A temporary site-admin quota override is active."
	}
	return row
}

func billingUsageAlert(rows []billingUsageRow) billingAlert {
	var warning80, warning95, over *billingUsageRow
	for i := range rows {
		row := &rows[i]
		switch row.StatusClass {
		case "is-over":
			if over == nil {
				over = row
			}
		case "is-danger":
			if warning95 == nil {
				warning95 = row
			}
		case "is-warning":
			if warning80 == nil {
				warning80 = row
			}
		}
	}
	switch {
	case over != nil:
		return billingAlert{
			Class:   "shithub-flash-error",
			Message: "This organization is over its " + strings.ToLower(over.Label) + " quota. Upgrade or reduce usage before additional hosted-cost writes are accepted.",
		}
	case warning95 != nil:
		return billingAlert{
			Class:   "shithub-flash-error",
			Message: "This organization has used at least 95% of its " + strings.ToLower(warning95.Label) + " quota. Upgrade or reduce usage before additional hosted-cost writes are blocked.",
		}
	case warning80 != nil:
		return billingAlert{
			Class:   "shithub-flash-notice",
			Message: "This organization has used at least 80% of its " + strings.ToLower(warning80.Label) + " quota.",
		}
	default:
		return billingAlert{}
	}
}

func (h *Handlers) ensureStripeCustomer(r *http.Request, org orgsdb.Org, state orgbilling.State) (orgbilling.State, error) {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return state, nil
	}
	customer, err := h.d.Stripe.CreateCustomer(r.Context(), stripebilling.CustomerInput{
		OrgID:   org.ID,
		OrgSlug: org.Slug,
		OrgName: strings.TrimSpace(org.DisplayName),
		Email:   strings.TrimSpace(org.BillingEmail),
	})
	if err != nil {
		return orgbilling.State{}, err
	}
	return orgbilling.SetStripeCustomer(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, customer.ID)
}

func (h *Handlers) billingReturnURL(orgSlug, overrideURL, fallbackPath string) string {
	overrideURL = strings.TrimSpace(overrideURL)
	if overrideURL != "" {
		return strings.ReplaceAll(overrideURL, "{org}", url.PathEscape(orgSlug))
	}
	base, err := url.Parse(strings.TrimRight(h.d.BaseURL, "/") + "/")
	if err != nil {
		return ""
	}
	rel, err := url.Parse(strings.TrimLeft(fallbackPath, "/"))
	if err != nil {
		return ""
	}
	return base.ResolveReference(rel).String()
}

func orgBillingSettingsPath(slug string) string {
	return "/organizations/" + slug + "/settings/billing"
}

func orgBillingLicensingPath(slug string) string {
	return orgBillingSettingsPath(slug) + "/licensing"
}

func orgBillingSeatAddPath(slug string) string {
	return orgBillingSettingsPath(slug) + "/seats/add"
}

func orgBillingSeatRemovePath(slug string) string {
	return orgBillingSettingsPath(slug) + "/seats/remove"
}

func billingNotice(code string) string {
	switch code {
	case "checkout-success":
		return "Checkout completed. Stripe will finish provisioning as webhook events arrive."
	case "checkout-canceled":
		return "Checkout canceled."
	case "team-created":
		return "Organization created. Continue with Team checkout to unlock paid features."
	case "team-created-import-started":
		return "Organization created and GitHub import started. Continue with Team checkout to unlock paid features."
	case "team-checkout-failed":
		return "Organization created, but checkout could not be started. Try Continue with Team again."
	case "quota-override-saved":
		return "Quota override saved."
	case "quota-override-cleared":
		return "Quota override cleared."
	case "seat-drift-none":
		return "Local licensed seats already match Stripe."
	case "seat-drift-repaired":
		return "Local licensed seats were repaired from Stripe."
	default:
		return ""
	}
}

func billingLicensingNotice(code string) string {
	switch code {
	case "seats-added":
		return "Seats added. Stripe will finalize invoices and webhook state as events arrive."
	case "seats-removed":
		return "Seats removed. Stripe will apply any billing adjustment according to the subscription schedule."
	default:
		return ""
	}
}

func (h *Handlers) billingDebugView(r *http.Request, orgID int64, state orgbilling.State, licenseState orgbilling.TeamLicenseState) billingDebugView {
	debug := billingDebugView{
		StripeCustomerID:         pgTextString(state.StripeCustomerID),
		StripeSubscriptionID:     pgTextString(state.StripeSubscriptionID),
		StripeSubscriptionItemID: pgTextString(state.StripeSubscriptionItemID),
		LastWebhookEventID:       strings.TrimSpace(state.LastWebhookEventID),
	}
	debug.SeatDrift = h.billingSeatDriftView(r, state, licenseState)
	overrides, err := orgbilling.ListOrgQuotaOverrides(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: list quota overrides", "org_id", orgID, "error", err)
	} else {
		debug.QuotaOverrides = billingQuotaOverrideViews(overrides)
	}
	debug.QuotaOverrideForms = billingQuotaOverrideForms(overrides)
	if debug.LastWebhookEventID == "" {
		return debug
	}
	receipt, err := orgbilling.GetWebhookEventReceipt(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, debug.LastWebhookEventID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.WarnContext(r.Context(), "org billing: load latest webhook receipt",
				"event_id", debug.LastWebhookEventID, "error", err)
		}
		return debug
	}
	debug.LastWebhookEventType = receipt.EventType
	debug.LastWebhookReceivedAt = formatOptionalTime(receipt.ReceivedAt)
	debug.LastWebhookProcessedAt = formatOptionalTime(receipt.ProcessedAt)
	debug.LastWebhookAttempts = receipt.ProcessingAttempts
	debug.LastWebhookError = strings.TrimSpace(receipt.ProcessError)
	switch {
	case receipt.ProcessedAt.Valid:
		debug.LastWebhookStatus = "processed"
	case debug.LastWebhookError != "":
		debug.LastWebhookStatus = "failed"
	default:
		debug.LastWebhookStatus = "pending"
	}
	return debug
}

func (h *Handlers) billingSeatDriftView(r *http.Request, state orgbilling.State, licenseState orgbilling.TeamLicenseState) billingSeatDriftView {
	drift := billingSeatDriftView{
		LocalLicensedSeats: int64(licenseState.LicensedSeats),
		UsedSeats:          licenseState.UsedSeats,
	}
	if !orgbilling.IsTeamState(state) {
		drift.Status = "Unavailable"
		drift.Detail = "Seat drift repair applies only to Team organizations."
		return drift
	}
	subscriptionItemID := stripeSubscriptionItemIDFromState(state)
	if subscriptionItemID == "" {
		drift.Status = "Unavailable"
		drift.Detail = "No Stripe subscription item is recorded for this organization."
		return drift
	}
	if h.d.Stripe == nil {
		drift.Status = "Unavailable"
		drift.Detail = "Stripe is not configured for this instance."
		return drift
	}
	quantity, err := h.d.Stripe.FetchSubscriptionItemQuantity(r.Context(), subscriptionItemID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: fetch stripe subscription item quantity",
			"subscription_item_id", subscriptionItemID, "error", err)
		drift.Status = "Unavailable"
		drift.Detail = "Could not fetch the live Stripe seat quantity right now."
		return drift
	}
	drift.Available = true
	drift.StripeSeats = quantity
	drift.Detected = quantity != drift.LocalLicensedSeats
	switch {
	case quantity < 1:
		drift.Status = "Invalid"
		drift.Detail = "Stripe returned an invalid licensed-seat quantity."
	case quantity > maxIntValue():
		drift.Status = "Invalid"
		drift.Detail = "Stripe returned a licensed-seat quantity that is too large to repair safely."
	case quantity < int64(licenseState.UsedSeats):
		drift.Status = "Repair blocked"
		drift.Detail = "Stripe reports fewer seats than the organization is using. Add seats in Stripe before repairing local state."
	case drift.Detected:
		drift.Status = "Drift detected"
		drift.Detail = "Stripe and local licensed-seat counts differ. Repair copies Stripe's live quantity into local entitlement state."
		drift.CanRepair = true
	default:
		drift.Status = "In sync"
		drift.Detail = "Local licensed seats match Stripe."
	}
	return drift
}

func stripeSubscriptionItemIDFromState(state orgbilling.State) string {
	if !state.StripeSubscriptionItemID.Valid {
		return ""
	}
	return strings.TrimSpace(state.StripeSubscriptionItemID.String)
}

func maxIntValue() int64 {
	return int64(^uint(0) >> 1)
}

func billingSeatRepairError(err error) string {
	switch {
	case errors.Is(err, orgbilling.ErrSeatCountBelowUsage):
		return "Stripe currently reports fewer seats than this organization is using. Add seats in Stripe first, then repair local state."
	case errors.Is(err, orgbilling.ErrInvalidSeatCount):
		return "Stripe returned an invalid seat quantity. Fix the Stripe subscription item before repairing local state."
	case errors.Is(err, orgbilling.ErrTeamPlanRequired):
		return "Seat drift repair applies only to Team organizations."
	default:
		return "Could not repair local licensed seats right now."
	}
}

func (h *Handlers) recordBillingSeatDriftRepairAudit(
	r *http.Request,
	viewer middleware.CurrentUser,
	orgID int64,
	before int,
	stripeSeats int,
	usedSeats int,
	state orgbilling.State,
) {
	actor, meta := viewer.AuditActor(map[string]any{
		"source":                      "stripe_repair",
		"local_licensed_seats_before": before,
		"local_licensed_seats_after":  stripeSeats,
		"stripe_licensed_seats":       stripeSeats,
		"used_seats":                  usedSeats,
		"stripe_subscription_item_id": stripeSubscriptionItemIDFromState(state),
	})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionAdminOrgBillingSeatsRepaired, audit.TargetOrg, orgID, meta)
}

func parseBillingQuotaKind(raw string) (orgbilling.QuotaKind, bool) {
	switch orgbilling.QuotaKind(strings.TrimSpace(raw)) {
	case orgbilling.QuotaKindStorageBytes:
		return orgbilling.QuotaKindStorageBytes, true
	case orgbilling.QuotaKindActionsMinutes:
		return orgbilling.QuotaKindActionsMinutes, true
	default:
		return "", false
	}
}

func parseBillingQuotaLimit(raw string, unlimited bool) (int64, error) {
	if unlimited {
		return 0, nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, orgbilling.ErrInvalidQuotaOverride
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, orgbilling.ErrInvalidQuotaOverride
	}
	return v, nil
}

func billingQuotaOverrideViews(overrides []orgbilling.QuotaOverride) []billingQuotaOverrideView {
	items := make([]billingQuotaOverrideView, 0, len(overrides))
	for _, override := range overrides {
		label := "Unavailable"
		switch override.Kind {
		case orgbilling.QuotaKindStorageBytes:
			label = "Storage"
		case orgbilling.QuotaKindActionsMinutes:
			label = "Actions minutes"
		}
		limit := "Unavailable"
		if override.Unlimited {
			limit = "Unlimited"
		} else if override.LimitValue.Valid {
			unit := "bytes"
			if override.Kind == orgbilling.QuotaKindActionsMinutes {
				unit = "minutes"
			}
			limit = billingUsageAmountLabel(override.LimitValue.Int64, unit)
		}
		items = append(items, billingQuotaOverrideView{
			Kind:      label,
			Limit:     limit,
			Note:      strings.TrimSpace(override.Note),
			UpdatedAt: formatOptionalTime(override.UpdatedAt),
		})
	}
	return items
}

func billingQuotaOverrideForms(overrides []orgbilling.QuotaOverride) []billingQuotaOverrideForm {
	current := make(map[orgbilling.QuotaKind]orgbilling.QuotaOverride, len(overrides))
	for _, override := range overrides {
		current[override.Kind] = override
	}
	forms := []billingQuotaOverrideForm{
		{
			KindValue: string(orgbilling.QuotaKindStorageBytes),
			KindLabel: "Storage",
			UnitLabel: "bytes",
			Help:      "Total repository and object storage accepted before hosted-cost writes are blocked.",
		},
		{
			KindValue: string(orgbilling.QuotaKindActionsMinutes),
			KindLabel: "Actions minutes",
			UnitLabel: "minutes",
			Help:      "Monthly Actions runner minutes accepted before org jobs stop dispatching.",
		},
	}
	for i := range forms {
		override, ok := current[orgbilling.QuotaKind(forms[i].KindValue)]
		if !ok {
			continue
		}
		if override.LimitValue.Valid {
			forms[i].LimitValue = strconv.FormatInt(override.LimitValue.Int64, 10)
		}
		forms[i].Unlimited = override.Unlimited
		forms[i].Note = strings.TrimSpace(override.Note)
	}
	return forms
}

func (h *Handlers) applyTeamSeatChange(r *http.Request, org orgsdb.Org, delta int, mode string) error {
	state, licenseState, err := h.teamSeatChangeState(r, org)
	if err != nil {
		return err
	}
	if delta < 1 {
		return orgbilling.ErrInvalidSeatCount
	}
	newTotal := licenseState.LicensedSeats + delta
	source := "owner_add"
	if mode == "remove" {
		newTotal = licenseState.LicensedSeats - delta
		source = "owner_remove"
	}
	if newTotal < licenseState.UsedSeats {
		return orgbilling.ErrSeatCountBelowUsage
	}
	if newTotal < 1 {
		return orgbilling.ErrInvalidSeatCount
	}
	prorationDate := time.Now().UTC().Unix()
	preview, err := h.previewTeamSeatChange(r, state, licenseState, newTotal, prorationDate)
	if err != nil {
		return err
	}
	if preview.ProrationDate > 0 {
		prorationDate = preview.ProrationDate
	}
	subscriptionItemID := teamSeatSubscriptionItemID(state, licenseState)
	if err := h.d.Stripe.ApplyTeamSeatChange(r.Context(), stripebilling.TeamSeatChangeInput{
		OrgID:              org.ID,
		SubscriptionItemID: subscriptionItemID,
		NewQuantity:        int64(newTotal),
		ProrationDate:      prorationDate,
		IdempotencyKey:     stripebilling.TeamSeatChangeIdempotencyKey(org.ID, subscriptionItemID, int64(newTotal), prorationDate),
	}); err != nil {
		return err
	}
	_, err = orgbilling.SetTeamLicensedSeats(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, newTotal, source)
	return err
}

func (h *Handlers) teamSeatChangeState(r *http.Request, org orgsdb.Org) (orgbilling.State, orgbilling.TeamLicenseState, error) {
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		return orgbilling.State{}, orgbilling.TeamLicenseState{}, err
	}
	if !orgbilling.IsTeamState(state) {
		return orgbilling.State{}, orgbilling.TeamLicenseState{}, orgbilling.ErrTeamPlanRequired
	}
	if !billingSeatManagementAvailable(state) {
		return orgbilling.State{}, orgbilling.TeamLicenseState{}, errSeatChangeUnavailable
	}
	licenseState, err := orgbilling.GetTeamLicenseState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		return orgbilling.State{}, orgbilling.TeamLicenseState{}, err
	}
	return state, licenseState, nil
}

func (h *Handlers) previewTeamSeatChange(r *http.Request, state orgbilling.State, licenseState orgbilling.TeamLicenseState, newTotal int, prorationDate int64) (stripebilling.TeamSeatPreview, error) {
	customerID := strings.TrimSpace(pgTextString(state.StripeCustomerID))
	if customerID == "" {
		return stripebilling.TeamSeatPreview{}, stripebilling.ErrCustomerIDRequired
	}
	subscriptionID := strings.TrimSpace(pgTextString(state.StripeSubscriptionID))
	if subscriptionID == "" {
		return stripebilling.TeamSeatPreview{}, stripebilling.ErrSubscriptionID
	}
	subscriptionItemID := teamSeatSubscriptionItemID(state, licenseState)
	if subscriptionItemID == "" {
		return stripebilling.TeamSeatPreview{}, stripebilling.ErrSubscriptionItemID
	}
	return h.d.Stripe.PreviewTeamSeatChange(r.Context(), stripebilling.TeamSeatPreviewInput{
		CustomerID:         customerID,
		SubscriptionID:     subscriptionID,
		SubscriptionItemID: subscriptionItemID,
		NewQuantity:        int64(newTotal),
		ProrationDate:      prorationDate,
	})
}

func teamSeatSubscriptionItemID(state orgbilling.State, licenseState orgbilling.TeamLicenseState) string {
	subscriptionItemID := strings.TrimSpace(licenseState.StripeSubscriptionItemID)
	if subscriptionItemID == "" && state.StripeSubscriptionItemID.Valid {
		subscriptionItemID = strings.TrimSpace(state.StripeSubscriptionItemID.String)
	}
	return subscriptionItemID
}

func parsePositiveSeatDelta(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, orgbilling.ErrInvalidSeatCount
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, orgbilling.ErrInvalidSeatCount
	}
	return n, nil
}

func billingSeatChangeError(err error, mode string) string {
	switch {
	case errors.Is(err, orgbilling.ErrTeamPlanRequired):
		return "Upgrade this organization to Team before changing licensed seats."
	case errors.Is(err, orgbilling.ErrSeatCountBelowUsage):
		return "Licensed seats cannot be reduced below the number of people consuming seats."
	case errors.Is(err, stripebilling.ErrCustomerIDRequired), errors.Is(err, stripebilling.ErrSubscriptionID):
		return "Seat changes are unavailable until Stripe has attached a Team subscription."
	case errors.Is(err, stripebilling.ErrSubscriptionItemID):
		return "Seat changes are unavailable until Stripe has attached a Team subscription item."
	case errors.Is(err, errSeatChangeUnavailable):
		return "Seat changes are unavailable until this subscription is active."
	default:
		if mode == "remove" {
			return "Could not remove seats right now."
		}
		return "Could not add seats right now."
	}
}

func billingSeatActionMenuForOrg(slug string, state orgbilling.State, licenseState orgbilling.TeamLicenseState, canManage bool) billingSeatActionMenu {
	menu := billingSeatActionMenu{
		CanManage:   canManage,
		AddHref:     orgBillingSeatAddPath(slug),
		RemoveHref:  orgBillingSeatRemovePath(slug),
		CanAddSeats: canManage && billingSeatManagementAvailable(state) && licenseState.StripeSubscriptionItemID != "",
		CanRemove:   canManage && billingSeatManagementAvailable(state) && licenseState.StripeSubscriptionItemID != "" && licenseState.AvailableSeats > 0,
	}
	switch {
	case !canManage:
		menu.DisabledNote = "Only organization owners can manage licensed seats."
	case !orgbilling.IsTeamState(state):
		menu.DisabledNote = "Upgrade to Team before managing licensed seats."
	case !billingSeatManagementAvailable(state):
		menu.DisabledNote = "Seat changes are unavailable until this subscription is active."
	case licenseState.StripeSubscriptionItemID == "":
		menu.DisabledNote = "Stripe has not attached a Team subscription item yet."
	case licenseState.AvailableSeats <= 0:
		menu.DisabledNote = "No unassigned seats are available to remove."
	}
	return menu
}

func billingSeatConsumerViews(rows []billingdb.ListTeamSeatConsumersRow) []billingSeatConsumerView {
	items := make([]billingSeatConsumerView, 0, len(rows))
	for _, row := range rows {
		displayName := strings.TrimSpace(row.DisplayName)
		if displayName == "" {
			displayName = row.Username
		}
		items = append(items, billingSeatConsumerView{
			Username:     row.Username,
			DisplayName:  displayName,
			AvatarURL:    "/avatars/" + url.PathEscape(row.Username),
			RoleLabel:    billingOrgRoleLabel(string(row.Role)),
			JoinedLabel:  formatOptionalTime(row.JoinedAt),
			StatusLabel:  "Consumed",
			StatusDetail: "Organization members, including owners, consume Team seats.",
		})
	}
	return items
}

func billingPendingSeatInviteViews(rows []orgsdb.ListPendingInvitationsForOrgRow) []billingPendingSeatInviteView {
	items := make([]billingPendingSeatInviteView, 0, len(rows))
	for _, row := range rows {
		target := "Pending invite"
		if row.TargetUsername.Valid {
			target = "@" + row.TargetUsername.String
		} else if row.TargetEmail.Valid {
			target = row.TargetEmail.String
		}
		invitedBy := ""
		if row.InvitedByUsername.Valid {
			invitedBy = row.InvitedByUsername.String
		}
		items = append(items, billingPendingSeatInviteView{
			Target:       target,
			RoleLabel:    billingOrgRoleLabel(string(row.Role)),
			InvitedBy:    invitedBy,
			CreatedLabel: formatOptionalTime(row.CreatedAt),
			StatusLabel:  "Pending",
			StatusDetail: "Pending invitations are shown separately; shithub reserves billing enforcement for acceptance until invitation-seat charging is enabled.",
		})
	}
	return items
}

func billingOrgRoleLabel(role string) string {
	if role == "owner" {
		return "Owner"
	}
	return "Member"
}

func billingSeatChangeFormForOrg(slug, mode string, delta int, state orgbilling.State, licenseState orgbilling.TeamLicenseState) billingSeatChangeForm {
	if delta < 1 {
		delta = 1
	}
	form := billingSeatChangeForm{
		Mode:               mode,
		CurrentSeats:       licenseState.LicensedSeats,
		UsedSeats:          licenseState.UsedSeats,
		AvailableSeats:     licenseState.AvailableSeats,
		ChangeSeats:        delta,
		SubscriptionStatus: billingStatusLabel(state.SubscriptionStatus),
		CanSubmit:          billingSeatManagementAvailable(state) && licenseState.StripeSubscriptionItemID != "",
	}
	switch mode {
	case "remove":
		form.Title = slug + " - remove seats"
		form.Heading = "Remove seats"
		form.Description = "Remove unassigned Team seats from this organization."
		form.Action = orgBillingSeatRemovePath(slug)
		form.InputName = "remove_seats"
		form.InputLabel = "Seats to remove"
		form.InputHelp = "You can remove only seats that are not assigned to members."
		form.SubmitLabel = "Remove seats"
		form.NewTotal = licenseState.LicensedSeats - delta
		if form.NewTotal < 0 {
			form.NewTotal = 0
		}
	default:
		form.Mode = "add"
		form.Title = slug + " - add seats"
		form.Heading = "Add seats"
		form.Description = "Add licensed Team seats before inviting more members."
		form.Action = orgBillingSeatAddPath(slug)
		form.InputName = "additional_seats"
		form.InputLabel = "Seats to add"
		form.InputHelp = "New seats are available after Stripe accepts the subscription quantity change."
		form.SubmitLabel = "Add seats"
		form.NewTotal = licenseState.LicensedSeats + delta
	}
	deltaForCost := form.NewTotal - licenseState.LicensedSeats
	form.MonthlyDeltaLabel = billingSeatMonthlyDeltaLabel(deltaForCost)
	form.NextBillLabel = formatCurrencyAmount("usd", int64(form.NewTotal*teamSeatMonthlyPriceUSD*100)) + " per month"
	form.ProrationLabel = billingSeatProrationLabel(state, deltaForCost, time.Now().UTC())
	if mode == "remove" && delta > licenseState.AvailableSeats {
		form.CanSubmit = false
	}
	if form.NewTotal < licenseState.UsedSeats {
		form.CanSubmit = false
	}
	return form
}

func applyTeamSeatPreviewLabels(form *billingSeatChangeForm, preview stripebilling.TeamSeatPreview) {
	currency := strings.TrimSpace(preview.Currency)
	if currency == "" {
		currency = "usd"
	}
	switch {
	case preview.CurrentPeriodAmount > 0:
		form.ProrationLabel = formatCurrencyAmount(currency, preview.CurrentPeriodAmount) + " prorated charge, added to the next invoice"
	case preview.CurrentPeriodAmount < 0:
		form.ProrationLabel = formatCurrencyAmount(currency, -preview.CurrentPeriodAmount) + " prorated credit, applied to the next invoice"
	default:
		form.ProrationLabel = formatCurrencyAmount(currency, 0) + " current-period adjustment"
	}
	if preview.AmountDue > 0 {
		form.ProrationLabel += "; preview amount due " + formatCurrencyAmount(currency, preview.AmountDue)
	}
}

func billingSeatManagementAvailable(state orgbilling.State) bool {
	if !orgbilling.IsTeamState(state) || state.LockedAt.Valid {
		return false
	}
	switch state.SubscriptionStatus {
	case orgbilling.SubscriptionStatusActive, orgbilling.SubscriptionStatusTrialing:
		return true
	default:
		return false
	}
}

func billingSeatMonthlyDeltaLabel(deltaSeats int) string {
	cents := int64(deltaSeats * teamSeatMonthlyPriceUSD * 100)
	if cents == 0 {
		return "$0.00 USD per month"
	}
	if cents > 0 {
		return "+" + formatCurrencyAmount("usd", cents) + " per month"
	}
	return "-" + formatCurrencyAmount("usd", -cents) + " per month"
}

func billingSeatProrationLabel(state orgbilling.State, deltaSeats int, now time.Time) string {
	if deltaSeats == 0 {
		return "$0.00 USD estimated for the current period"
	}
	if !state.CurrentPeriodStart.Valid || !state.CurrentPeriodEnd.Valid {
		return "Stripe will calculate the current-period adjustment."
	}
	start := state.CurrentPeriodStart.Time
	end := state.CurrentPeriodEnd.Time
	if !end.After(start) || !end.After(now) {
		return "Stripe will calculate the current-period adjustment."
	}
	total := end.Sub(start)
	remaining := end.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	cents := int64(deltaSeats*teamSeatMonthlyPriceUSD*100) * int64(remaining) / int64(total)
	if cents >= 0 {
		return formatCurrencyAmount("usd", cents) + " estimated charge for the current period"
	}
	return formatCurrencyAmount("usd", -cents) + " estimated credit for the current period"
}

func billingSummary(state orgbilling.State, licenseState orgbilling.TeamLicenseState) []billingSummaryItem {
	summary := []billingSummaryItem{
		{
			Label:  "Current plan",
			Value:  billingPlanLabel(state.Plan),
			Detail: billingPlanDetail(state),
		},
		{
			Label:  "Subscription",
			Value:  billingStatusLabel(state.SubscriptionStatus),
			Detail: billingStatusDetail(state),
		},
		{
			Label:  "Licensed seats",
			Value:  fmt.Sprintf("%d", licenseState.LicensedSeats),
			Detail: billingSeatDetail(licenseState),
		},
		{
			Label:  "Payment source",
			Value:  billingPaymentSourceLabel(state),
			Detail: billingPaymentSourceDetail(state),
		},
	}
	return summary
}

func billingPlanLabel(plan orgbilling.Plan) string {
	switch plan {
	case orgbilling.PlanTeam:
		return "Team"
	case orgbilling.PlanEnterprise:
		return "Enterprise"
	default:
		return "Free"
	}
}

func billingPlanDetail(state orgbilling.State) string {
	if state.CurrentPeriodEnd.Valid {
		label := "Current period ends"
		if state.CancelAtPeriodEnd {
			label = "Scheduled to cancel at period end"
		}
		return label + " " + state.CurrentPeriodEnd.Time.Format("Jan 2, 2006")
	}
	if state.SubscriptionStatus == orgbilling.SubscriptionStatusNone ||
		state.SubscriptionStatus == orgbilling.SubscriptionStatusCanceled {
		return "No active paid subscription."
	}
	return ""
}

func billingStatusLabel(status orgbilling.SubscriptionStatus) string {
	switch status {
	case orgbilling.SubscriptionStatusActive:
		return "Active"
	case orgbilling.SubscriptionStatusTrialing:
		return "Trialing"
	case orgbilling.SubscriptionStatusIncomplete:
		return "Incomplete"
	case orgbilling.SubscriptionStatusPastDue:
		return "Past due"
	case orgbilling.SubscriptionStatusCanceled:
		return "Canceled"
	case orgbilling.SubscriptionStatusUnpaid:
		return "Unpaid"
	case orgbilling.SubscriptionStatusPaused:
		return "Paused"
	default:
		return "No subscription"
	}
}

func billingStatusDetail(state orgbilling.State) string {
	if state.GraceUntil.Valid {
		return "Grace period until " + state.GraceUntil.Time.Format("Jan 2, 2006")
	}
	if state.CanceledAt.Valid {
		return "Canceled " + state.CanceledAt.Time.Format("Jan 2, 2006")
	}
	if state.TrialEnd.Valid {
		return "Trial ends " + state.TrialEnd.Time.Format("Jan 2, 2006")
	}
	return ""
}

func billingSeatDetail(state orgbilling.TeamLicenseState) string {
	if state.SeatSnapshotAt.Valid {
		return fmt.Sprintf("Latest license snapshot: %d licensed, %d used captured %s", state.LicensedSeats, state.UsedSeats, state.SeatSnapshotAt.Time.Format("Jan 2, 2006"))
	}
	if state.LicensedSeats > 0 || state.UsedSeats > 0 {
		return fmt.Sprintf("Latest license snapshot: %d licensed, %d used", state.LicensedSeats, state.UsedSeats)
	}
	return "Seat sync has not recorded a snapshot yet."
}

func billingPaymentSourceLabel(state orgbilling.State) string {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return "Stripe customer connected"
	}
	return "Not connected"
}

func billingPaymentSourceDetail(state orgbilling.State) string {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return "Payment method and invoices are managed in Stripe Billing Portal."
	}
	return "Checkout creates a customer record the first time this organization upgrades."
}

func billingAlertForState(state orgbilling.State, orgSlug string) billingAlert {
	path := orgBillingSettingsPath(orgSlug)
	switch state.SubscriptionStatus {
	case orgbilling.SubscriptionStatusPastDue:
		if state.GraceUntil.Valid && time.Now().UTC().Before(state.GraceUntil.Time) {
			return billingAlert{
				Class:      "shithub-flash-notice",
				Message:    "Payment failed. Team features remain available during the billing grace period, which ends " + state.GraceUntil.Time.Format("Jan 2, 2006") + ".",
				ActionText: "Manage billing",
				ActionHref: path,
			}
		}
		return billingAlert{
			Class:      "shithub-flash-error",
			Message:    "Payment is past due. Team-only features are read-only until billing is brought back into good standing.",
			ActionText: "Manage billing",
			ActionHref: path,
		}
	case orgbilling.SubscriptionStatusCanceled:
		return billingAlert{
			Class:      "shithub-flash-notice",
			Message:    "This organization is on Free after cancellation. Existing paid configuration is preserved, but Team-only features are read-only until reactivated.",
			ActionText: "Upgrade to Team",
			ActionHref: path + "#manage-plan",
		}
	case orgbilling.SubscriptionStatusIncomplete, orgbilling.SubscriptionStatusUnpaid, orgbilling.SubscriptionStatusPaused:
		return billingAlert{
			Class:      "shithub-flash-error",
			Message:    "This subscription needs billing action before Team features are available.",
			ActionText: "Manage billing",
			ActionHref: path,
		}
	default:
		if state.CancelAtPeriodEnd && state.CurrentPeriodEnd.Valid {
			return billingAlert{
				Class:      "shithub-flash-notice",
				Message:    "Team is scheduled to cancel at the end of the current billing period on " + state.CurrentPeriodEnd.Time.Format("Jan 2, 2006") + ".",
				ActionText: "Manage billing",
				ActionHref: path,
			}
		}
		return billingAlert{}
	}
}

func billingInvoiceViews(invoices []billingdb.BillingInvoice) []billingInvoiceView {
	items := make([]billingInvoiceView, 0, len(invoices))
	for _, inv := range invoices {
		number := strings.TrimSpace(inv.Number)
		if number == "" {
			number = inv.StripeInvoiceID
		}
		items = append(items, billingInvoiceView{
			Number:           number,
			StatusLabel:      billingInvoiceStatusLabel(inv.Status),
			StatusClass:      strings.ReplaceAll(strings.ToLower(string(inv.Status)), "_", "-"),
			AmountLabel:      formatCurrencyAmount(inv.Currency, inv.AmountDueCents),
			PeriodLabel:      billingPeriodLabel(inv),
			DueLabel:         billingDueLabel(inv),
			HostedInvoiceURL: strings.TrimSpace(inv.HostedInvoiceUrl),
			InvoicePDFURL:    strings.TrimSpace(inv.InvoicePdfUrl),
		})
	}
	return items
}

func billingInvoiceStatusLabel(status orgbilling.InvoiceStatus) string {
	switch status {
	case orgbilling.InvoiceStatusOpen:
		return "Open"
	case orgbilling.InvoiceStatusPaid:
		return "Paid"
	case orgbilling.InvoiceStatusVoid:
		return "Void"
	case orgbilling.InvoiceStatusUncollectible:
		return "Uncollectible"
	case orgbilling.InvoiceStatusRefunded:
		return "Refunded"
	default:
		return "Draft"
	}
}

func billingPeriodLabel(inv billingdb.BillingInvoice) string {
	if inv.PeriodStart.Valid && inv.PeriodEnd.Valid {
		return inv.PeriodStart.Time.Format("Jan 2, 2006") + " - " + inv.PeriodEnd.Time.Format("Jan 2, 2006")
	}
	return "—"
}

func billingDueLabel(inv billingdb.BillingInvoice) string {
	switch {
	case inv.RefundedAt.Valid:
		return "Refunded " + inv.RefundedAt.Time.Format("Jan 2, 2006")
	case inv.PaidAt.Valid:
		return "Paid " + inv.PaidAt.Time.Format("Jan 2, 2006")
	case inv.VoidedAt.Valid:
		return "Voided " + inv.VoidedAt.Time.Format("Jan 2, 2006")
	case inv.DueAt.Valid:
		return inv.DueAt.Time.Format("Jan 2, 2006")
	default:
		return "—"
	}
}

func formatGracePeriod(d time.Duration) string {
	if d <= 0 {
		return "No grace period"
	}
	if d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
	return d.String()
}

func pgTextString(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return strings.TrimSpace(v.String)
}

func formatOptionalTime(v pgtype.Timestamptz) string {
	if v.Valid && !v.Time.IsZero() {
		return v.Time.UTC().Format("Jan 2, 2006 15:04 UTC")
	}
	return ""
}

func formatCurrencyAmount(currency string, cents int64) string {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	major := cents / 100
	minor := cents % 100
	if currency == "" {
		currency = "USD"
	}
	return fmt.Sprintf("%s$%d.%02d %s", sign, major, minor, currency)
}

func billingUsageAmountLabel(value int64, unit string) string {
	switch unit {
	case "bytes":
		return formatBytes(value)
	case "minutes":
		return fmt.Sprintf("%d minutes", value)
	default:
		return fmt.Sprintf("%d", value)
	}
}

func billingLimitLabel(limit entitlements.LimitValue) string {
	if !limit.Defined {
		if limit.Reason == entitlements.ReasonEnterpriseContactSales {
			return "Contact sales"
		}
		return "Unavailable"
	}
	if limit.Unlimited {
		if limit.Overridden {
			return "Unlimited override"
		}
		return "Unlimited"
	}
	label := billingUsageAmountLabel(limit.Value, limit.Unit)
	if limit.Overridden {
		return label + " override"
	}
	return label
}

func cappedPercentValue(used, limit int64) int64 {
	if used <= 0 || limit <= 0 {
		return 0
	}
	percent := (used*100 + limit/2) / limit
	if percent > 100 {
		return 100
	}
	return percent
}

func formatPercent(used, limit int64) string {
	if used <= 0 || limit <= 0 {
		return "0%"
	}
	tenths := (used*1000 + limit/2) / limit
	if tenths%10 == 0 {
		return fmt.Sprintf("%d%%", tenths/10)
	}
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1024 {
		if bytes == 1 {
			return "1 B"
		}
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	div := int64(1024)
	unitIndex := 0
	for unitIndex < len(units)-1 && bytes >= div*1024 {
		div *= 1024
		unitIndex++
	}
	tenths := (bytes*10 + div/2) / div
	if tenths%10 == 0 {
		return fmt.Sprintf("%d %s", tenths/10, units[unitIndex])
	}
	return fmt.Sprintf("%d.%d %s", tenths/10, tenths%10, units[unitIndex])
}
