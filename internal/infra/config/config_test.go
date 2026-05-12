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

func TestMergeEnv_AppliesNestedKeys(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	env := []string{
		"SHITHUB_WEB__ADDR=:9090",
		"SHITHUB_DB__MAX_CONNS=42",
		"SHITHUB_TRACING__ENABLED=true",
		"SHITHUB_TRACING__ENDPOINT=http://otel:4318",
		"SHITHUB_DB__CONNECT_TIMEOUT=8s",
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
}

func TestPrintRedacted_HidesSecrets(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.DB.URL = "postgres://shithub:hunter2@localhost/shithub"
	cfg.Session.KeyB64 = "supersecretkey"
	cfg.Metrics.BasicAuthPass = "metrics-pass"
	cfg.ErrorReporting.DSN = "https://abc@sentry.example/1"

	out, err := PrintRedacted(cfg)
	if err != nil {
		t.Fatalf("PrintRedacted: %v", err)
	}

	for _, leak := range []string{"hunter2", "supersecretkey", "metrics-pass", "https://abc@sentry"} {
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
