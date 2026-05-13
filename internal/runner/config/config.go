// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config owns the layered configuration loader for shithubd-runner.
//
// Precedence, lowest to highest:
//  1. built-in defaults
//  2. TOML file (/etc/shithubd-runner/config.toml, SHITHUB_RUNNER_CONFIG, or --config)
//  3. environment variables with SHITHUB_RUNNER_ prefix
//  4. CLI flag overrides handed in by the caller
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tenseleyFlow/shithub/internal/actions/runnerlabels"
)

const (
	DefaultPath            = "/etc/shithubd-runner/config.toml"
	EnvPrefix              = "SHITHUB_RUNNER_"
	defaultImage           = "ghcr.io/tenseleyflow/shithub/runner-nix:1.0"
	defaultNetwork         = "shithub-actions"
	defaultDNSServer       = "172.30.0.1"
	defaultSeccompProfile  = "/etc/shithubd-runner/seccomp.json"
	defaultContainerUser   = "65534:65534"
	defaultContainerPIDMax = 512
)

var defaultNetworkAllowlist = []string{
	"api.github.com",
	"auth.docker.io",
	"codeload.github.com",
	"github.com",
	"objects.githubusercontent.com",
	"production.cloudflare.docker.com",
	"registry-1.docker.io",
	"*.githubusercontent.com",
}

// LoadOptions controls config resolution. Zero value uses the default path,
// process environment, and no CLI overrides.
type LoadOptions struct {
	ConfigPath string
	Overrides  map[string]string
	Environ    []string
}

// Config is the typed root consumed by cmd/shithubd-runner.
type Config struct {
	Server ServerConfig `toml:"server"`
	Runner RunnerConfig `toml:"runner"`
	Engine EngineConfig `toml:"engine"`
	Log    LogConfig    `toml:"log"`
}

type ServerConfig struct {
	BaseURL string `toml:"base_url"`
}

type RunnerConfig struct {
	Token            string        `toml:"token"`
	Labels           []string      `toml:"labels"`
	Capacity         int           `toml:"capacity"`
	PollInterval     time.Duration `toml:"poll_interval"`
	WorkspaceRoot    string        `toml:"workspace_root"`
	WorkspaceTTL     time.Duration `toml:"workspace_ttl"`
	NetworkAllowlist []string      `toml:"network_allowlist"`
}

type EngineConfig struct {
	Kind           string   `toml:"kind"`
	DefaultImage   string   `toml:"default_image"`
	Network        string   `toml:"network"`
	Memory         string   `toml:"memory"`
	CPUs           string   `toml:"cpus"`
	SeccompProfile string   `toml:"seccomp_profile"`
	User           string   `toml:"user"`
	PidsLimit      int      `toml:"pids_limit"`
	DNSServers     []string `toml:"dns_servers"`
}

type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

