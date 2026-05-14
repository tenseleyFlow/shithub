// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults_Validate(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate(Defaults()): %v", err)
	}
}

func TestValidate_RejectsBadEnv(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Env = "production" // typo of "prod"
	if err := Validate(&cfg); err == nil {
		t.Errorf("expected validation error for env=production")
	}
}

func TestValidate_RejectsTracingWithoutEndpoint(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Tracing.Enabled = true
	if err := Validate(&cfg); err == nil {
		t.Errorf("expected validation error when tracing.enabled=true and endpoint empty")
	}
}

func TestValidate_RejectsBadSampleRate(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = "http://otel:4318"
	cfg.Tracing.SampleRate = 2.0
	if err := Validate(&cfg); err == nil {
		t.Errorf("expected validation error for sample_rate=2.0")
	}
}

func TestValidate_BillingRequiresStripeSettings(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Billing.Enabled = true
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "billing.stripe.secret_key") {
		t.Fatalf("Validate missing secret key: got %v", err)
	}

	cfg.Billing.Stripe.SecretKey = "sk_test_123"
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "billing.stripe.webhook_secret") {
		t.Fatalf("Validate missing webhook secret: got %v", err)
	}

	cfg.Billing.Stripe.WebhookSecret = "whsec_123"
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "billing.stripe.team_price_id") {
		t.Fatalf("Validate missing team price: got %v", err)
	}

	cfg.Billing.Stripe.TeamPriceID = "price_123"
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate complete billing config: %v", err)
	}
}

func TestValidate_BillingRejectsBadURLs(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Billing.Stripe.SuccessURL = "/relative"
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "billing.stripe.success_url") {
		t.Fatalf("Validate bad success URL: got %v", err)
	}

	cfg = Defaults()
	cfg.Billing.Enabled = true
	cfg.Billing.Stripe.SecretKey = "sk_test_123"
	cfg.Billing.Stripe.WebhookSecret = "whsec_123"
	cfg.Billing.Stripe.TeamPriceID = "price_123"
	cfg.Auth.BaseURL = "shithub.local"
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), "auth.base_url") {
		t.Fatalf("Validate bad auth base URL with billing enabled: got %v", err)
	}
}

func TestMergeEnv_AppliesNestedKeys(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	env := []string{
		"SHITHUB_WEB__ADDR=:9090",
		"SHITHUB_DB__MAX_CONNS=42",
		"SHITHUB_TRACING__ENABLED=true",
		"SHITHUB_TRACING__ENDPOINT=http://otel:4318",
		"SHITHUB_DB__CONNECT_TIMEOUT=8s",
		"SHITHUB_BILLING__ENABLED=true",
		"SHITHUB_BILLING__GRACE_PERIOD=240h",
		"SHITHUB_BILLING__STRIPE__SECRET_KEY=sk_test_123",
		"SHITHUB_BILLING__STRIPE__WEBHOOK_SECRET=whsec_123",
		"SHITHUB_BILLING__STRIPE__TEAM_PRICE_ID=price_123",
		"SHITHUB_BILLING__STRIPE__AUTOMATIC_TAX=true",
	}
	if err := mergeEnv(&cfg, env); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if cfg.Web.Addr != ":9090" {
		t.Errorf("Web.Addr: got %q", cfg.Web.Addr)
	}
	if cfg.DB.MaxConns != 42 {
		t.Errorf("DB.MaxConns: got %d", cfg.DB.MaxConns)
	}
	if !cfg.Tracing.Enabled {
		t.Errorf("Tracing.Enabled: not set")
	}
	if cfg.DB.ConnectTimeout != 8*time.Second {
		t.Errorf("DB.ConnectTimeout: got %v", cfg.DB.ConnectTimeout)
	}
	if !cfg.Billing.Enabled {
		t.Errorf("Billing.Enabled: not set")
	}
	if cfg.Billing.GracePeriod != 240*time.Hour {
		t.Errorf("Billing.GracePeriod: got %v", cfg.Billing.GracePeriod)
	}
	if cfg.Billing.Stripe.SecretKey != "sk_test_123" {
		t.Errorf("Billing.Stripe.SecretKey: got %q", cfg.Billing.Stripe.SecretKey)
	}
	if cfg.Billing.Stripe.WebhookSecret != "whsec_123" {
		t.Errorf("Billing.Stripe.WebhookSecret: got %q", cfg.Billing.Stripe.WebhookSecret)
	}
	if cfg.Billing.Stripe.TeamPriceID != "price_123" {
		t.Errorf("Billing.Stripe.TeamPriceID: got %q", cfg.Billing.Stripe.TeamPriceID)
	}
	if !cfg.Billing.Stripe.AutomaticTax {
		t.Errorf("Billing.Stripe.AutomaticTax: not set")
	}
}

