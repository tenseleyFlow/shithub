// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config owns the layered configuration loader.
//
// Precedence (lowest → highest):
//
//  1. Built-in defaults (this file)
//  2. TOML file (path from $SHITHUB_CONFIG, falling back to /etc/shithub/config.toml)
//  3. Environment variables (SHITHUB_<area>_<key>; nested keys joined with "__")
//  4. CLI flag overrides handed in by the caller
//
// The Config struct is the single source of truth for what is configurable.
// Every consumer reads from this struct rather than from os.Getenv directly.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the typed root.
type Config struct {
	Env            string               `toml:"env"` // "dev", "staging", "prod"
	Web            WebConfig            `toml:"web"`
	DB             DBConfig             `toml:"db"`
	Log            LogConfig            `toml:"log"`
	Metrics        MetricsConfig        `toml:"metrics"`
	Tracing        TracingConfig        `toml:"tracing"`
	ErrorReporting ErrorReportingConfig `toml:"error_reporting"`
	Session        SessionConfig        `toml:"session"`
	Storage        StorageConfig        `toml:"storage"`
	Auth           AuthConfig           `toml:"auth"`
	Notif          NotifConfig          `toml:"notif"`
	RateLimit      RateLimitConfig      `toml:"ratelimit"`
	Billing        BillingConfig        `toml:"billing"`
	Actions        ActionsConfig        `toml:"actions"`
	Webhook        WebhookConfig        `toml:"webhook"`
}

// WebhookConfig configures the outbound webhook subsystem. Today
// it carries only the dedicated AEAD key used to encrypt webhook
// secrets at rest. AEADKey is required for webhook delivery; when
// empty the worker logs a loud warning and disables the delivery
// handlers entirely. (Pre-F-shakedown, an unset key fell back to
// auth.totp_key_b64; that fallback was retired once prod confirmed
// zero rows remained on the legacy key.)
type WebhookConfig struct {
	AEADKey string `toml:"aead_key"` // base64 32-byte AEAD key
}

// RateLimitConfig configures runtime rate-limit budgets for surfaces that
// don't carry a domain-specific limiter. The /api/v1/ JSON surface uses
// the API.* sub-block; the public-HTML chi group uses HTML.*; future
// surfaces (search, git transports) get their own sub-blocks here as
// they're factored out of their handlers.
type RateLimitConfig struct {
	API  APIRateLimitConfig  `toml:"api"`
	HTML HTMLRateLimitConfig `toml:"html"`
}

// APIRateLimitConfig sets the per-hour budgets for /api/v1/* requests.
// AuthedPerHour applies when the caller presents a valid PAT (keyed by
// token id). AnonPerHour applies to unauthenticated callers (keyed by
// remote IP). Defaults are GitHub-aligned: 5000/h authed, 60/h anon —
// operators tune via SHITHUB_RATELIMIT__API__AUTHED_PER_HOUR /
// SHITHUB_RATELIMIT__API__ANON_PER_HOUR.
type APIRateLimitConfig struct {
	AuthedPerHour int `toml:"authed_per_hour"`
	AnonPerHour   int `toml:"anon_per_hour"`
}

// HTMLRateLimitConfig sets the per-bucket budgets for the public-HTML
// chi group (F02). Numbers are expressed in token-bucket terms —
// Burst (max in a short window) + Refill (steady-state per second).
// The middleware translates to its backing fixed-window limiter.
//
// Defaults:
//   - Anonymous: 60 burst / 1 refill/sec  (~60 req/min steady, 60 burst)
//   - Authenticated: 600 burst / 10 refill/sec (10× anon, plenty for browsing)
//
// Site admins bypass the middleware entirely — the budgets don't apply
// to operators investigating an incident.
//
// Operators tune via:
//
//	SHITHUB_RATELIMIT__HTML__ANON_BURST
//	SHITHUB_RATELIMIT__HTML__ANON_REFILL_PER_SEC
//	SHITHUB_RATELIMIT__HTML__AUTHED_BURST
//	SHITHUB_RATELIMIT__HTML__AUTHED_REFILL_PER_SEC
type HTMLRateLimitConfig struct {
	AnonBurst    int `toml:"anon_burst"`
	AnonRefill   int `toml:"anon_refill_per_sec"`
	AuthedBurst  int `toml:"authed_burst"`
	AuthedRefill int `toml:"authed_refill_per_sec"`
}

