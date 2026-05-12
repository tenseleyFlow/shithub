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