func TestPrintRedacted_HidesSecrets(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.DB.URL = "postgres://shithub:hunter2@localhost/shithub"
	cfg.Session.KeyB64 = "supersecretkey"
	cfg.Metrics.BasicAuthPass = "metrics-pass"
	cfg.ErrorReporting.DSN = "https://abc@sentry.example/1"
	cfg.Billing.Stripe.SecretKey = "sk_live_secret"
	cfg.Billing.Stripe.WebhookSecret = "whsec_secret"

	out, err := PrintRedacted(cfg)
	if err != nil {
		t.Fatalf("PrintRedacted: %v", err)
	}

	for _, leak := range []string{"hunter2", "supersecretkey", "metrics-pass", "https://abc@sentry", "sk_live_secret", "whsec_secret"} {
		if strings.Contains(out, leak) {
			t.Errorf("PrintRedacted leaked %q\noutput: %s", leak, out)
		}
	}
	if !strings.Contains(out, "***") {
		t.Errorf("PrintRedacted produced no *** redactions; output:\n%s", out)
	}
}

func TestMergeFlags_OverridesEnv(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	if err := mergeEnv(&cfg, []string{"SHITHUB_WEB__ADDR=:9090"}); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if err := mergeFlags(&cfg, map[string]string{"web.addr": ":7777"}); err != nil {
		t.Fatalf("mergeFlags: %v", err)
	}
	if cfg.Web.Addr != ":7777" {
		t.Errorf("Web.Addr: got %q, want :7777", cfg.Web.Addr)
	}
}

func TestDefaults_RateLimitAPI(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	if cfg.RateLimit.API.AuthedPerHour != 5000 {
		t.Errorf("RateLimit.API.AuthedPerHour: got %d, want 5000", cfg.RateLimit.API.AuthedPerHour)
	}
	if cfg.RateLimit.API.AnonPerHour != 60 {
		t.Errorf("RateLimit.API.AnonPerHour: got %d, want 60", cfg.RateLimit.API.AnonPerHour)
	}
}

func TestValidate_RejectsNegativeRateLimit(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.RateLimit.API.AuthedPerHour = -1
	if err := Validate(&cfg); err == nil {
		t.Errorf("expected validation error for ratelimit.api.authed_per_hour=-1")
	}
	cfg = Defaults()
	cfg.RateLimit.API.AnonPerHour = -5
	if err := Validate(&cfg); err == nil {
		t.Errorf("expected validation error for ratelimit.api.anon_per_hour=-5")
	}
}

func TestValidate_RateLimitZeroFillsDefault(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.RateLimit.API.AuthedPerHour = 0
	cfg.RateLimit.API.AnonPerHour = 0
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.RateLimit.API.AuthedPerHour != 5000 {
		t.Errorf("zero-fill authed: got %d, want 5000", cfg.RateLimit.API.AuthedPerHour)
	}
	if cfg.RateLimit.API.AnonPerHour != 60 {
		t.Errorf("zero-fill anon: got %d, want 60", cfg.RateLimit.API.AnonPerHour)
	}
}

func TestMergeEnv_RateLimitAPI(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	env := []string{
		"SHITHUB_RATELIMIT__API__AUTHED_PER_HOUR=10000",
		"SHITHUB_RATELIMIT__API__ANON_PER_HOUR=120",
	}
	if err := mergeEnv(&cfg, env); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if cfg.RateLimit.API.AuthedPerHour != 10000 {
		t.Errorf("AuthedPerHour: got %d, want 10000", cfg.RateLimit.API.AuthedPerHour)
	}
	if cfg.RateLimit.API.AnonPerHour != 120 {
		t.Errorf("AnonPerHour: got %d, want 120", cfg.RateLimit.API.AnonPerHour)
	}
}