// ActionsConfig groups configuration for the Actions subsystem. Today
// it carries only the sealed-box keypair used by the REST secrets
// surface; secrets storage encryption (at-rest) is handled by the
// shared `auth.totp_key_b64` AEAD key.
type ActionsConfig struct {
	Secrets ActionsSecretsConfig `toml:"secrets"`
}

// ActionsSecretsConfig configures the NaCl sealed-box keypair the
// REST `actions/secrets/public-key` endpoint serves. Clients encrypt
// secret values against this public key; the server decrypts on PUT
// before re-encrypting with the storage AEAD.
//
// BoxPrivateKeyB64 is base64 of a 32-byte X25519 private key. When
// empty the server auto-generates a per-process keypair on startup
// and logs a warning — secrets PUT against one process won't be
// decryptable by another, so production deployments MUST set it.
type ActionsSecretsConfig struct {
	BoxPrivateKeyB64 string `toml:"box_private_key_b64"`
}

// NotifConfig configures the S29 notification surface. UnsubscribeKeyB64
// is the base64-encoded HMAC-SHA256 key that signs one-click
// unsubscribe URLs (RFC 8058). When empty (dev default), the
// fan-out worker derives a deterministic key from the session key so
// links are stable across restarts without operator action — this
// derivation is NOT suitable for prod and the wiring layer logs a
// warning when it fires.
type NotifConfig struct {
	UnsubscribeKeyB64 string `toml:"unsubscribe_key_b64"`
}

// BillingConfig controls paid-organization enforcement and the payment
// processor integration. Stripe is the first supported processor and remains
// optional until operators provide live or test-mode credentials.
type BillingConfig struct {
	Enabled     bool                `toml:"enabled"`
	GracePeriod time.Duration       `toml:"grace_period"`
	Stripe      StripeBillingConfig `toml:"stripe"`
	// Enforce gates the per-feature flip from PRO05's report-only mode
	// to hard enforcement for user-kind principals. Each feature carries
	// its own knob so the launch can revert one feature without rolling
	// back the deploy (PRO07 pitfall: "do not enforce a feature you
	// can't unenforce"). Org-kind enforcement has been on since SP05 and
	// is not gated by this struct.
	Enforce EnforceConfig `toml:"enforce"`
}

