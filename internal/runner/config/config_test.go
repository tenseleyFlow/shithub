// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoad_DefaultsWithToken(t *testing.T) {
	t.Parallel()
	cfg, err := Load(LoadOptions{
		Environ: []string{"SHITHUB_RUNNER_TOKEN=tok"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("BaseURL: %q", cfg.Server.BaseURL)
	}
	if cfg.Engine.Kind != "docker" {
		t.Fatalf("Engine.Kind: %q", cfg.Engine.Kind)
	}
	if cfg.Runner.PollInterval != 5*time.Second {
		t.Fatalf("PollInterval: %v", cfg.Runner.PollInterval)
	}
}

func TestLoad_FileEnvAliasAndFlagsPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[server]
base_url = "https://file.example/"

[runner]
token = "file-token"
labels = ["self-hosted", "linux"]
capacity = 2
poll_interval = "10s"
workspace_root = "/tmp/file"
workspace_ttl = "12h"

[engine]
kind = "docker"
default_image = "file-image"
network = "none"
memory = "1g"
cpus = "1"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(LoadOptions{
		ConfigPath: path,
		Environ: []string{
			"SHITHUB_RUNNER_TOKEN=alias-token",
			"SHITHUB_RUNNER_RUNNER__CAPACITY=3",
			"SHITHUB_RUNNER_RUNNER__LABELS=self-hosted,linux,x64",
		},
		Overrides: map[string]string{
			"server.base_url":      "https://flag.example/path/",
			"runner.capacity":      "4",
			"runner.poll_interval": "2s",
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BaseURL != "https://flag.example/path" {
		t.Fatalf("BaseURL: %q", cfg.Server.BaseURL)
	}
	if cfg.Runner.Token != "alias-token" {
		t.Fatalf("Token: %q", cfg.Runner.Token)
	}
	if cfg.Runner.Capacity != 4 {
		t.Fatalf("Capacity: %d", cfg.Runner.Capacity)
	}
	if cfg.Runner.PollInterval != 2*time.Second {
		t.Fatalf("PollInterval: %v", cfg.Runner.PollInterval)
	}
	if want := []string{"self-hosted", "linux", "x64"}; !reflect.DeepEqual(cfg.Runner.Labels, want) {
		t.Fatalf("Labels: got %#v want %#v", cfg.Runner.Labels, want)
	}
}

func TestLoad_RequiresToken(t *testing.T) {
	t.Parallel()
	_, err := Load(LoadOptions{Environ: []string{}})
	if err == nil || !strings.Contains(err.Error(), "runner.token") {
		t.Fatalf("Load error: %v", err)
	}
}

func TestValidate_RejectsBadCapacity(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Runner.Token = "tok"
	cfg.Runner.Capacity = 0
	if err := Validate(&cfg); err == nil {
		t.Fatal("Validate returned nil error")
	}
}

func TestValidate_RejectsBadEngineKind(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Runner.Token = "tok"
	cfg.Engine.Kind = "runc"
	if err := Validate(&cfg); err == nil {
		t.Fatal("Validate returned nil error")
	}
}
