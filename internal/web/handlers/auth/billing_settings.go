// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	userbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO06 — user-tier billing settings.
//
// Mirrors `internal/web/handlers/orgs/billing_settings.go` minus
// seat-specific machinery (Pro is a single-seat plan) and minus
// the private-collaboration breakdown (PRO01 ratified no Free user
// collaborator cap). The view types are clone-and-pruned rather
// than extracted to a shared package; PRO06's spec says the
// extraction lands if/when the copies diverge meaningfully. For
// now they're identical-shape but distinct types so user-side
// changes don't ripple into the org page.

type userBillingSummaryItem struct {
	Label  string
	Value  string
	Detail string
}

type userBillingInvoiceView struct {
	Number           string
	StatusLabel      string
	StatusClass      string
	AmountLabel      string
	PeriodLabel      string
	DueLabel         string
	HostedInvoiceURL string
	InvoicePDFURL    string
}

type userBillingAlert struct {
	Class      string
	Message    string
	ActionText string
	ActionHref string
}

type userBillingDebugView struct {
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
}

const userBillingSettingsPath = "/settings/billing"

// settingsBilling renders the user's billing page. Auth middleware
// has already verified the request is authenticated; this handler
// loads the current user's billing state and projects it for the
// template.
func (h *Handlers) settingsBilling(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.ID == 0 {
		http.Redirect(w, r, "/login?return_to=/settings/billing", http.StatusSeeOther)
		return
	}
	h.renderUserSettingsBilling(w, r, user.ID, user.Username, "", userBillingNotice(r.URL.Query().Get("notice")))
}

// settingsBillingCheckout creates a Stripe Checkout session for
// the authenticated user upgrading to Pro and redirects to it.
// Returns 404 when billing isn't configured on this instance.
func (h *Handlers) settingsBillingCheckout(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.ID == 0 {
		http.Redirect(w, r, "/login?return_to=/settings/billing", http.StatusSeeOther)
		return
	}
	if !h.billingConfiguredForUser() {
		http.NotFound(w, r)
		return
	}
	sessionURL, err := h.startUserBillingCheckout(r, user.ID, user.Username)
	if err != nil {
		metrics.BillingCheckoutSessionsTotal.WithLabelValues("user", "failure").Inc()
		h.d.Logger.ErrorContext(r.Context(), "user billing: create checkout", "user_id", user.ID, "error", err)
		h.renderUserSettingsBilling(w, r, user.ID, user.Username, "Could not start checkout right now.", "")
		return
	}
	metrics.BillingCheckoutSessionsTotal.WithLabelValues("user", "success").Inc()
	http.Redirect(w, r, sessionURL, http.StatusSeeOther)
}

