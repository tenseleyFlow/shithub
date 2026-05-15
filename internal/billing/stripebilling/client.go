// SPDX-License-Identifier: AGPL-3.0-or-later

// Package stripebilling contains the Stripe-specific edge of the billing
// system. Local subscription state stays in internal/billing; this package
// owns hosted Checkout, Billing Portal, seat quantity updates, and webhook
// signature verification.
package stripebilling

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	stripeapi "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

const (
	// MetadataOrgID and MetadataOrgSlug are the legacy SP03 keys.
	// PRO04 introduces MetadataSubjectKind and MetadataSubjectID;
	// new subscriptions stamp both for forward compatibility, the
	// webhook resolver falls back to the legacy keys for org rows
	// created before PRO04 deployed.
	MetadataOrgID        = "shithub_org_id"
	MetadataOrgSlug      = "shithub_org_slug"
	MetadataSubjectKind  = "shithub_subject_kind"
	MetadataSubjectID    = "shithub_subject_id"
	MetadataSubjectLabel = "shithub_subject_label" // human-readable, e.g. org slug or username
	MetadataSeatCount    = "shithub_seat_count"
)

// SubjectKind mirrors billing.SubjectKind without taking a hard
// dependency on the billing package (avoid import cycle).
// Conversions live in the webhook handler.
type SubjectKind string

const (
	SubjectKindUser SubjectKind = "user"
	SubjectKindOrg  SubjectKind = "org"
)

var (
	ErrSecretKeyRequired     = errors.New("stripe billing: secret key is required")
	ErrWebhookSecretRequired = errors.New("stripe billing: webhook secret is required")
	ErrTeamPriceRequired     = errors.New("stripe billing: team price id is required")
	ErrProPriceRequired      = errors.New("stripe billing: pro price id is required")
	ErrCustomerIDRequired    = errors.New("stripe billing: customer id is required")
	ErrSubscriptionID        = errors.New("stripe billing: subscription id is required")
	ErrSubscriptionItemID    = errors.New("stripe billing: subscription item id is required")
	ErrInvalidSeatQuantity   = errors.New("stripe billing: seat quantity must be positive")
	ErrIdempotencyKey        = errors.New("stripe billing: idempotency key is required")
	ErrURLRequired           = errors.New("stripe billing: redirect url is required")
	ErrInvalidSubjectKind    = errors.New("stripe billing: invalid subject kind")
)

type Config struct {
	SecretKey     string
	WebhookSecret string
	TeamPriceID   string
	ProPriceID    string // PRO04: required once user-tier path is enabled
	AutomaticTax  bool
}

type Remote interface {
	CreateCustomer(context.Context, CustomerInput) (Customer, error)
	CreateCheckoutSession(context.Context, CheckoutInput) (CheckoutSession, error)
	CreatePortalSession(context.Context, PortalInput) (PortalSession, error)
	PreviewTeamSeatChange(context.Context, TeamSeatPreviewInput) (TeamSeatPreview, error)
	ApplyTeamSeatChange(context.Context, TeamSeatChangeInput) error
	FetchSubscriptionItemQuantity(context.Context, string) (int64, error)
	UpdateSubscriptionItemQuantity(context.Context, SeatQuantityInput) error
	VerifyWebhook(payload []byte, signatureHeader string) (stripeapi.Event, error)
}

type Client struct {
	stripe        *stripeapi.Client
	webhookSecret string
	teamPriceID   string
	proPriceID    string
	automaticTax  bool
}

// CustomerInput carries enough subject context to populate Stripe
// metadata for new customer records. The legacy `OrgID`/`OrgSlug`
// pair stays for backward compatibility with SP03 callers; PRO04
// callers populate `Kind` + `SubjectID` + `Label`, and the
// metadata stamps both old and new keys.
type CustomerInput struct {
	OrgID   int64
	OrgSlug string
	OrgName string
	Email   string

	Kind      SubjectKind // PRO04: "user" | "org"
	SubjectID int64       // PRO04: user id or org id
	Label     string      // PRO04: human-readable (org slug or username)
}

type Customer struct {
	ID string
}

type CheckoutInput struct {
	OrgID      int64
	OrgSlug    string
	CustomerID string
	SeatCount  int64
	SuccessURL string
	CancelURL  string

	Kind      SubjectKind // PRO04: routes to TeamPriceID (org) or ProPriceID (user)
	SubjectID int64       // PRO04: user id or org id
	Label     string      // PRO04: human-readable for metadata.shithub_subject_label
}