func TestDefaults_RateLimitHTML(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	if got, want := cfg.RateLimit.HTML.AnonBurst, 60; got != want {
		t.Errorf("RateLimit.HTML.AnonBurst: got %d, want %d", got, want)
	}
	if got, want := cfg.RateLimit.HTML.AnonRefill, 1; got != want {
		t.Errorf("RateLimit.HTML.AnonRefill: got %d, want %d", got, want)
	}
	if got, want := cfg.RateLimit.HTML.AuthedBurst, 600; got != want {
		t.Errorf("RateLimit.HTML.AuthedBurst: got %d, want %d", got, want)
	}
	if got, want := cfg.RateLimit.HTML.AuthedRefill, 10; got != want {
		t.Errorf("RateLimit.HTML.AuthedRefill: got %d, want %d", got, want)
	}
}

func TestValidate_RejectsNegativeRateLimitHTML(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"anon_burst":            func(c *Config) { c.RateLimit.HTML.AnonBurst = -1 },
		"anon_refill_per_sec":   func(c *Config) { c.RateLimit.HTML.AnonRefill = -1 },
		"authed_burst":          func(c *Config) { c.RateLimit.HTML.AuthedBurst = -1 },
		"authed_refill_per_sec": func(c *Config) { c.RateLimit.HTML.AuthedRefill = -1 },
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Defaults()
			mutate(&cfg)
			if err := Validate(&cfg); err == nil {
				t.Errorf("expected validation error for ratelimit.html.%s=-1", name)
			}
		})
	}
}

func TestValidate_RateLimitHTMLZeroFillsDefault(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.RateLimit.HTML.AnonBurst = 0
	cfg.RateLimit.HTML.AnonRefill = 0
	cfg.RateLimit.HTML.AuthedBurst = 0
	cfg.RateLimit.HTML.AuthedRefill = 0
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.RateLimit.HTML.AnonBurst != 60 {
		t.Errorf("zero-fill anon_burst: got %d, want 60", cfg.RateLimit.HTML.AnonBurst)
	}
	if cfg.RateLimit.HTML.AnonRefill != 1 {
		t.Errorf("zero-fill anon_refill: got %d, want 1", cfg.RateLimit.HTML.AnonRefill)
	}
	if cfg.RateLimit.HTML.AuthedBurst != 600 {
		t.Errorf("zero-fill authed_burst: got %d, want 600", cfg.RateLimit.HTML.AuthedBurst)
	}
	if cfg.RateLimit.HTML.AuthedRefill != 10 {
		t.Errorf("zero-fill authed_refill: got %d, want 10", cfg.RateLimit.HTML.AuthedRefill)
	}
}

func TestMergeEnv_RateLimitHTML(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	env := []string{
		"SHITHUB_RATELIMIT__HTML__ANON_BURST=120",
		"SHITHUB_RATELIMIT__HTML__ANON_REFILL_PER_SEC=2",
		"SHITHUB_RATELIMIT__HTML__AUTHED_BURST=1200",
		"SHITHUB_RATELIMIT__HTML__AUTHED_REFILL_PER_SEC=20",
	}
	if err := mergeEnv(&cfg, env); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if cfg.RateLimit.HTML.AnonBurst != 120 {
		t.Errorf("AnonBurst: got %d, want 120", cfg.RateLimit.HTML.AnonBurst)
	}
	if cfg.RateLimit.HTML.AnonRefill != 2 {
		t.Errorf("AnonRefill: got %d, want 2", cfg.RateLimit.HTML.AnonRefill)
	}
	if cfg.RateLimit.HTML.AuthedBurst != 1200 {
		t.Errorf("AuthedBurst: got %d, want 1200", cfg.RateLimit.HTML.AuthedBurst)
	}
	if cfg.RateLimit.HTML.AuthedRefill != 20 {
		t.Errorf("AuthedRefill: got %d, want 20", cfg.RateLimit.HTML.AuthedRefill)
	}
}