func Defaults() Config {
	return Config{
		Server: ServerConfig{
			BaseURL: "http://127.0.0.1:8080",
		},
		Runner: RunnerConfig{
			Labels:           runnerlabels.DefaultShared(),
			Capacity:         1,
			PollInterval:     5 * time.Second,
			WorkspaceRoot:    "/var/lib/shithubd-runner/workspaces",
			WorkspaceTTL:     24 * time.Hour,
			NetworkAllowlist: append([]string{}, defaultNetworkAllowlist...),
		},
		Engine: EngineConfig{
			Kind:           "docker",
			DefaultImage:   defaultImage,
			Network:        defaultNetwork,
			Memory:         "2g",
			CPUs:           "2",
			SeccompProfile: defaultSeccompProfile,
			User:           defaultContainerUser,
			PidsLimit:      defaultContainerPIDMax,
			DNSServers:     []string{defaultDNSServer},
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

func Load(opts LoadOptions) (Config, error) {
	cfg := Defaults()
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	if err := mergeFile(&cfg, configPath(opts.ConfigPath, environ)); err != nil {
		return cfg, err
	}
	if err := mergeEnv(&cfg, environ); err != nil {
		return cfg, err
	}
	applyAliases(&cfg, environ)
	if err := mergeFlags(&cfg, opts.Overrides); err != nil {
		return cfg, err
	}
	if err := Validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func configPath(flagPath string, environ []string) string {
	if strings.TrimSpace(flagPath) != "" {
		return strings.TrimSpace(flagPath)
	}
	if v := envLookup(environ, EnvPrefix+"CONFIG"); v != "" {
		return v
	}
	return DefaultPath
}

func mergeFile(cfg *Config, path string) error {
	body, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == DefaultPath {
			return nil
		}
		return fmt.Errorf("runner config: read %s: %w", path, err)
	}
	if _, err := toml.Decode(string(body), cfg); err != nil {
		return fmt.Errorf("runner config: parse %s: %w", path, err)
	}
	return nil
}

func mergeEnv(cfg *Config, environ []string) error {
	src := make(map[string]string)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimPrefix(kv[:eq], EnvPrefix)
		src[key] = kv[eq+1:]
	}
	return walkAndApply(reflect.ValueOf(cfg).Elem(), reflect.TypeOf(*cfg), "", src)
}

func applyAliases(cfg *Config, environ []string) {
	if v := envLookup(environ, EnvPrefix+"URL"); v != "" && envLookup(environ, EnvPrefix+"SERVER__BASE_URL") == "" {
		cfg.Server.BaseURL = v
	}
	if v := envLookup(environ, EnvPrefix+"TOKEN"); v != "" && envLookup(environ, EnvPrefix+"RUNNER__TOKEN") == "" {
		cfg.Runner.Token = v
	}
	if v := envLookup(environ, EnvPrefix+"LABELS"); v != "" && envLookup(environ, EnvPrefix+"RUNNER__LABELS") == "" {
		cfg.Runner.Labels = []string{v}
	}
}

func mergeFlags(cfg *Config, overrides map[string]string) error {
	if len(overrides) == 0 {
		return nil
	}
	src := make(map[string]string, len(overrides))
	for k, v := range overrides {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		src[strings.ToUpper(strings.ReplaceAll(k, ".", "__"))] = v
	}
	return walkAndApply(reflect.ValueOf(cfg).Elem(), reflect.TypeOf(*cfg), "", src)
}

func Validate(c *Config) error {
	c.Server.BaseURL = strings.TrimSpace(c.Server.BaseURL)
	if c.Server.BaseURL == "" {
		return errors.New("runner config: server.base_url is required")
	}
	u, err := url.Parse(c.Server.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("runner config: server.base_url must be an absolute http(s) URL, got %q", c.Server.BaseURL)
	}
	c.Server.BaseURL = strings.TrimRight(c.Server.BaseURL, "/")

	if strings.TrimSpace(c.Runner.Token) == "" {
		return errors.New("runner config: runner.token is required")
	}
	labels, err := normalizeLabels(c.Runner.Labels)
	if err != nil {
		return fmt.Errorf("runner config: runner.labels: %w", err)
	}
	c.Runner.Labels = labels
	if c.Runner.Capacity < 1 || c.Runner.Capacity > 64 {
		return fmt.Errorf("runner config: runner.capacity must be between 1 and 64, got %d", c.Runner.Capacity)
	}
	if c.Runner.PollInterval <= 0 {
		return errors.New("runner config: runner.poll_interval must be positive")
	}
	if strings.TrimSpace(c.Runner.WorkspaceRoot) == "" {
		return errors.New("runner config: runner.workspace_root is required")
	}
	if c.Runner.WorkspaceTTL <= 0 {
		return errors.New("runner config: runner.workspace_ttl must be positive")
	}
	allowlist, err := normalizeHostPatterns(c.Runner.NetworkAllowlist)
	if err != nil {
		return fmt.Errorf("runner config: runner.network_allowlist: %w", err)
	}
	c.Runner.NetworkAllowlist = allowlist

	switch strings.ToLower(strings.TrimSpace(c.Engine.Kind)) {
	case "docker", "podman":
		c.Engine.Kind = strings.ToLower(strings.TrimSpace(c.Engine.Kind))
	default:
		return fmt.Errorf("runner config: engine.kind must be docker|podman, got %q", c.Engine.Kind)
	}
	if strings.TrimSpace(c.Engine.DefaultImage) == "" {
		return errors.New("runner config: engine.default_image is required")
	}
	if strings.TrimSpace(c.Engine.Network) == "" {
		return errors.New("runner config: engine.network is required")
	}
	if strings.TrimSpace(c.Engine.Memory) == "" {
		return errors.New("runner config: engine.memory is required")
	}
	if strings.TrimSpace(c.Engine.CPUs) == "" {
		return errors.New("runner config: engine.cpus is required")
	}
	c.Engine.SeccompProfile = strings.TrimSpace(c.Engine.SeccompProfile)
	if c.Engine.SeccompProfile == "" {
		return errors.New("runner config: engine.seccomp_profile is required")
	}
	c.Engine.User = strings.TrimSpace(c.Engine.User)
	if c.Engine.User == "" {
		return errors.New("runner config: engine.user is required")
	}
	if c.Engine.PidsLimit <= 0 {
		return fmt.Errorf("runner config: engine.pids_limit must be positive, got %d", c.Engine.PidsLimit)
	}
	dnsServers, err := normalizeDNSServers(c.Engine.DNSServers)
	if err != nil {
		return fmt.Errorf("runner config: engine.dns_servers: %w", err)
	}
	c.Engine.DNSServers = dnsServers

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(c.Log.Level)
	default:
		return fmt.Errorf("runner config: log.level must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
		c.Log.Format = strings.ToLower(c.Log.Format)
	default:
		return fmt.Errorf("runner config: log.format must be text|json, got %q", c.Log.Format)
	}
	return nil
}

func normalizeHostPatterns(patterns []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.ContainsAny(p, "/:") || strings.Trim(p, "*.abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return nil, fmt.Errorf("invalid host pattern %q", p)
		}
		if strings.Contains(p, "**") || strings.Contains(p, "..") || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") {
			return nil, fmt.Errorf("invalid host pattern %q", p)
		}
		if strings.Contains(p, "*") && !strings.HasPrefix(p, "*.") {
			return nil, fmt.Errorf("invalid wildcard host pattern %q", p)
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("must contain at least one host pattern")
	}
	return out, nil
}

func normalizeDNSServers(servers []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.ContainsAny(s, " \t\r\n") {
			return nil, fmt.Errorf("invalid DNS server %q", s)
		}
		if _, err := netip.ParseAddr(s); err != nil {
			return nil, fmt.Errorf("invalid DNS server %q", s)
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func normalizeLabels(labels []string) ([]string, error) {
	if len(labels) == 1 && strings.Contains(labels[0], ",") {
		return runnerlabels.ParseCSV(labels[0])
	}
	return runnerlabels.Normalize(labels)
}

func envLookup(environ []string, key string) string {
	prefix := key + "="
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

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
			return fmt.Errorf("runner config: %s: %w", strings.ReplaceAll(strings.ToLower(fieldPath), "__", "."), err)
		}
	}
	return nil
}

func setField(v reflect.Value, t reflect.Type, raw string) error {
	switch t.Kind() {
	case reflect.String:
		v.SetString(raw)
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
	case reflect.Slice:
		if t.Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice type %s", t)
		}
		parts, err := runnerlabels.ParseCSV(raw)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(parts))
	default:
		return fmt.Errorf("unsupported field kind %s", t.Kind())
	}
	return nil
}
