// SPDX-License-Identifier: AGPL-3.0-or-later

package stripebilling

import (
	"errors"
	"fmt"
	"testing"

	stripeapi "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

func TestNewValidatesRequiredConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); !errors.Is(err, ErrSecretKeyRequired) {
		t.Fatalf("New without secret key: got %v", err)
	}
	if _, err := New(Config{SecretKey: "sk_test_123"}); !errors.Is(err, ErrWebhookSecretRequired) {
		t.Fatalf("New without webhook secret: got %v", err)
	}
	if _, err := New(Config{SecretKey: "sk_test_123", WebhookSecret: "whsec_123"}); !errors.Is(err, ErrTeamPriceRequired) {
		t.Fatalf("New without price id: got %v", err)
	}
}

func TestSupportsProReflectsConfig(t *testing.T) {
	t.Parallel()
	teamOnly, err := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: "whsec_123",
		TeamPriceID:   "price_team",
	})
	if err != nil {
		t.Fatalf("New team-only: %v", err)
	}
	if teamOnly.SupportsPro() {
		t.Errorf("SupportsPro should be false when ProPriceID empty")
	}

	withPro, err := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: "whsec_123",
		TeamPriceID:   "price_team",
		ProPriceID:    "price_pro",
	})
	if err != nil {
		t.Fatalf("New with pro: %v", err)
	}
	if !withPro.SupportsPro() {
		t.Errorf("SupportsPro should be true when ProPriceID set")
	}
}

func TestNormalizeSubjectLegacyOrgOnly(t *testing.T) {
	t.Parallel()
	kind, id, label, err := normalizeSubject("", 0, "", 42, "acme")
	if err != nil {
		t.Fatalf("legacy org-only: %v", err)
	}
	if kind != SubjectKindOrg || id != 42 || label != "acme" {
		t.Errorf("legacy normalize: kind=%s id=%d label=%q", kind, id, label)
	}
}

func TestNormalizeSubjectExplicitUser(t *testing.T) {
	t.Parallel()
	kind, id, label, err := normalizeSubject(SubjectKindUser, 7, "alice", 0, "")
	if err != nil {
		t.Fatalf("explicit user: %v", err)
	}
	if kind != SubjectKindUser || id != 7 || label != "alice" {
		t.Errorf("user normalize: kind=%s id=%d label=%q", kind, id, label)
	}
}

func TestNormalizeSubjectRejectsBogusKind(t *testing.T) {
	t.Parallel()
	if _, _, _, err := normalizeSubject("alien", 1, "x", 0, ""); !errors.Is(err, ErrInvalidSubjectKind) {
		t.Fatalf("expected ErrInvalidSubjectKind, got %v", err)
	}
}

func TestNormalizeSubjectRequiresIDOrOrgFallback(t *testing.T) {
	t.Parallel()
	// User kind without an ID is invalid.
	if _, _, _, err := normalizeSubject(SubjectKindUser, 0, "", 0, ""); !errors.Is(err, ErrInvalidSubjectKind) {
		t.Fatalf("user without id: expected ErrInvalidSubjectKind, got %v", err)
	}
	// Org kind with zero SubjectID but OrgID set falls back.
	kind, id, _, err := normalizeSubject(SubjectKindOrg, 0, "acme", 99, "acme")
	if err != nil {
		t.Fatalf("org fallback: %v", err)
	}
	if kind != SubjectKindOrg || id != 99 {
		t.Errorf("org fallback: kind=%s id=%d", kind, id)
	}
}

func TestSubjectMetadataOrgKindIncludesLegacyKeys(t *testing.T) {
	t.Parallel()
	m := subjectMetadata(SubjectKindOrg, 42, "acme", 42, "acme")
	if m[MetadataSubjectKind] != "org" || m[MetadataSubjectID] != "42" {
		t.Errorf("PRO04 keys missing for org: %+v", m)
	}
	if m[MetadataOrgID] != "42" || m[MetadataOrgSlug] != "acme" {
		t.Errorf("legacy keys missing for org: %+v", m)
	}
}

func TestSubjectMetadataUserKindOmitsLegacyOrgKeys(t *testing.T) {
	t.Parallel()
	m := subjectMetadata(SubjectKindUser, 7, "alice", 0, "")
	if m[MetadataSubjectKind] != "user" || m[MetadataSubjectID] != "7" {
		t.Errorf("PRO04 keys missing for user: %+v", m)
	}
	if _, ok := m[MetadataOrgID]; ok {
		t.Errorf("user metadata should omit MetadataOrgID; got %+v", m)
	}
	if _, ok := m[MetadataOrgSlug]; ok {
		t.Errorf("user metadata should omit MetadataOrgSlug; got %+v", m)
	}
}

