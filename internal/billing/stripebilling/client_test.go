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
