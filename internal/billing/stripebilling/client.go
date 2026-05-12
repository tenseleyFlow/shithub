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
	MetadataOrgID   = "shithub_org_id"
	MetadataOrgSlug = "shithub_org_slug"
)

var (
	ErrSecretKeyRequired     = errors.New("stripe billing: secret key is required")
	ErrWebhookSecretRequired = errors.New("stripe billing: webhook secret is required")
	ErrTeamPriceRequired     = errors.New("stripe billing: team price id is required")
	ErrCustomerIDRequired    = errors.New("stripe billing: customer id is required")
	ErrSubscriptionItemID    = errors.New("stripe billing: subscription item id is required")
	ErrURLRequired           = errors.New("stripe billing: redirect url is required")
)

type Config struct {
	SecretKey     string
	WebhookSecret string
	TeamPriceID   string
	AutomaticTax  bool
}

type Remote interface {
	CreateCustomer(context.Context, CustomerInput) (Customer, error)
	CreateCheckoutSession(context.Context, CheckoutInput) (CheckoutSession, error)
	CreatePortalSession(context.Context, PortalInput) (PortalSession, error)
	UpdateSubscriptionItemQuantity(context.Context, SeatQuantityInput) error
	VerifyWebhook(payload []byte, signatureHeader string) (stripeapi.Event, error)
}

type Client struct {
	stripe        *stripeapi.Client
	webhookSecret string
	teamPriceID   string
	automaticTax  bool
}

type CustomerInput struct {
	OrgID   int64
	OrgSlug string
	OrgName string
	Email   string
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

func New(cfg Config) (*Client, error) {
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	cfg.TeamPriceID = strings.TrimSpace(cfg.TeamPriceID)
	if cfg.SecretKey == "" {
		return nil, ErrSecretKeyRequired
	}
	if cfg.WebhookSecret == "" {
		return nil, ErrWebhookSecretRequired
	}
	if cfg.TeamPriceID == "" {
		return nil, ErrTeamPriceRequired
	}
	return &Client{
		stripe:        stripeapi.NewClient(cfg.SecretKey),
		webhookSecret: cfg.WebhookSecret,
		teamPriceID:   cfg.TeamPriceID,
		automaticTax:  cfg.AutomaticTax,
	}, nil
}

func (c *Client) CreateCustomer(ctx context.Context, in CustomerInput) (Customer, error) {
	name := strings.TrimSpace(in.OrgName)
	if name == "" {
		name = strings.TrimSpace(in.OrgSlug)
	}
	params := &stripeapi.CustomerCreateParams{
		Name:        stripeapi.String(name),
		Description: stripeapi.String(fmt.Sprintf("shithub organization %s", strings.TrimSpace(in.OrgSlug))),
		Metadata:    orgMetadata(in.OrgID, in.OrgSlug),
	}
	if email := strings.TrimSpace(in.Email); email != "" {
		params.Email = stripeapi.String(email)
	}
	params.SetIdempotencyKey(idempotencyKey("customer", in.OrgID, "v1"))
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
	if in.SeatCount < 1 {
		in.SeatCount = 1
	}
	metadata := orgMetadata(in.OrgID, in.OrgSlug)
	mode := string(stripeapi.CheckoutSessionModeSubscription)
	paymentMethodCollection := string(stripeapi.CheckoutSessionPaymentMethodCollectionAlways)
	billingAddressCollection := string(stripeapi.CheckoutSessionBillingAddressCollectionAuto)
	params := &stripeapi.CheckoutSessionCreateParams{
		Mode:                     stripeapi.String(mode),
		Customer:                 stripeapi.String(in.CustomerID),
		ClientReferenceID:        stripeapi.String(strconv.FormatInt(in.OrgID, 10)),
		SuccessURL:               stripeapi.String(in.SuccessURL),
		CancelURL:                stripeapi.String(in.CancelURL),
		PaymentMethodCollection:  stripeapi.String(paymentMethodCollection),
		BillingAddressCollection: stripeapi.String(billingAddressCollection),
		LineItems: []*stripeapi.CheckoutSessionCreateLineItemParams{{
			Price:    stripeapi.String(c.teamPriceID),
			Quantity: stripeapi.Int64(in.SeatCount),
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
	params.SetIdempotencyKey(idempotencyKey("checkout", in.OrgID, "team", strconv.FormatInt(in.SeatCount, 10)))
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

func (c *Client) VerifyWebhook(payload []byte, signatureHeader string) (stripeapi.Event, error) {
	return webhook.ConstructEvent(payload, signatureHeader, c.webhookSecret)
}

func orgMetadata(orgID int64, orgSlug string) map[string]string {
	return map[string]string{
		MetadataOrgID:   strconv.FormatInt(orgID, 10),
		MetadataOrgSlug: strings.TrimSpace(orgSlug),
	}
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