// EnforceConfig is the PRO07 per-feature enforcement matrix. Defaults
// (all false) keep production in report-only mode; operators flip
// features individually after the 7-day telemetry soak ratifies that
// no Free user is currently exercising the gate. The flag order and
// names are stable — flipping any to true is a one-way deploy that
// operators back out with the same knob, not by reverting code.
type EnforceConfig struct {
	// UserAdvancedBranchProtection: when true, Free users on private
	// personal repos are blocked from setting prevent_force_push,
	// prevent_deletion, or require_signed_commits.
	UserAdvancedBranchProtection bool `toml:"user_advanced_branch_protection"`
	// UserRequiredReviewers: when true, Free users on private personal
	// repos are blocked from setting required_review_count > 0.
	UserRequiredReviewers bool `toml:"user_required_reviewers"`
	// UserProfilePinsBeyondFree: when true, Free users are blocked from
	// pinning more than the Free cap (6) profile repos. PRO07 ships this
	// gate without a report-only phase — the cap is small, the failure
	// mode is benign, and a Free user has no way to have exceeded the
	// cap before this knob existed.
	UserProfilePinsBeyondFree bool `toml:"user_profile_pins_beyond_free"`
	// UserPrivateRepoTemplates: when true, Free users cannot mark a
	// private personal repo as a template, and create-from-template
	// requests targeting a private template whose owner is not currently
	// Pro are rejected. Off by default → report-only; PRO-EXT01-17 flips
	// after the standard 7-day soak.
	UserPrivateRepoTemplates bool `toml:"user_private_repo_templates"`
	// UserSavedRepliesUnlimited: when true, Free users hitting the
	// FreeSavedRepliesCap are blocked from creating an N+1 reply.
	// Off by default → report-only (the would-deny is logged and the
	// insert lands). PRO-EXT01-07a; promoted in PRO-EXT01-17.
	UserSavedRepliesUnlimited bool `toml:"user_saved_replies_unlimited"`
	// UserScheduledIssues: when true, Free users attempting to schedule
	// an issue have the schedule_at field ignored and the issue is
	// created immediately. Off by default → report-only (the schedule
	// honoured + logged). PRO-EXT01-07b; promoted in PRO-EXT01-17.
	UserScheduledIssues bool `toml:"user_scheduled_issues"`
	// UserAdvancedCodeSearch: when true, Free users are blocked from
	// creating saved code-search queries (PRO-EXT01-08a) and from
	// using regex query syntax (PRO-EXT01-08b). Off by default →
	// report-only (the would-deny is logged but the action lands).
	// Promoted in PRO-EXT01-17.
	UserAdvancedCodeSearch bool `toml:"user_advanced_code_search"`
	// UserContributionPrivacy: when true, Free users are blocked from
	// adding per-repo opt-outs to their contribution graph. Off by
	// default → report-only (the opt-out lands but the would-deny is
	// logged). PRO-EXT01-09; promoted in PRO-EXT01-17.
	UserContributionPrivacy bool `toml:"user_contribution_privacy"`
	// UserSecretScanHistory: when true, secret-scan worker jobs queued
	// against a Free user's repo short-circuit without scanning. Off by
	// default → report-only (the would-deny is logged + the scan runs).
	// PRO-EXT01-10b; promoted in PRO-EXT01-17.
	UserSecretScanHistory bool `toml:"user_secret_scan_history"`
	// UserFineGrainedPATs: when true, Free users are blocked from
	// *attaching* PAT restrictions (IP allowlist now; repo binding in
	// 11b). Off by default → report-only (the restriction lands but the
	// would-deny is logged). 14-day soak before PRO-EXT01-17 flip
	// (longer than the campaign default 7) — credential-escalation
	// blast radius if the gate has a bug.
	//
	// NOTE: this flag governs the WRITE path. The middleware ALWAYS
	// enforces an IP allowlist that exists on a token, regardless of
	// enforce mode — once the user has expressed intent, we honor it.
	UserFineGrainedPATs bool `toml:"user_fine_grained_pats"`
	// UserActionsSecrets: when true, Free users are blocked from
	// creating user-scoped Actions secrets / variables AND user-scoped
	// rows are filtered out of runner-side secret resolution for Free
	// users' workflows. Off by default → report-only (write succeeds,
	// runners still see the rows). PRO-EXT01-12; promoted in
	// PRO-EXT01-17.
	//
	// SECURITY-CRITICAL: runner secret resolution is a credential
	// boundary. PRO-EXT01-12b wires the runner-side filter; this flag
	// gates both the write path AND the runner-read path so a Free
	// user can't sneak rows through during the soak window.
	UserActionsSecrets bool `toml:"user_actions_secrets"`
	// UserWebhookRelay: when true, Free users are blocked from
	// creating webhook relays AND inbound POSTs to existing relays
	// owned by Free users return 403. Off by default → report-only
	// (create and inbound succeed; the would-deny is logged).
	// PRO-EXT01-13a; promoted in PRO-EXT01-17.
	//
	// SECURITY-CRITICAL: a relay is a 1-in → N-out amplification
	// surface. Soak with the operator-visible report-only log
	// stream before flipping enforce.
	UserWebhookRelay bool `toml:"user_webhook_relay"`
	// UserCronWorkflowDispatch: when true, Free users are blocked
	// from creating cron-scheduled workflow dispatches AND in-flight
	// dispatches owned by Free users are skipped on each tick. Off
	// by default → report-only (create succeeds; the would-deny is
	// logged on each scheduled fire). PRO-EXT01-13b; promoted in
	// PRO-EXT01-17.
	UserCronWorkflowDispatch bool `toml:"user_cron_workflow_dispatch"`
}

// StripeBillingConfig holds Stripe Billing API settings. Checkout and portal
// URLs are optional; when omitted the web layer derives them from auth.base_url.
// PRO04 introduced ProPriceID for the user-tier Pro plan; it stays empty
// when the operator has only enabled the org-tier Team plan.
type StripeBillingConfig struct {
	SecretKey       string `toml:"secret_key"`
	WebhookSecret   string `toml:"webhook_secret"`
	TeamPriceID     string `toml:"team_price_id"`
	ProPriceID      string `toml:"pro_price_id"`
	SuccessURL      string `toml:"success_url"`
	CancelURL       string `toml:"cancel_url"`
	PortalReturnURL string `toml:"portal_return_url"`
	AutomaticTax    bool   `toml:"automatic_tax"`
}