type CheckoutSession struct {
	ID  string
	URL string
}

type PortalInput struct {
	CustomerID string
	ReturnURL  string
}

type PortalSession struct {
	ID  string
	URL string
}

type SeatQuantityInput struct {
	OrgID              int64
	SubscriptionItemID string
	Quantity           int64
}

type TeamSeatPreviewInput struct {
	CustomerID         string
	SubscriptionID     string
	SubscriptionItemID string
	NewQuantity        int64
	ProrationDate      int64
}

type TeamSeatPreview struct {
	Currency            string
	CurrentPeriodAmount int64
	AmountDue           int64
	Total               int64
	Subtotal            int64
	ProrationDate       int64
}

type TeamSeatChangeInput struct {
	OrgID              int64
	SubscriptionItemID string
	NewQuantity        int64
	ProrationDate      int64
	IdempotencyKey     string
}

func New(cfg Config) (*Client, error) {
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	cfg.TeamPriceID = strings.TrimSpace(cfg.TeamPriceID)
	cfg.ProPriceID = strings.TrimSpace(cfg.ProPriceID)
	if cfg.SecretKey == "" {
		return nil, ErrSecretKeyRequired
	}
	if cfg.WebhookSecret == "" {
		return nil, ErrWebhookSecretRequired
	}
	if cfg.TeamPriceID == "" {
		return nil, ErrTeamPriceRequired
	}
	// ProPriceID is optional at construction (operators on the
	// SP-only path don't have it). Pro-path callers will get
	// ErrProPriceRequired at CheckoutSession time if they try to
	// route a user-kind checkout against a client that lacks the
	// price id.
	return &Client{
		stripe:        stripeapi.NewClient(cfg.SecretKey),
		webhookSecret: cfg.WebhookSecret,
		teamPriceID:   cfg.TeamPriceID,
		proPriceID:    cfg.ProPriceID,
		automaticTax:  cfg.AutomaticTax,
	}, nil
}

// SupportsPro reports whether the client was configured with a Pro
// price. Wiring code uses this to decide whether to register the
// user-tier checkout/portal routes; refusing the routes when Pro
// isn't configured keeps the operator-disabled path consistent.
func (c *Client) SupportsPro() bool { return c.proPriceID != "" }

func (c *Client) CreateCustomer(ctx context.Context, in CustomerInput) (Customer, error) {
	// PRO04: subject-aware customer creation. When `in.Kind` is set
	// the new metadata keys + idempotency-key prefix include the
	// kind; legacy SP03 callers (no Kind set) keep the org-only
	// behavior so existing org customers don't get duplicated.
	kind, subjectID, label, err := normalizeSubject(in.Kind, in.SubjectID, in.Label, in.OrgID, in.OrgSlug)
	if err != nil {
		return Customer{}, err
	}
	name := strings.TrimSpace(in.OrgName)
	if name == "" {
		name = label
	}
	descriptor := fmt.Sprintf("shithub %s %s", kind, label)
	params := &stripeapi.CustomerCreateParams{
		Name:        stripeapi.String(name),
		Description: stripeapi.String(descriptor),
		Metadata:    subjectMetadata(kind, subjectID, label, in.OrgID, in.OrgSlug),
	}
	if email := strings.TrimSpace(in.Email); email != "" {
		params.Email = stripeapi.String(email)
	}
	params.SetIdempotencyKey(idempotencyKey("customer", string(kind), subjectID, "v1"))
	customer, err := c.stripe.V1Customers.Create(ctx, params)
	if err != nil {
		return Customer{}, err
	}
	return Customer{ID: customer.ID}, nil
}

