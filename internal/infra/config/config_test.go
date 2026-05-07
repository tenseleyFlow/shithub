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