// WebConfig holds HTTP server settings.
type WebConfig struct {
	Addr            string        `toml:"addr"`
	ReadTimeout     time.Duration `toml:"read_timeout"`
	WriteTimeout    time.Duration `toml:"write_timeout"`
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
}

// DBConfig holds Postgres settings.
type DBConfig struct {
	URL            string        `toml:"url"`
	MaxConns       int           `toml:"max_conns"`
	MinConns       int           `toml:"min_conns"`
	ConnectTimeout time.Duration `toml:"connect_timeout"`
}

// LogConfig holds slog settings.
type LogConfig struct {
	Level  string `toml:"level"`  // debug | info | warn | error
	Format string `toml:"format"` // text (dev) | json (prod)
}

// MetricsConfig configures the /metrics endpoint.
type MetricsConfig struct {
	Enabled       bool   `toml:"enabled"`
	BasicAuthUser string `toml:"basic_auth_user"`
	BasicAuthPass string `toml:"basic_auth_pass"`
}

// TracingConfig configures the OpenTelemetry exporter.
type TracingConfig struct {
	Enabled     bool    `toml:"enabled"`
	Endpoint    string  `toml:"endpoint"` // OTLP HTTP endpoint
	SampleRate  float64 `toml:"sample_rate"`
	ServiceName string  `toml:"service_name"`
}

// ErrorReportingConfig configures the Sentry/GlitchTip-protocol DSN.
type ErrorReportingConfig struct {
	DSN         string `toml:"dsn"`
	Environment string `toml:"environment"`
	Release     string `toml:"release"`
}

// SessionConfig configures the cookie session store.
type SessionConfig struct {
	KeyB64 string        `toml:"key_b64"`
	MaxAge time.Duration `toml:"max_age"`
	Secure bool          `toml:"secure"`
}

// StorageConfig configures repo filesystem storage and S3-compatible
// object storage. The S3 block targets MinIO in dev/test and DigitalOcean
// Spaces in prod (force_path_style true for MinIO, false for Spaces).
type StorageConfig struct {
	ReposRoot string          `toml:"repos_root"` // filesystem root for bare repos
	S3        S3StorageConfig `toml:"s3"`
}

// AuthConfig configures the email/password auth surface.
type AuthConfig struct {
	RequireEmailVerification bool           `toml:"require_email_verification"`
	BaseURL                  string         `toml:"base_url"` // e.g. https://shithub.example
	SiteName                 string         `toml:"site_name"`
	EmailFrom                string         `toml:"email_from"`
	EmailBackend             string         `toml:"email_backend"` // stdout | smtp | postmark | resend
	SMTP                     SMTPConfig     `toml:"smtp"`
	Postmark                 PostmarkConfig `toml:"postmark"`
	Resend                   ResendConfig   `toml:"resend"`
	Argon2                   Argon2Config   `toml:"argon2"`
	TOTPKeyB64               string         `toml:"totp_key_b64"` // base64 32-byte AEAD key for at-rest TOTP secrets
	SSH                      SSHAuthConfig  `toml:"ssh"`
}

// SSHAuthConfig flips whether the repo pages render an SSH clone URL
// alongside the HTTPS one. The actual SSH service (sshd Match-User-git
// + AuthorizedKeysCommand → shithubd ssh-authkeys) is operator-side
// and orthogonal to this flag — the flag just controls the UI surface.
// Operators who haven't wired SSH should leave Enabled=false so users
// don't get a clone URL that doesn't work.
type SSHAuthConfig struct {
	Enabled bool   `toml:"enabled"`
	Host    string `toml:"host"` // e.g. "git@shithub.sh" — the userinfo + hostname shown in the SSH clone URL
}