func (c *Client) CreateCheckoutSession(ctx context.Context, in CheckoutInput) (CheckoutSession, error) {
	in.CustomerID = strings.TrimSpace(in.CustomerID)
	if in.CustomerID == "" {
		return CheckoutSession{}, ErrCustomerIDRequired
	}
	in.SuccessURL = strings.TrimSpace(in.SuccessURL)
	if in.SuccessURL == "" {
		return CheckoutSession{}, fmt.Errorf("%w: success_url", ErrURLRequired)
	}
	in.CancelURL = strings.TrimSpace(in.CancelURL)
	if in.CancelURL == "" {
		return CheckoutSession{}, fmt.Errorf("%w: cancel_url", ErrURLRequired)
	}
	kind, subjectID, label, err := normalizeSubject(in.Kind, in.SubjectID, in.Label, in.OrgID, in.OrgSlug)
	if err != nil {
		return CheckoutSession{}, err
	}
	// Route price + quantity by kind. Pro is single-seat; Team
	// quantity equals the caller-provided seat count.
	var priceID string
	var quantity int64
	switch kind {
	case SubjectKindOrg:
		priceID = c.teamPriceID
		quantity = in.SeatCount
		if quantity < 1 {
			quantity = 1
		}
	case SubjectKindUser:
		if c.proPriceID == "" {
			return CheckoutSession{}, ErrProPriceRequired
		}
		priceID = c.proPriceID
		quantity = 1
	default:
		return CheckoutSession{}, fmt.Errorf("%w: %q", ErrInvalidSubjectKind, kind)
	}
	metadata := subjectMetadata(kind, subjectID, label, in.OrgID, in.OrgSlug)
	if kind == SubjectKindOrg {
		metadata[MetadataSeatCount] = strconv.FormatInt(quantity, 10)
	}
	mode := string(stripeapi.CheckoutSessionModeSubscription)
	paymentMethodCollection := string(stripeapi.CheckoutSessionPaymentMethodCollectionAlways)
	billingAddressCollection := string(stripeapi.CheckoutSessionBillingAddressCollectionAuto)
	params := &stripeapi.CheckoutSessionCreateParams{
		Mode:                     stripeapi.String(mode),
		Customer:                 stripeapi.String(in.CustomerID),
		ClientReferenceID:        stripeapi.String(strconv.FormatInt(subjectID, 10)),
		SuccessURL:               stripeapi.String(in.SuccessURL),
		CancelURL:                stripeapi.String(in.CancelURL),
		PaymentMethodCollection:  stripeapi.String(paymentMethodCollection),
		BillingAddressCollection: stripeapi.String(billingAddressCollection),
		LineItems: []*stripeapi.CheckoutSessionCreateLineItemParams{{
			Price:    stripeapi.String(priceID),
			Quantity: stripeapi.Int64(quantity),
		}},
		Metadata: metadata,
		SubscriptionData: &stripeapi.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: metadata,
		},
	}
	if c.automaticTax {
		params.AutomaticTax = &stripeapi.CheckoutSessionCreateAutomaticTaxParams{
			Enabled: stripeapi.Bool(true),
		}
	}
	params.SetIdempotencyKey(idempotencyKey("checkout", string(kind), subjectID, planLabelForKind(kind), strconv.FormatInt(quantity, 10)))
	session, err := c.stripe.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return CheckoutSession{}, err
	}
	return CheckoutSession{ID: session.ID, URL: session.URL}, nil
}

func (c *Client) CreatePortalSession(ctx context.Context, in PortalInput) (PortalSession, error) {
	in.CustomerID = strings.TrimSpace(in.CustomerID)
	if in.CustomerID == "" {
		return PortalSession{}, ErrCustomerIDRequired
	}
	in.ReturnURL = strings.TrimSpace(in.ReturnURL)
	if in.ReturnURL == "" {
		return PortalSession{}, fmt.Errorf("%w: portal_return_url", ErrURLRequired)
	}
	params := &stripeapi.BillingPortalSessionCreateParams{
		Customer:  stripeapi.String(in.CustomerID),
		ReturnURL: stripeapi.String(in.ReturnURL),
	}
	session, err := c.stripe.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return PortalSession{}, err
	}
	return PortalSession{ID: session.ID, URL: session.URL}, nil
}

func (c *Client) PreviewTeamSeatChange(ctx context.Context, in TeamSeatPreviewInput) (TeamSeatPreview, error) {
	params, err := teamSeatPreviewParams(in)
	if err != nil {
		return TeamSeatPreview{}, err
	}
	invoice, err := c.stripe.V1Invoices.CreatePreview(ctx, params)
	if err != nil {
		return TeamSeatPreview{}, err
	}
	return teamSeatPreviewFromStripeInvoice(invoice, in.ProrationDate), nil
}

func (c *Client) ApplyTeamSeatChange(ctx context.Context, in TeamSeatChangeInput) error {
	params, err := teamSeatChangeParams(in)
	if err != nil {
		return err
	}
	_, err = c.stripe.V1SubscriptionItems.Update(ctx, strings.TrimSpace(in.SubscriptionItemID), params)
	return err
}

