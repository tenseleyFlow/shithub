// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path/filepath"
	"net"
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

// TestDefaults_EnforceFlagsAllOn pins the PRO-EXT01-17 wrap decision:
// every Pro gate enforces by default. A new EnforceConfig field that
// forgets to set itself in defaultEnforce() defaults to false and this
// test will fail — exactly the trip-wire we want when extending the
// matrix without thinking.
func TestDefaults_EnforceFlagsAllOn(t *testing.T) {
	t.Parallel()
	e := Defaults().Billing.Enforce
	cases := map[string]bool{
		"UserAdvancedBranchProtection": e.UserAdvancedBranchProtection,
		"UserRequiredReviewers":        e.UserRequiredReviewers,
		"UserProfilePinsBeyondFree":    e.UserProfilePinsBeyondFree,
		"UserProfileVanity":            e.UserProfileVanity,
		"UserUsernameReservations":     e.UserUsernameReservations,
		"UserPrivateRepoTemplates":     e.UserPrivateRepoTemplates,
		"UserSavedRepliesUnlimited":    e.UserSavedRepliesUnlimited,
		"UserScheduledIssues":          e.UserScheduledIssues,
		"UserAdvancedCodeSearch":       e.UserAdvancedCodeSearch,
		"UserContributionPrivacy":      e.UserContributionPrivacy,
		"UserSecretScanHistory":        e.UserSecretScanHistory,
		"UserSecretScanAlerts":         e.UserSecretScanAlerts,
		"UserFineGrainedPATs":          e.UserFineGrainedPATs,
		"UserActionsSecrets":           e.UserActionsSecrets,
		"UserActionsVariables":         e.UserActionsVariables,
		"AnimatedAvatars":              e.AnimatedAvatars,
		"UserWebhookRelay":             e.UserWebhookRelay,
		"UserCronWorkflowDispatch":     e.UserCronWorkflowDispatch,
		"UserPersonalStatusPage":       e.UserPersonalStatusPage,
		"UserRepoTimeMachine":          e.UserRepoTimeMachine,
		"UserInboxRules":               e.UserInboxRules,
		"UserInboxDigests":             e.UserInboxDigests,
	}
	for name, on := range cases {
		if !on {
			t.Errorf("EnforceConfig.%s defaulted to false; defaultEnforce() must set every flag true", name)
		}
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

func TestValidateLoopbackAddr(t *testing.T) {
	ok := []string{"", "127.0.0.1:6060", "127.0.0.1:0", "127.9.9.9:6060", "[::1]:6060"}
	for _, addr := range ok {
		if err := ValidateLoopbackAddr("web.pprof_addr", addr); err != nil {
			t.Errorf("ValidateLoopbackAddr(%q) = %v, want nil", addr, err)
		}
	}
	bad := []string{
		":6060",              // every interface
		"0.0.0.0:6060",       // every interface, explicit
		"[::]:6060",          // every interface, v6
		"10.50.0.2:6060",     // mesh
		"24.199.108.81:6060", // public
		"localhost:6060",     // hostname, not resolved on purpose
		"127.0.0.1",          // no port
		"not an address",
	}
	for _, addr := range bad {
		if err := ValidateLoopbackAddr("web.pprof_addr", addr); err == nil {
			t.Errorf("ValidateLoopbackAddr(%q) = nil, want an error", addr)
		}
	}
}

// The knob has to fail the whole config load, not just its own
// helper — an operator who binds pprof to 0.0.0.0 should get a
// refusing-to-start error, not a listening profiler.
func TestValidateRejectsNonLoopbackPprofAddr(t *testing.T) {
	cfg := Defaults()
	cfg.Storage.ReposRoot = "/tmp/repos"
	cfg.Auth.BaseURL = "http://127.0.0.1:8080"
	cfg.Auth.SiteName = "shithub"
	cfg.Auth.EmailFrom = "noreply@example.test"
	cfg.Web.PprofAddr = "0.0.0.0:6060"

	if err := Validate(&cfg); err == nil {
		t.Fatal("Validate accepted web.pprof_addr=0.0.0.0:6060")
	}

	cfg.Web.PprofAddr = "127.0.0.1:6060"
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate rejected a loopback pprof addr: %v", err)
	}
}

func TestDefaultsDisablePprof(t *testing.T) {
	if got := Defaults().Web.PprofAddr; got != "" {
		t.Fatalf("Defaults().Web.PprofAddr = %q, want empty (disabled)", got)
	}
}

func TestPprofAddrFromEnv(t *testing.T) {
	t.Setenv("SHITHUB_WEB__PPROF_ADDR", "127.0.0.1:6060")
	t.Setenv("SHITHUB_STORAGE__REPOS_ROOT", "/tmp/repos")
	t.Setenv("SHITHUB_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.PprofAddr != "127.0.0.1:6060" {
		t.Fatalf("web.pprof_addr = %q, want 127.0.0.1:6060", cfg.Web.PprofAddr)
	}
}

// TestDefaults_TrustedProxies pins the reference-deployment default:
// Caddy proxies from loopback, so loopback is trusted out of the box.
// Without it every anonymous request behind the proxy keys on
// 127.0.0.1 and shares one rate-limit bucket.
func TestDefaults_TrustedProxies(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	want := []string{"127.0.0.0/8", "::1/128"}
	if len(cfg.Web.TrustedProxies) != len(want) {
		t.Fatalf("Web.TrustedProxies: got %v, want %v", cfg.Web.TrustedProxies, want)
	}
	for i, w := range want {
		if cfg.Web.TrustedProxies[i] != w {
			t.Errorf("Web.TrustedProxies[%d]: got %q, want %q", i, cfg.Web.TrustedProxies[i], w)
		}
	}
	nets, err := cfg.Web.TrustedProxyNets()
	if err != nil {
		t.Fatalf("TrustedProxyNets: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("TrustedProxyNets: got %d nets, want 2", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Errorf("TrustedProxyNets[0] does not contain 127.0.0.1")
	}
	if !nets[1].Contains(net.ParseIP("::1")) {
		t.Errorf("TrustedProxyNets[1] does not contain ::1")
	}
}

func TestMergeEnv_TrustedProxiesCommaSeparated(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	env := []string{"SHITHUB_WEB__TRUSTED_PROXIES=10.0.0.0/8, 172.16.0.0/12 ,"}
	if err := mergeEnv(&cfg, env); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	want := []string{"10.0.0.0/8", "172.16.0.0/12"}
	if len(cfg.Web.TrustedProxies) != len(want) {
		t.Fatalf("Web.TrustedProxies: got %v, want %v", cfg.Web.TrustedProxies, want)
	}
	for i, w := range want {
		if cfg.Web.TrustedProxies[i] != w {
			t.Errorf("Web.TrustedProxies[%d]: got %q, want %q", i, cfg.Web.TrustedProxies[i], w)
		}
	}
}

// An explicitly empty value turns the default off — the operator's
// way of saying "there is no proxy in front of me".
func TestMergeEnv_TrustedProxiesEmptyDisables(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	if err := mergeEnv(&cfg, []string{"SHITHUB_WEB__TRUSTED_PROXIES="}); err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if len(cfg.Web.TrustedProxies) != 0 {
		t.Fatalf("Web.TrustedProxies: got %v, want empty", cfg.Web.TrustedProxies)
	}
}

func TestTrustedProxyNets_BareIPAndErrors(t *testing.T) {
	t.Parallel()
	nets, err := WebConfig{TrustedProxies: []string{"10.0.0.7", "2001:db8::1"}}.TrustedProxyNets()
	if err != nil {
		t.Fatalf("TrustedProxyNets: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("got %d nets, want 2", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("10.0.0.7")) || nets[0].Contains(net.ParseIP("10.0.0.8")) {
		t.Errorf("bare IPv4 did not become a /32: %v", nets[0])
	}
	if !nets[1].Contains(net.ParseIP("2001:db8::1")) || nets[1].Contains(net.ParseIP("2001:db8::2")) {
		t.Errorf("bare IPv6 did not become a /128: %v", nets[1])
	}

	if _, err := (WebConfig{TrustedProxies: []string{"not-an-ip"}}).TrustedProxyNets(); err == nil {
		t.Fatal("TrustedProxyNets: want error for malformed entry")
	}
}

func TestValidate_RejectsBadTrustedProxy(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Web.TrustedProxies = []string{"127.0.0.0/8", "300.1.2.3/24"}
	err := Validate(&cfg)
	if err == nil || !strings.Contains(err.Error(), "web.trusted_proxies") {
		t.Fatalf("Validate: got %v, want web.trusted_proxies error", err)
	}
}