// settingsBillingPortal opens the Stripe billing portal for the
// authenticated user's customer record. 404 when billing isn't
// configured; renders an inline message when the user has no
// customer record yet (haven't completed checkout).
func (h *Handlers) settingsBillingPortal(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.ID == 0 {
		http.Redirect(w, r, "/login?return_to=/settings/billing", http.StatusSeeOther)
		return
	}
	if !h.billingConfiguredForUser() {
		http.NotFound(w, r)
		return
	}
	state, err := userbilling.GetUserBillingState(r.Context(), userbilling.Deps{Pool: h.d.Pool}, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "user billing: load state for portal", "user_id", user.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !state.StripeCustomerID.Valid || strings.TrimSpace(state.StripeCustomerID.String) == "" {
		h.renderUserSettingsBilling(w, r, user.ID, user.Username, "Billing portal is unavailable until you have a Stripe customer record. Complete checkout once to create one.", "")
		return
	}
	session, err := h.d.Stripe.CreatePortalSession(r.Context(), stripebilling.PortalInput{
		CustomerID: state.StripeCustomerID.String,
		ReturnURL:  h.userBillingReturnURL(h.d.StripePortalReturnURL, userBillingSettingsPath),
	})
	if err != nil {
		metrics.BillingPortalSessionsTotal.WithLabelValues("user", "failure").Inc()
		h.d.Logger.ErrorContext(r.Context(), "user billing: create portal session", "user_id", user.ID, "error", err)
		h.renderUserSettingsBilling(w, r, user.ID, user.Username, "Could not open the Stripe billing portal right now.", "")
		return
	}
	metrics.BillingPortalSessionsTotal.WithLabelValues("user", "success").Inc()
	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

// settingsBillingSuccess renders the post-checkout success page.
// Stripe redirects here after the user completes payment; the user
// billing state may not yet reflect Pro because the webhook hasn't
// fired yet. The page tells the user activation is in progress.
func (h *Handlers) settingsBillingSuccess(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.ID == 0 {
		http.Redirect(w, r, "/login?return_to=/settings/billing", http.StatusSeeOther)
		return
	}
	h.renderUserBillingResult(w, r, user.Username, userBillingResultSuccess)
}

// settingsBillingCancel renders the Stripe-cancel-return page.
// No state has changed; the user is still on Free.
func (h *Handlers) settingsBillingCancel(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.ID == 0 {
		http.Redirect(w, r, "/login?return_to=/settings/billing", http.StatusSeeOther)
		return
	}
	h.renderUserBillingResult(w, r, user.Username, userBillingResultCanceled)
}

const (
	userBillingResultSuccess  = "success"
	userBillingResultCanceled = "canceled"
)

func (h *Handlers) renderUserBillingResult(w http.ResponseWriter, r *http.Request, username, result string) {
	heading := "Checkout complete"
	message := "Stripe accepted the checkout session. Pro activation finishes after shithub receives and processes the signed Stripe webhook. Refresh the billing settings page in a minute if your plan still shows Free."
	if result == userBillingResultCanceled {
		heading = "Checkout canceled"
		message = "No Pro subscription was activated. Your account stays on Free until checkout is completed."
	}
	_ = h.d.Render.RenderPage(w, r, "settings/billing_result", map[string]any{
		"Title":       heading,
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"Username":    username,
		"Result":      result,
		"Heading":     heading,
		"Message":     message,
		"BillingPath": userBillingSettingsPath,
	})
}

// renderUserSettingsBilling is the canonical page render. Pulls
// state, invoices, and (for site admins) the debug projection.
func (h *Handlers) renderUserSettingsBilling(w http.ResponseWriter, r *http.Request, userID int64, username, errMsg, notice string) {
	state, err := userbilling.GetUserBillingState(r.Context(), userbilling.Deps{Pool: h.d.Pool}, userID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "user billing: load state", "user_id", userID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	invoices, err := userbilling.ListInvoicesForPrincipal(r.Context(), userbilling.Deps{Pool: h.d.Pool}, userbilling.PrincipalForUser(userID), 10)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "user billing: list invoices", "user_id", userID, "error", err)
		invoices = nil
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	debug := userBillingDebugView{}
	if viewer.IsSiteAdmin {
		debug = h.userBillingDebugView(r, state)
	}
	_ = h.d.Render.RenderPage(w, r, "settings/billing", map[string]any{
		"Title":            "Billing and plans",
		"SettingsActive":   "billing",
		"CSRFToken":        middleware.CSRFTokenForRequest(r),
		"Username":         username,
		"BillingEnabled":   h.d.BillingEnabled,
		"Error":            errMsg,
		"Notice":           notice,
		"BillingAlert":     userBillingAlertForState(state),
		"Summary":          userBillingSummary(state),
		"CanStartCheckout": h.billingConfiguredForUser(),
		// Gate on StripeSubscriptionID, not StripeCustomerID. A
		// customer record is created when the Checkout Session is
		// minted (before payment); a subscription id only appears
		// after customer.subscription.created lands. Gating on the
		// customer id surfaced "Manage or cancel" buttons for users
		// who abandoned checkout without paying.
		"CanManageSubscription": h.billingConfiguredForUser() && state.StripeSubscriptionID.Valid && strings.TrimSpace(state.StripeSubscriptionID.String) != "",
		"GracePeriodLabel":      userFormatGracePeriod(h.d.BillingGracePeriod),
		"Invoices":              userBillingInvoiceViews(invoices),
		"IsSiteAdmin":           viewer.IsSiteAdmin,
		"Debug":                 debug,
	})
}

// billingConfiguredForUser reports whether the user-tier Pro path
// is wired. Operators that have only configured Team (no
// ProPriceID) will see false here; the Stripe client's SupportsPro
// makes the final call on whether the Pro price is available.
func (h *Handlers) billingConfiguredForUser() bool {
	if !h.d.BillingEnabled || h.d.Stripe == nil {
		return false
	}
	// SupportsPro is exposed on the concrete *stripebilling.Client.
	// The Remote interface in Deps is the test-friendly seam; the
	// concrete check is best-effort and degrades to "billing enabled"
	// when Stripe is a fake implementation. Tests that need the Pro
	// gate populated can use a mock that returns true.
	if checker, ok := h.d.Stripe.(interface{ SupportsPro() bool }); ok {
		return checker.SupportsPro()
	}
	return true
}

// startUserBillingCheckout drives the Stripe checkout flow.
// Ensures a Stripe customer exists for the user (creating one on
// first call), then constructs a kind=user, quantity=1 checkout
// session against the Pro price.
func (h *Handlers) startUserBillingCheckout(r *http.Request, userID int64, username string) (string, error) {
	state, err := userbilling.GetUserBillingState(r.Context(), userbilling.Deps{Pool: h.d.Pool}, userID)
	if err != nil {
		return "", fmt.Errorf("load user billing state: %w", err)
	}
	state, err = h.ensureUserStripeCustomer(r, userID, username, state)
	if err != nil {
		return "", fmt.Errorf("ensure stripe customer: %w", err)
	}
	session, err := h.d.Stripe.CreateCheckoutSession(r.Context(), stripebilling.CheckoutInput{
		Kind:       stripebilling.SubjectKindUser,
		SubjectID:  userID,
		Label:      username,
		CustomerID: state.StripeCustomerID.String,
		SuccessURL: h.userBillingReturnURL(h.d.StripeSuccessURL, "/settings/billing/success"),
		CancelURL:  h.userBillingReturnURL(h.d.StripeCancelURL, "/settings/billing/cancel"),
	})
	if err != nil {
		return "", fmt.Errorf("create stripe checkout session: %w", err)
	}
	return session.URL, nil
}

func (h *Handlers) ensureUserStripeCustomer(r *http.Request, userID int64, username string, state userbilling.UserState) (userbilling.UserState, error) {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return state, nil
	}
	email := ""
	if u, err := h.q.GetUserByID(r.Context(), h.d.Pool, userID); err == nil && u.PrimaryEmailID.Valid {
		if em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, u.PrimaryEmailID.Int64); err == nil {
			email = strings.TrimSpace(em.Email)
		}
	}
	customer, err := h.d.Stripe.CreateCustomer(r.Context(), stripebilling.CustomerInput{
		Kind:      stripebilling.SubjectKindUser,
		SubjectID: userID,
		Label:     username,
		OrgName:   username, // displays as the user's identity in Stripe Dashboard
		Email:     email,
	})
	if err != nil {
		return userbilling.UserState{}, err
	}
	if _, err := userbilling.SetStripeCustomerForPrincipal(r.Context(), userbilling.Deps{Pool: h.d.Pool}, userbilling.PrincipalForUser(userID), customer.ID); err != nil {
		return userbilling.UserState{}, err
	}
	// Reload to get the fresh state with the customer id set.
	return userbilling.GetUserBillingState(r.Context(), userbilling.Deps{Pool: h.d.Pool}, userID)
}