func (c *Client) FetchSubscriptionItemQuantity(ctx context.Context, subscriptionItemID string) (int64, error) {
	subscriptionItemID = strings.TrimSpace(subscriptionItemID)
	if subscriptionItemID == "" {
		return 0, ErrSubscriptionItemID
	}
	item, err := c.stripe.V1SubscriptionItems.Retrieve(ctx, subscriptionItemID, nil)
	if err != nil {
		return 0, err
	}
	return item.Quantity, nil
}

func (c *Client) UpdateSubscriptionItemQuantity(ctx context.Context, in SeatQuantityInput) error {
	in.SubscriptionItemID = strings.TrimSpace(in.SubscriptionItemID)
	if in.SubscriptionItemID == "" {
		return ErrSubscriptionItemID
	}
	if in.Quantity < 1 {
		in.Quantity = 1
	}
	params := &stripeapi.SubscriptionItemUpdateParams{
		Quantity: stripeapi.Int64(in.Quantity),
	}
	params.SetIdempotencyKey(idempotencyKey("seat-sync", in.OrgID, in.SubscriptionItemID, strconv.FormatInt(in.Quantity, 10)))
	_, err := c.stripe.V1SubscriptionItems.Update(ctx, in.SubscriptionItemID, params)
	return err
}

func TeamSeatChangeIdempotencyKey(orgID int64, subscriptionItemID string, newQuantity, prorationDate int64) string {
	return idempotencyKey("team-seat-change", orgID, subscriptionItemID, strconv.FormatInt(newQuantity, 10), strconv.FormatInt(prorationDate, 10))
}

func teamSeatPreviewParams(in TeamSeatPreviewInput) (*stripeapi.InvoiceCreatePreviewParams, error) {
	in.CustomerID = strings.TrimSpace(in.CustomerID)
	if in.CustomerID == "" {
		return nil, ErrCustomerIDRequired
	}
	in.SubscriptionID = strings.TrimSpace(in.SubscriptionID)
	if in.SubscriptionID == "" {
		return nil, ErrSubscriptionID
	}
	in.SubscriptionItemID = strings.TrimSpace(in.SubscriptionItemID)
	if in.SubscriptionItemID == "" {
		return nil, ErrSubscriptionItemID
	}
	if in.NewQuantity < 1 {
		return nil, ErrInvalidSeatQuantity
	}
	params := &stripeapi.InvoiceCreatePreviewParams{
		Customer:     stripeapi.String(in.CustomerID),
		Subscription: stripeapi.String(in.SubscriptionID),
		SubscriptionDetails: &stripeapi.InvoiceCreatePreviewSubscriptionDetailsParams{
			Items: []*stripeapi.InvoiceCreatePreviewSubscriptionDetailsItemParams{{
				ID:       stripeapi.String(in.SubscriptionItemID),
				Quantity: stripeapi.Int64(in.NewQuantity),
			}},
			ProrationBehavior: stripeapi.String("create_prorations"),
		},
	}
	if in.ProrationDate > 0 {
		params.SubscriptionDetails.ProrationDate = stripeapi.Int64(in.ProrationDate)
	}
	return params, nil
}

