// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
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

func (h *Handlers) settingsBilling(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderSettingsBilling(w, r, org, "", billingNotice(r.URL.Query().Get("notice")))
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
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: load state for checkout", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	state, err = h.ensureStripeCustomer(r, org, state)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: ensure stripe customer", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not start checkout right now.", "")
		return
	}
	seats, err := orgbilling.CountBillableOrgMembers(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: count seats", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not calculate billable seats right now.", "")
		return
	}
	session, err := h.d.Stripe.CreateCheckoutSession(r.Context(), stripebilling.CheckoutInput{
		OrgID:      org.ID,
		OrgSlug:    org.Slug,
		CustomerID: state.StripeCustomerID.String,
		SeatCount:  int64(seats),
		SuccessURL: h.billingReturnURL(org.Slug, h.d.StripeSuccessURL, "/organizations/"+org.Slug+"/billing/success"),
		CancelURL:  h.billingReturnURL(org.Slug, h.d.StripeCancelURL, "/organizations/"+org.Slug+"/billing/cancel"),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: create checkout session", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not create the Stripe checkout session.", "")
		return
	}
	http.Redirect(w, r, session.URL, http.StatusSeeOther)
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
		h.d.Logger.ErrorContext(r.Context(), "org billing: create portal session", "org_id", org.ID, "error", err)
		h.renderSettingsBilling(w, r, org, "Could not open the Stripe billing portal right now.", "")
		return
	}
	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

func (h *Handlers) billingSuccess(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=checkout-success", http.StatusSeeOther)
}

func (h *Handlers) billingCancel(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, orgBillingSettingsPath(org.Slug)+"?notice=checkout-canceled", http.StatusSeeOther)
}

func (h *Handlers) renderSettingsBilling(w http.ResponseWriter, r *http.Request, org orgsdb.Org, errMsg, notice string) {
	state, err := orgbilling.GetOrgBillingState(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org billing: load state", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	memberCount, err := orgbilling.CountBillableOrgMembers(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: count members", "org_id", org.ID, "error", err)
		memberCount = int(state.BillableSeats)
	}
	invoices, err := orgbilling.ListInvoicesForOrg(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, org.ID, 10)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org billing: list invoices", "org_id", org.ID, "error", err)
		invoices = nil
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
		"Summary":               billingSummary(state, memberCount),
		"CanStartCheckout":      h.billingConfigured(),
		"CanManageSubscription": h.billingConfigured() && state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "",
		"GracePeriodLabel":      formatGracePeriod(h.d.BillingGracePeriod),
		"Invoices":              billingInvoiceViews(invoices),
	})
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

func billingNotice(code string) string {
	switch code {
	case "checkout-success":
		return "Checkout completed. Stripe will finish provisioning as webhook events arrive."
	case "checkout-canceled":
		return "Checkout canceled."
	default:
		return ""
	}
}

func billingSummary(state orgbilling.State, memberCount int) []billingSummaryItem {
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
			Label:  "Billable members",
			Value:  fmt.Sprintf("%d", memberCount),
			Detail: billingSeatDetail(state),
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
	if state.Plan == orgbilling.PlanFree {
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

func billingSeatDetail(state orgbilling.State) string {
	if state.SeatSnapshotAt.Valid {
		return fmt.Sprintf("Latest billed seat snapshot: %d captured %s", state.BillableSeats, state.SeatSnapshotAt.Time.Format("Jan 2, 2006"))
	}
	if state.BillableSeats > 0 {
		return fmt.Sprintf("Latest billed seat snapshot: %d", state.BillableSeats)
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
		return state.StripeCustomerID.String
	}
	return "Checkout creates a customer record the first time this organization upgrades."
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