// SMTPConfig holds plain-SMTP backend settings (e.g. MailHog in dev).
type SMTPConfig struct {
	Addr     string `toml:"addr"` // host:port
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// ResendConfig holds Resend transactional API settings.
type ResendConfig struct {
	APIKey string `toml:"api_key"`
}

// PostmarkConfig holds Postmark transactional API settings.
type PostmarkConfig struct {
	ServerToken string `toml:"server_token"`
}

// Argon2Config tunes the password hasher's cost. Defaults match the
// internal/auth/password Defaults() — operators bump these on faster
// hardware to keep the per-hash cost in the 100–300 ms band.
type Argon2Config struct {
	MemoryKiB uint32 `toml:"memory_kib"`
	Time      uint32 `toml:"time"`
	Threads   uint8  `toml:"threads"`
}

// S3StorageConfig holds the S3-compatible endpoint settings.
type S3StorageConfig struct {
	Endpoint        string `toml:"endpoint"`          // host[:port], no scheme
	Region          string `toml:"region"`            // e.g. "us-east-1", "nyc3"
	AccessKeyID     string `toml:"access_key_id"`     //
	SecretAccessKey string `toml:"secret_access_key"` //
	Bucket          string `toml:"bucket"`            // e.g. "shithub-dev"
	UseSSL          bool   `toml:"use_ssl"`           // true for Spaces, false for local MinIO
	ForcePathStyle  bool   `toml:"force_path_style"`  // true for MinIO, false for Spaces
}

// Defaults returns the zero-config baseline.
func Defaults() Config {
	return Config{
		Env: "dev",
		Web: WebConfig{
			Addr:            ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    30 * time.Second,
			ShutdownTimeout: 10 * time.Second,
		},
		DB: DBConfig{
			MaxConns:       10,
			MinConns:       0,
			ConnectTimeout: 5 * time.Second,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
		Metrics: MetricsConfig{
			Enabled: true,
		},
		Tracing: TracingConfig{
			Enabled:     false,
			SampleRate:  0.05,
			ServiceName: "shithubd",
		},
		Billing: BillingConfig{
			GracePeriod: 14 * 24 * time.Hour,
		},
		Session: SessionConfig{
			MaxAge: 30 * 24 * time.Hour,
			Secure: false,
		},
		Storage: StorageConfig{
			ReposRoot: "/data/repos",
			S3: S3StorageConfig{
				Region:         "us-east-1",
				ForcePathStyle: true,
			},
		},
		RateLimit: RateLimitConfig{
			API: APIRateLimitConfig{
				AuthedPerHour: 5000,
				AnonPerHour:   60,
			},
			HTML: HTMLRateLimitConfig{
				AnonBurst:    60,
				AnonRefill:   1,
				AuthedBurst:  600,
				AuthedRefill: 10,
			},
		},
		Auth: AuthConfig{
			RequireEmailVerification: true,
			BaseURL:                  "http://127.0.0.1:8080",
			SiteName:                 "shithub",
			EmailFrom:                "shithub <noreply@shithub.local>",
			EmailBackend:             "stdout",
			SMTP: SMTPConfig{
				Addr: "127.0.0.1:1025",
			},
			Argon2: Argon2Config{
				MemoryKiB: 64 * 1024,
				Time:      3,
				Threads:   2,
			},
		},
	}
}

// Load resolves configuration in the documented precedence order. CLI
// overrides may be nil. The TOML file is optional — its absence is not an
// error; its existence with bad syntax IS.
func Load(cliOverrides map[string]string) (Config, error) {
	cfg := Defaults()
	if err := mergeFile(&cfg); err != nil {
		return cfg, err
	}
	if err := mergeEnv(&cfg, os.Environ()); err != nil {
		return cfg, err
	}
	if err := mergeFlags(&cfg, cliOverrides); err != nil {
		return cfg, err
	}
	applyAliases(&cfg)
	if err := Validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyAliases honors well-known env-var aliases that don't follow the
// nested-key convention. SHITHUB_DATABASE_URL is the S01-era name for
// db.url and remains supported.
func applyAliases(cfg *Config) {
	if cfg.DB.URL == "" {
		if v := os.Getenv("SHITHUB_DATABASE_URL"); v != "" {
			cfg.DB.URL = v
		}
	}
	if cfg.Session.KeyB64 == "" {
		if v := os.Getenv("SHITHUB_SESSION_KEY"); v != "" {
			cfg.Session.KeyB64 = v
		}
	}
	if cfg.Auth.TOTPKeyB64 == "" {
		if v := os.Getenv("SHITHUB_TOTP_KEY"); v != "" {
			cfg.Auth.TOTPKeyB64 = v
		}
	}
}

// Validate enforces invariants. Errors are precise enough to point at the
// offending key.
func Validate(c *Config) error {
	switch strings.ToLower(c.Env) {
	case "dev", "staging", "prod":
		c.Env = strings.ToLower(c.Env)
	default:
		return fmt.Errorf("config: env: must be dev|staging|prod, got %q", c.Env)
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(c.Log.Level)
	default:
		return fmt.Errorf("config: log.level: must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
		c.Log.Format = strings.ToLower(c.Log.Format)
	default:
		return fmt.Errorf("config: log.format: must be text|json, got %q", c.Log.Format)
	}
	if c.Web.Addr == "" {
		return errors.New("config: web.addr is required")
	}
	if c.Tracing.Enabled && c.Tracing.Endpoint == "" {
		return errors.New("config: tracing.endpoint is required when tracing.enabled=true")
	}
	if c.Tracing.SampleRate < 0 || c.Tracing.SampleRate > 1 {
		return fmt.Errorf("config: tracing.sample_rate: must be in [0, 1], got %v", c.Tracing.SampleRate)
	}
	if c.Storage.ReposRoot == "" {
		return errors.New("config: storage.repos_root is required")
	}
	if err := validateS3(c.Storage.S3); err != nil {
		return err
	}
	switch c.Auth.EmailBackend {
	case "stdout", "smtp", "postmark", "resend":
	default:
		return fmt.Errorf("config: auth.email_backend: must be stdout|smtp|postmark|resend, got %q", c.Auth.EmailBackend)
	}
	if c.Auth.EmailBackend == "smtp" && c.Auth.SMTP.Addr == "" {
		return errors.New("config: auth.smtp.addr is required when email_backend=smtp")
	}
	if c.Auth.EmailBackend == "postmark" && c.Auth.Postmark.ServerToken == "" {
		return errors.New("config: auth.postmark.server_token is required when email_backend=postmark")
	}
	if c.Auth.EmailBackend == "resend" && c.Auth.Resend.APIKey == "" {
		return errors.New("config: auth.resend.api_key is required when email_backend=resend")
	}
	if c.Auth.BaseURL == "" {
		return errors.New("config: auth.base_url is required (used in email links)")
	}
	if c.Auth.SiteName == "" {
		return errors.New("config: auth.site_name is required")
	}
	if c.Auth.EmailFrom == "" {
		return errors.New("config: auth.email_from is required")
	}
	if c.RateLimit.API.AuthedPerHour < 0 {
		return fmt.Errorf("config: ratelimit.api.authed_per_hour: must be >= 0, got %d", c.RateLimit.API.AuthedPerHour)
	}
	if c.RateLimit.API.AnonPerHour < 0 {
		return fmt.Errorf("config: ratelimit.api.anon_per_hour: must be >= 0, got %d", c.RateLimit.API.AnonPerHour)
	}
	if c.RateLimit.API.AuthedPerHour == 0 {
		c.RateLimit.API.AuthedPerHour = 5000
	}
	if c.RateLimit.API.AnonPerHour == 0 {
		c.RateLimit.API.AnonPerHour = 60
	}
	if err := validateHTMLRateLimit(&c.RateLimit.HTML); err != nil {
		return err
	}
	if err := validateBilling(c); err != nil {
		return err
	}
	return nil
}

// validateHTMLRateLimit rejects negative knobs and zero-fills any
// untouched field with the F02 sprint defaults. Refusing service
// (turning the middleware off) on a zero budget would be a worse
// failure than reverting to the documented default — the worst
// case the middleware itself can hit is misconfigured-Burst-zero
// which it already handles by failing open.
func validateHTMLRateLimit(c *HTMLRateLimitConfig) error {
	if c.AnonBurst < 0 {
		return fmt.Errorf("config: ratelimit.html.anon_burst: must be >= 0, got %d", c.AnonBurst)
	}
	if c.AnonRefill < 0 {
		return fmt.Errorf("config: ratelimit.html.anon_refill_per_sec: must be >= 0, got %d", c.AnonRefill)
	}
	if c.AuthedBurst < 0 {
		return fmt.Errorf("config: ratelimit.html.authed_burst: must be >= 0, got %d", c.AuthedBurst)
	}
	if c.AuthedRefill < 0 {
		return fmt.Errorf("config: ratelimit.html.authed_refill_per_sec: must be >= 0, got %d", c.AuthedRefill)
	}
	if c.AnonBurst == 0 {
		c.AnonBurst = 60
	}
	if c.AnonRefill == 0 {
		c.AnonRefill = 1
	}
	if c.AuthedBurst == 0 {
		c.AuthedBurst = 600
	}
	if c.AuthedRefill == 0 {
		c.AuthedRefill = 10
	}
	return nil
}

func validateBilling(c *Config) error {
	if c.Billing.GracePeriod < 0 {
		return fmt.Errorf("config: billing.grace_period: must be >= 0, got %v", c.Billing.GracePeriod)
	}
	for key, raw := range map[string]string{
		"billing.stripe.success_url":       c.Billing.Stripe.SuccessURL,
		"billing.stripe.cancel_url":        c.Billing.Stripe.CancelURL,
		"billing.stripe.portal_return_url": c.Billing.Stripe.PortalReturnURL,
	} {
		if err := validateOptionalHTTPURL(key, raw); err != nil {
			return err
		}
	}
	if !c.Billing.Enabled {
		return nil
	}
	if c.Billing.Stripe.SecretKey == "" {
		return errors.New("config: billing.stripe.secret_key is required when billing.enabled=true")
	}
	if c.Billing.Stripe.WebhookSecret == "" {
		return errors.New("config: billing.stripe.webhook_secret is required when billing.enabled=true")
	}
	if c.Billing.Stripe.TeamPriceID == "" {
		return errors.New("config: billing.stripe.team_price_id is required when billing.enabled=true")
	}
	if err := validateOptionalHTTPURL("auth.base_url", c.Auth.BaseURL); err != nil {
		return err
	}
	return nil
}

func validateOptionalHTTPURL(key, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: %s: parse: %w", key, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: %s: must be an absolute http(s) URL", key)
	}
	return nil
}

// validateS3 enforces all-or-nothing on the S3 block: if any of
// endpoint/bucket/access keys are set, all must be set.
func validateS3(s S3StorageConfig) error {
	any := s.Endpoint != "" || s.Bucket != "" || s.AccessKeyID != "" || s.SecretAccessKey != ""
	if !any {
		return nil
	}
	missing := []string{}
	if s.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if s.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if s.AccessKeyID == "" {
		missing = append(missing, "access_key_id")
	}
	if s.SecretAccessKey == "" {
		missing = append(missing, "secret_access_key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: storage.s3: incomplete configuration, missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// mergeFile reads the TOML file (when present) over cfg.
func mergeFile(cfg *Config) error {
	path := os.Getenv("SHITHUB_CONFIG")
	if path == "" {
		path = "/etc/shithub/config.toml"
	}
	body, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if _, err := toml.Decode(string(body), cfg); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// mergeEnv overrides cfg from environment variables. Naming convention:
// SHITHUB_<area>__<key> (double-underscore separates nested levels).
// Single-segment keys also accept SHITHUB_<key>.
func mergeEnv(cfg *Config, environ []string) error {
	envMap := make(map[string]string, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "SHITHUB_") {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimPrefix(kv[:eq], "SHITHUB_")
		envMap[key] = kv[eq+1:]
	}
	return walkAndApply(reflect.ValueOf(cfg).Elem(), reflect.TypeOf(*cfg), "", envMap)
}

// mergeFlags applies CLI overrides. Keys use TOML notation
// ("web.addr", "tracing.endpoint", etc.).
func mergeFlags(cfg *Config, overrides map[string]string) error {
	if len(overrides) == 0 {
		return nil
	}
	envStyle := make(map[string]string, len(overrides))
	for k, v := range overrides {
		envStyle[strings.ToUpper(strings.ReplaceAll(k, ".", "__"))] = v
	}
	return walkAndApply(reflect.ValueOf(cfg).Elem(), reflect.TypeOf(*cfg), "", envStyle)
}

// walkAndApply walks struct fields recursively, applying values from src
// keyed by uppercased dot-then-double-underscore-joined paths.
func walkAndApply(v reflect.Value, t reflect.Type, prefix string, src map[string]string) error {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		fieldPath := strings.ToUpper(tag)
		if prefix != "" {
			fieldPath = prefix + "__" + fieldPath
		}
		fv := v.Field(i)

		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Duration(0)) {
			if err := walkAndApply(fv, field.Type, fieldPath, src); err != nil {
				return err
			}
			continue
		}

		raw, ok := src[fieldPath]
		if !ok {
			continue
		}
		if err := setField(fv, field.Type, raw); err != nil {
			return fmt.Errorf("config: %s: %w", strings.ReplaceAll(strings.ToLower(fieldPath), "__", "."), err)
		}
	}
	return nil
}

func setField(v reflect.Value, t reflect.Type, raw string) error {
	switch t.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid bool: %w", err)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if t == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("invalid duration: %w", err)
			}
			v.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid int: %w", err)
		}
		v.SetInt(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("invalid float: %w", err)
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("unsupported field kind %s", t.Kind())
	}
	return nil
}