func teamSeatChangeParams(in TeamSeatChangeInput) (*stripeapi.SubscriptionItemUpdateParams, error) {
	in.SubscriptionItemID = strings.TrimSpace(in.SubscriptionItemID)
	if in.SubscriptionItemID == "" {
		return nil, ErrSubscriptionItemID
	}
	if in.NewQuantity < 1 {
		return nil, ErrInvalidSeatQuantity
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.IdempotencyKey == "" {
		return nil, ErrIdempotencyKey
	}
	params := &stripeapi.SubscriptionItemUpdateParams{
		Quantity:          stripeapi.Int64(in.NewQuantity),
		ProrationBehavior: stripeapi.String("create_prorations"),
	}
	if in.ProrationDate > 0 {
		params.ProrationDate = stripeapi.Int64(in.ProrationDate)
	}
	params.SetIdempotencyKey(in.IdempotencyKey)
	return params, nil
}

func teamSeatPreviewFromStripeInvoice(invoice *stripeapi.Invoice, prorationDate int64) TeamSeatPreview {
	if invoice == nil {
		return TeamSeatPreview{ProrationDate: prorationDate}
	}
	return TeamSeatPreview{
		Currency:            string(invoice.Currency),
		CurrentPeriodAmount: invoiceProrationAmount(invoice),
		AmountDue:           invoice.AmountDue,
		Total:               invoice.Total,
		Subtotal:            invoice.Subtotal,
		ProrationDate:       prorationDate,
	}
}

func invoiceProrationAmount(invoice *stripeapi.Invoice) int64 {
	if invoice == nil || invoice.Lines == nil {
		return 0
	}
	var total int64
	for _, line := range invoice.Lines.Data {
		if line == nil || line.Parent == nil {
			continue
		}
		if line.Parent.InvoiceItemDetails != nil && line.Parent.InvoiceItemDetails.Proration {
			total += line.Amount
			continue
		}
		if line.Parent.SubscriptionItemDetails != nil && line.Parent.SubscriptionItemDetails.Proration {
			total += line.Amount
		}
	}
	return total
}

func (c *Client) VerifyWebhook(payload []byte, signatureHeader string) (stripeapi.Event, error) {
	return webhook.ConstructEvent(payload, signatureHeader, c.webhookSecret)
}

// normalizeSubject resolves the (kind, id, label) tuple from a
// CheckoutInput / CustomerInput. Legacy SP03 callers populate only
// OrgID/OrgSlug and zero Kind — those default to SubjectKindOrg
// for backward compatibility. PRO04 callers populate Kind +
// SubjectID + Label and may leave OrgID/OrgSlug zero (user kind).
//
// Cross-checks: if both legacy OrgID and explicit user-kind
// SubjectID are set, the explicit Kind wins; the OrgID stays in
// the metadata for audit but the routing key is Kind+SubjectID.
func normalizeSubject(kind SubjectKind, subjectID int64, label string, orgID int64, orgSlug string) (SubjectKind, int64, string, error) {
	if kind == "" {
		// Legacy path: org-only.
		if orgID <= 0 {
			return "", 0, "", fmt.Errorf("%w: %q", ErrInvalidSubjectKind, kind)
		}
		return SubjectKindOrg, orgID, strings.TrimSpace(orgSlug), nil
	}
	if kind != SubjectKindOrg && kind != SubjectKindUser {
		return "", 0, "", fmt.Errorf("%w: %q", ErrInvalidSubjectKind, kind)
	}
	if subjectID <= 0 {
		// Fall back to OrgID for org kind when caller forgot to set
		// SubjectID explicitly; tightens the API without breaking
		// the SP03→PRO04 transition.
		if kind == SubjectKindOrg && orgID > 0 {
			subjectID = orgID
		} else {
			return "", 0, "", fmt.Errorf("%w: subject id required for kind %q", ErrInvalidSubjectKind, kind)
		}
	}
	label = strings.TrimSpace(label)
	if label == "" && kind == SubjectKindOrg {
		label = strings.TrimSpace(orgSlug)
	}
	return kind, subjectID, label, nil
}

// subjectMetadata returns the Stripe metadata map stamped on
// customers, checkout sessions, and subscription objects. Both the
// new MetadataSubject* keys and the legacy MetadataOrg* keys are
// emitted when the kind is org — this keeps SP03-deployed webhook
// resolvers working through the transition. User-kind metadata
// omits the legacy keys (no legacy resolver expected them).
func subjectMetadata(kind SubjectKind, subjectID int64, label string, orgID int64, orgSlug string) map[string]string {
	m := map[string]string{
		MetadataSubjectKind:  string(kind),
		MetadataSubjectID:    strconv.FormatInt(subjectID, 10),
		MetadataSubjectLabel: label,
	}
	if kind == SubjectKindOrg {
		if orgID == 0 {
			orgID = subjectID
		}
		if orgSlug == "" {
			orgSlug = label
		}
		m[MetadataOrgID] = strconv.FormatInt(orgID, 10)
		m[MetadataOrgSlug] = strings.TrimSpace(orgSlug)
	}
	return m
}

// planLabelForKind returns the marketing label baked into the
// idempotency key. Keeps Pro and Team checkouts from colliding on
// the same idempotency string even if a single subject ID exists
// on both subject_kind tables (which the schema prohibits, but
// defense-in-depth).
func planLabelForKind(kind SubjectKind) string {
	switch kind {
	case SubjectKindUser:
		return "pro"
	case SubjectKindOrg:
		return "team"
	}
	return string(kind)
}

func idempotencyKey(parts ...any) string {
	var b strings.Builder
	b.WriteString("shithub")
	for _, part := range parts {
		b.WriteByte(':')
		b.WriteString(strings.NewReplacer(":", "_", " ", "_", "/", "_").Replace(fmt.Sprint(part)))
	}
	return b.String()
}