func TestTeamSeatPreviewParamsRequireStripeIdentifiers(t *testing.T) {
	t.Parallel()
	valid := TeamSeatPreviewInput{
		CustomerID:         "cus_test",
		SubscriptionID:     "sub_test",
		SubscriptionItemID: "si_test",
		NewQuantity:        3,
		ProrationDate:      1710000000,
	}
	cases := []struct {
		name string
		mut  func(*TeamSeatPreviewInput)
		want error
	}{
		{name: "customer", mut: func(in *TeamSeatPreviewInput) { in.CustomerID = "" }, want: ErrCustomerIDRequired},
		{name: "subscription", mut: func(in *TeamSeatPreviewInput) { in.SubscriptionID = "" }, want: ErrSubscriptionID},
		{name: "subscription item", mut: func(in *TeamSeatPreviewInput) { in.SubscriptionItemID = "" }, want: ErrSubscriptionItemID},
		{name: "quantity", mut: func(in *TeamSeatPreviewInput) { in.NewQuantity = 0 }, want: ErrInvalidSeatQuantity},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := valid
			tc.mut(&in)
			if _, err := teamSeatPreviewParams(in); !errors.Is(err, tc.want) {
				t.Fatalf("teamSeatPreviewParams err=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestTeamSeatPreviewParamsBuildsSubscriptionUpdatePreview(t *testing.T) {
	t.Parallel()
	params, err := teamSeatPreviewParams(TeamSeatPreviewInput{
		CustomerID:         " cus_test ",
		SubscriptionID:     " sub_test ",
		SubscriptionItemID: " si_test ",
		NewQuantity:        4,
		ProrationDate:      1710000000,
	})
	if err != nil {
		t.Fatalf("teamSeatPreviewParams: %v", err)
	}
	if params.Customer == nil || *params.Customer != "cus_test" {
		t.Fatalf("customer param = %v", params.Customer)
	}
	if params.Subscription == nil || *params.Subscription != "sub_test" {
		t.Fatalf("subscription param = %v", params.Subscription)
	}
	details := params.SubscriptionDetails
	if details == nil || details.ProrationBehavior == nil || *details.ProrationBehavior != "create_prorations" {
		t.Fatalf("subscription details missing create_prorations: %+v", details)
	}
	if details.ProrationDate == nil || *details.ProrationDate != 1710000000 {
		t.Fatalf("proration date = %v", details.ProrationDate)
	}
	if len(details.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(details.Items))
	}
	item := details.Items[0]
	if item.ID == nil || *item.ID != "si_test" || item.Quantity == nil || *item.Quantity != 4 {
		t.Fatalf("item params = %+v", item)
	}
}

func TestTeamSeatChangeParamsRequireExplicitIdempotency(t *testing.T) {
	t.Parallel()
	if _, err := teamSeatChangeParams(TeamSeatChangeInput{
		SubscriptionItemID: "si_test",
		NewQuantity:        4,
	}); !errors.Is(err, ErrIdempotencyKey) {
		t.Fatalf("teamSeatChangeParams without idempotency err=%v, want %v", err, ErrIdempotencyKey)
	}
	params, err := teamSeatChangeParams(TeamSeatChangeInput{
		SubscriptionItemID: " si_test ",
		NewQuantity:        4,
		ProrationDate:      1710000000,
		IdempotencyKey:     " seat-change-key ",
	})
	if err != nil {
		t.Fatalf("teamSeatChangeParams: %v", err)
	}
	if params.Quantity == nil || *params.Quantity != 4 {
		t.Fatalf("quantity = %v", params.Quantity)
	}
	if params.ProrationBehavior == nil || *params.ProrationBehavior != "create_prorations" {
		t.Fatalf("proration behavior = %v", params.ProrationBehavior)
	}
	if params.ProrationDate == nil || *params.ProrationDate != 1710000000 {
		t.Fatalf("proration date = %v", params.ProrationDate)
	}
	if params.IdempotencyKey == nil || *params.IdempotencyKey != "seat-change-key" {
		t.Fatalf("idempotency key = %v", params.IdempotencyKey)
	}
}

func TestTeamSeatPreviewFromStripeInvoiceSumsProrationLines(t *testing.T) {
	t.Parallel()
	preview := teamSeatPreviewFromStripeInvoice(&stripeapi.Invoice{
		Currency:  stripeapi.Currency("usd"),
		AmountDue: 800,
		Subtotal:  1400,
		Total:     1200,
		Lines: &stripeapi.InvoiceLineItemList{Data: []*stripeapi.InvoiceLineItem{
			{
				Amount: 300,
				Parent: &stripeapi.InvoiceLineItemParent{
					SubscriptionItemDetails: &stripeapi.InvoiceLineItemParentSubscriptionItemDetails{Proration: true},
				},
			},
			{
				Amount: -100,
				Parent: &stripeapi.InvoiceLineItemParent{
					InvoiceItemDetails: &stripeapi.InvoiceLineItemParentInvoiceItemDetails{Proration: true},
				},
			},
			{Amount: 1000},
		}},
	}, 1710000000)
	if preview.Currency != "usd" || preview.AmountDue != 800 || preview.Subtotal != 1400 || preview.Total != 1200 {
		t.Fatalf("invoice totals not copied: %+v", preview)
	}
	if preview.CurrentPeriodAmount != 200 {
		t.Fatalf("CurrentPeriodAmount=%d, want 200", preview.CurrentPeriodAmount)
	}
	if preview.ProrationDate != 1710000000 {
		t.Fatalf("ProrationDate=%d", preview.ProrationDate)
	}
}

func TestVerifyWebhookUsesSigningSecret(t *testing.T) {
	t.Parallel()
	client, err := New(Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: "whsec_test",
		TeamPriceID:   "price_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte(fmt.Sprintf(`{"id":"evt_test","object":"event","api_version":%q,"type":"customer.subscription.updated","data":{"object":{"id":"sub_test","object":"subscription"}}}`, stripeapi.APIVersion))
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payload,
		Secret:  "whsec_test",
	})
	event, err := client.VerifyWebhook(payload, signed.Header)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.ID != "evt_test" || event.Type != "customer.subscription.updated" {
		t.Fatalf("unexpected event: id=%s type=%s", event.ID, event.Type)
	}
	if _, err := client.VerifyWebhook(payload, "t=1,v1=bad"); err == nil {
		t.Fatalf("VerifyWebhook accepted bad signature")
	}
}