func (h *Handlers) userBillingReturnURL(overrideURL, fallbackPath string) string {
	overrideURL = strings.TrimSpace(overrideURL)
	if overrideURL != "" {
		// User-tier success/cancel URLs don't substitute an org slug.
		// If the operator's template happens to carry {org}, replace
		// with the literal "user" so the URL stays valid rather than
		// leaving the placeholder.
		return strings.ReplaceAll(overrideURL, "{org}", "user")
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

func userBillingNotice(code string) string {
	switch code {
	case "checkout-success":
		return "Checkout completed. Stripe will finish provisioning as webhook events arrive."
	case "checkout-canceled":
		return "Checkout canceled."
	default:
		return ""
	}
}

func (h *Handlers) userBillingDebugView(r *http.Request, state userbilling.UserState) userBillingDebugView {
	debug := userBillingDebugView{
		StripeCustomerID:         userPgTextString(state.StripeCustomerID),
		StripeSubscriptionID:     userPgTextString(state.StripeSubscriptionID),
		StripeSubscriptionItemID: userPgTextString(state.StripeSubscriptionItemID),
		LastWebhookEventID:       strings.TrimSpace(state.LastWebhookEventID),
	}
	if debug.LastWebhookEventID == "" {
		return debug
	}
	receipt, err := userbilling.GetWebhookEventReceipt(r.Context(), userbilling.Deps{Pool: h.d.Pool}, debug.LastWebhookEventID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.WarnContext(r.Context(), "user billing: load latest webhook receipt",
				"event_id", debug.LastWebhookEventID, "error", err)
		}
		return debug
	}
	debug.LastWebhookEventType = receipt.EventType
	debug.LastWebhookReceivedAt = userFormatOptionalTime(receipt.ReceivedAt)
	debug.LastWebhookProcessedAt = userFormatOptionalTime(receipt.ProcessedAt)
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

// ─── Projection helpers ──────────────────────────────────────────

func userBillingSummary(state userbilling.UserState) []userBillingSummaryItem {
	return []userBillingSummaryItem{
		{
			Label:  "Current plan",
			Value:  userBillingPlanLabel(state.Plan),
			Detail: userBillingPlanDetail(state),
		},
		{
			Label:  "Subscription",
			Value:  userBillingStatusLabel(state.SubscriptionStatus),
			Detail: userBillingStatusDetail(state),
		},
		{
			Label:  "Payment source",
			Value:  userBillingPaymentSourceLabel(state),
			Detail: userBillingPaymentSourceDetail(state),
		},
	}
}

func userBillingPlanLabel(plan userbilling.UserPlan) string {
	if plan == userbilling.UserPlanPro {
		return "Pro"
	}
	return "Free"
}

func userBillingPlanDetail(state userbilling.UserState) string {
	if state.CurrentPeriodEnd.Valid {
		label := "Current period ends"
		if state.CancelAtPeriodEnd {
			label = "Scheduled to cancel at period end"
		}
		return label + " " + state.CurrentPeriodEnd.Time.Format("Jan 2, 2006")
	}
	if state.SubscriptionStatus == userbilling.SubscriptionStatusNone ||
		state.SubscriptionStatus == userbilling.SubscriptionStatusCanceled {
		return "No active paid subscription."
	}
	return ""
}

func userBillingStatusLabel(status userbilling.SubscriptionStatus) string {
	switch status {
	case userbilling.SubscriptionStatusActive:
		return "Active"
	case userbilling.SubscriptionStatusTrialing:
		return "Trialing"
	case userbilling.SubscriptionStatusIncomplete:
		return "Incomplete"
	case userbilling.SubscriptionStatusPastDue:
		return "Past due"
	case userbilling.SubscriptionStatusCanceled:
		return "Canceled"
	case userbilling.SubscriptionStatusUnpaid:
		return "Unpaid"
	case userbilling.SubscriptionStatusPaused:
		return "Paused"
	default:
		return "No subscription"
	}
}

func userBillingStatusDetail(state userbilling.UserState) string {
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

func userBillingPaymentSourceLabel(state userbilling.UserState) string {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return "Stripe customer connected"
	}
	return "Not connected"
}

func userBillingPaymentSourceDetail(state userbilling.UserState) string {
	if state.StripeCustomerID.Valid && strings.TrimSpace(state.StripeCustomerID.String) != "" {
		return "Payment method and invoices are managed in Stripe Billing Portal."
	}
	return "Checkout creates a customer record the first time you upgrade to Pro."
}

func userBillingAlertForState(state userbilling.UserState) userBillingAlert {
	switch state.SubscriptionStatus {
	case userbilling.SubscriptionStatusPastDue:
		if state.GraceUntil.Valid && time.Now().UTC().Before(state.GraceUntil.Time) {
			return userBillingAlert{
				Class:      "shithub-flash-notice",
				Message:    "Payment failed. Pro features remain available during the billing grace period, which ends " + state.GraceUntil.Time.Format("Jan 2, 2006") + ".",
				ActionText: "Manage billing",
				ActionHref: userBillingSettingsPath,
			}
		}
		return userBillingAlert{
			Class:      "shithub-flash-error",
			Message:    "Payment is past due. Pro-only features are read-only until billing is brought back into good standing.",
			ActionText: "Manage billing",
			ActionHref: userBillingSettingsPath,
		}
	case userbilling.SubscriptionStatusCanceled:
		return userBillingAlert{
			Class:      "shithub-flash-notice",
			Message:    "Your account is on Free after cancellation. Existing paid configuration is preserved, but Pro-only features are read-only until reactivated.",
			ActionText: "Upgrade to Pro",
			ActionHref: userBillingSettingsPath + "#manage-plan",
		}
	case userbilling.SubscriptionStatusIncomplete, userbilling.SubscriptionStatusUnpaid, userbilling.SubscriptionStatusPaused:
		return userBillingAlert{
			Class:      "shithub-flash-error",
			Message:    "This subscription needs billing action before Pro features are available.",
			ActionText: "Manage billing",
			ActionHref: userBillingSettingsPath,
		}
	default:
		if state.CancelAtPeriodEnd && state.CurrentPeriodEnd.Valid {
			return userBillingAlert{
				Class:      "shithub-flash-notice",
				Message:    "Pro is scheduled to cancel at the end of the current billing period on " + state.CurrentPeriodEnd.Time.Format("Jan 2, 2006") + ".",
				ActionText: "Keep Pro",
				ActionHref: userBillingSettingsPath,
			}
		}
		return userBillingAlert{}
	}
}

func userBillingInvoiceViews(invoices []billingdb.BillingInvoice) []userBillingInvoiceView {
	items := make([]userBillingInvoiceView, 0, len(invoices))
	for _, inv := range invoices {
		number := strings.TrimSpace(inv.Number)
		if number == "" {
			number = inv.StripeInvoiceID
		}
		items = append(items, userBillingInvoiceView{
			Number:           number,
			StatusLabel:      userBillingInvoiceStatusLabel(inv.Status),
			StatusClass:      strings.ReplaceAll(strings.ToLower(string(inv.Status)), "_", "-"),
			AmountLabel:      userFormatCurrencyAmount(inv.Currency, inv.AmountDueCents),
			PeriodLabel:      userBillingPeriodLabel(inv),
			DueLabel:         userBillingDueLabel(inv),
			HostedInvoiceURL: strings.TrimSpace(inv.HostedInvoiceUrl),
			InvoicePDFURL:    strings.TrimSpace(inv.InvoicePdfUrl),
		})
	}
	return items
}

func userBillingInvoiceStatusLabel(status userbilling.InvoiceStatus) string {
	switch status {
	case userbilling.InvoiceStatusOpen:
		return "Open"
	case userbilling.InvoiceStatusPaid:
		return "Paid"
	case userbilling.InvoiceStatusVoid:
		return "Void"
	case userbilling.InvoiceStatusUncollectible:
		return "Uncollectible"
	case userbilling.InvoiceStatusRefunded:
		return "Refunded"
	default:
		return "Draft"
	}
}

func userBillingPeriodLabel(inv billingdb.BillingInvoice) string {
	if inv.PeriodStart.Valid && inv.PeriodEnd.Valid {
		return inv.PeriodStart.Time.Format("Jan 2, 2006") + " - " + inv.PeriodEnd.Time.Format("Jan 2, 2006")
	}
	return "—"
}

func userBillingDueLabel(inv billingdb.BillingInvoice) string {
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

func userFormatGracePeriod(d time.Duration) string {
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

func userPgTextString(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return strings.TrimSpace(v.String)
}

func userFormatOptionalTime(v pgtype.Timestamptz) string {
	if v.Valid && !v.Time.IsZero() {
		return v.Time.UTC().Format("Jan 2, 2006 15:04 UTC")
	}
	return ""
}

func userFormatCurrencyAmount(currency string, cents int64) string {
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
