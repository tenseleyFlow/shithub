// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dependencyupdates parses the supported subset of GitHub-compatible
// dependency update configuration. It intentionally accepts only ecosystems
// that shithub can inventory locally today so paid-plan copy cannot overclaim
// update automation coverage.
package dependencyupdates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultConfigPath         = ".github/dependabot.yml"
	DefaultOpenPullRequestMax = 5
	MaxConfigFileBytes        = 512 * 1024
	MaxYAMLAliases            = 100
)

var (
	ErrTooLarge       = errors.New("dependency update config exceeds size limit")
	ErrTooManyAliases = errors.New("dependency update config has too many aliases")
)

type Severity string

const (
	Warning Severity = "warning"
	Error   Severity = "error"
)

type Diagnostic struct {
	Path     string
	Message  string
	Severity Severity
}

type File struct {
	Version       int
	Configs       []Config
	RawConfigHash string
}

type Config struct {
	PackageEcosystem    string
	Ecosystem           string
	PackageManager      string
	Directory           string
	Schedule            Schedule
	OpenPullRequestMax  int
	TargetBranch        string
	AllowRules          []AllowRule
	IgnoreRules         []IgnoreRule
	Groups              map[string]GroupRule
	Registries          []string
	UnsupportedKeys     []string
	RawConfigPath       string
	LastSyncedSHA       string
	Enabled             bool
	AllowRulesJSON      []byte
	IgnoreRulesJSON     []byte
	GroupsJSON          []byte
	RegistriesJSON      []byte
	UnsupportedKeysJSON []byte
}

type Schedule struct {
	Interval string
	Day      string
	Time     string
	Timezone string
	Cronjob  string
}

type AllowRule struct {
	DependencyName string   `json:"dependency_name,omitempty"`
	DependencyType string   `json:"dependency_type,omitempty"`
	UpdateTypes    []string `json:"update_types,omitempty"`
}

type IgnoreRule struct {
	DependencyName string   `json:"dependency_name,omitempty"`
	Versions       []string `json:"versions,omitempty"`
	UpdateTypes    []string `json:"update_types,omitempty"`
}

type GroupRule struct {
	Patterns        []string `json:"patterns,omitempty"`
	ExcludePatterns []string `json:"exclude_patterns,omitempty"`
	DependencyType  string   `json:"dependency_type,omitempty"`
	UpdateTypes     []string `json:"update_types,omitempty"`
	AppliesTo       string   `json:"applies_to,omitempty"`
}

// Parse decodes a .github/dependabot.yml-compatible file. YAML syntax,
// oversized input, and alias-bomb guard errors are returned as Go errors.
// Unsupported but non-dangerous keys are warnings. Validation problems that
// make a config unusable are diagnostic errors and yield a nil File with a nil
// Go error so callers can render the full diagnostic list.
func Parse(src []byte) (*File, []Diagnostic, error) {
	if len(src) > MaxConfigFileBytes {
		return nil, []Diagnostic{{
			Path:     "$",
			Message:  fmt.Sprintf("config is %d bytes; limit is %d", len(src), MaxConfigFileBytes),
			Severity: Error,
		}}, ErrTooLarge
	}

	var root yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	if err := dec.Decode(&root); err != nil {
		return nil, []Diagnostic{{Path: "$", Message: "YAML decode: " + err.Error(), Severity: Error}}, err
	}
	if root.Kind == 0 {
		return nil, []Diagnostic{{Path: "$", Message: "config file is empty", Severity: Error}}, errors.New("empty dependency update config")
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, []Diagnostic{{Path: "$", Message: "config must be a YAML mapping", Severity: Error}}, errors.New("non-mapping dependency update config")
	}
	if aliases := countAliases(root.Content[0], 0); aliases > MaxYAMLAliases {
		return nil, []Diagnostic{{
			Path:     "$",
			Message:  fmt.Sprintf("config has %d alias references; limit is %d", aliases, MaxYAMLAliases),
			Severity: Error,
		}}, ErrTooManyAliases
	}

	mapping := root.Content[0]
	var diags []Diagnostic
	file := &File{
		RawConfigHash: Digest(src),
	}
	seenVersion := false
	seenUpdates := false
	forEachMapping(mapping, func(k string, v *yaml.Node) {
		switch k {
		case "version":
			seenVersion = true
			version, ok := scalarInt(v)
			if !ok {
				diags = append(diags, errAt("version", "must be the scalar integer 2"))
				return
			}
			file.Version = version
			if version != 2 {
				diags = append(diags, errAt("version", "must be 2 for Dependabot-compatible configuration"))
			}
		case "updates":
			seenUpdates = true
			configs := parseUpdates(v, &diags)
			file.Configs = append(file.Configs, configs...)
		case "registries":
			if v.Kind != yaml.MappingNode {
				diags = append(diags, errAt("registries", "must be a mapping when present"))
			}
		default:
			diags = append(diags, warnAt(k, "unsupported top-level key ignored"))
		}
	})
	if !seenVersion {
		diags = append(diags, errAt("version", "is required"))
	}
	if !seenUpdates {
		diags = append(diags, errAt("updates", "is required"))
	}
	if !hasError(diags) && len(file.Configs) == 0 {
		diags = append(diags, errAt("updates", "must include at least one supported update entry"))
	}
	if hasError(diags) {
		return nil, diags, nil
	}
	return file, diags, nil
}

func Digest(src []byte) string {
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:])
}

func parseUpdates(n *yaml.Node, diags *[]Diagnostic) []Config {
	if n.Kind != yaml.SequenceNode {
		*diags = append(*diags, errAt("updates", "must be a sequence"))
		return nil
	}
	configs := make([]Config, 0, len(n.Content))
	seen := map[string]struct{}{}
	for idx, item := range n.Content {
		basePath := fmt.Sprintf("updates[%d]", idx)
		cfg, ok := parseUpdate(item, basePath, diags)
		if !ok {
			continue
		}
		key := cfg.Ecosystem + "\x00" + cfg.Directory
		if _, exists := seen[key]; exists {
			*diags = append(*diags, errAt(basePath, "duplicates another supported entry for the same ecosystem and directory"))
			continue
		}
		seen[key] = struct{}{}
		cfg.RawConfigPath = DefaultConfigPath
		cfg.Enabled = true
		cfg.AllowRulesJSON = mustJSON(cfg.AllowRules, []byte("[]"))
		cfg.IgnoreRulesJSON = mustJSON(cfg.IgnoreRules, []byte("[]"))
		cfg.GroupsJSON = mustJSON(cfg.Groups, []byte("{}"))
		cfg.RegistriesJSON = mustJSON(cfg.Registries, []byte("[]"))
		cfg.UnsupportedKeysJSON = mustJSON(cfg.UnsupportedKeys, []byte("[]"))
		configs = append(configs, cfg)
	}
	return configs
}

func parseUpdate(n *yaml.Node, basePath string, diags *[]Diagnostic) (Config, bool) {
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt(basePath, "must be a mapping"))
		return Config{}, false
	}

	cfg := Config{
		Directory:          "/",
		OpenPullRequestMax: DefaultOpenPullRequestMax,
		Groups:             map[string]GroupRule{},
	}
	seen := map[string]bool{}
	forEachMapping(n, func(k string, v *yaml.Node) {
		seen[k] = true
		switch k {
		case "package-ecosystem":
			cfg.PackageEcosystem = scalarString(v)
			ecosystem, manager, ok := normalizeEcosystem(cfg.PackageEcosystem)
			if !ok {
				*diags = append(*diags, errAt(basePath+".package-ecosystem", "unsupported ecosystem "+strconv.Quote(cfg.PackageEcosystem)+" (supported: gomod, npm)"))
				return
			}
			cfg.Ecosystem = ecosystem
			cfg.PackageManager = manager
		case "directory":
			dir, ok := normalizeDirectory(scalarString(v))
			if !ok {
				*diags = append(*diags, errAt(basePath+".directory", "must be an absolute repository directory without '..'"))
				return
			}
			cfg.Directory = dir
		case "schedule":
			cfg.Schedule = parseSchedule(v, basePath+".schedule", diags)
		case "open-pull-requests-limit":
			limit, ok := scalarInt(v)
			if !ok || limit < 0 || limit > 100 {
				*diags = append(*diags, errAt(basePath+".open-pull-requests-limit", "must be an integer between 0 and 100"))
				return
			}
			cfg.OpenPullRequestMax = limit
		case "target-branch":
			cfg.TargetBranch = scalarString(v)
		case "allow":
			cfg.AllowRules = parseAllowRules(v, basePath+".allow", diags)
		case "ignore":
			cfg.IgnoreRules = parseIgnoreRules(v, basePath+".ignore", diags)
		case "groups":
			cfg.Groups = parseGroups(v, basePath+".groups", diags, &cfg.UnsupportedKeys)
		case "registries":
			cfg.Registries = scalarStringList(v, basePath+".registries", diags)
		default:
			path := basePath + "." + k
			cfg.UnsupportedKeys = append(cfg.UnsupportedKeys, path)
			*diags = append(*diags, warnAt(path, "unsupported update key ignored"))
		}
	})
	if !seen["package-ecosystem"] {
		*diags = append(*diags, errAt(basePath+".package-ecosystem", "is required"))
	}
	if !seen["schedule"] {
		*diags = append(*diags, errAt(basePath+".schedule", "is required"))
	}
	if cfg.Schedule.Interval == "" && seen["schedule"] {
		*diags = append(*diags, errAt(basePath+".schedule.interval", "is required"))
	}
	return cfg, cfg.Ecosystem != "" && cfg.Schedule.Interval != "" && !hasErrorAt(*diags, basePath)
}

func parseSchedule(n *yaml.Node, basePath string, diags *[]Diagnostic) Schedule {
	var s Schedule
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt(basePath, "must be a mapping"))
		return s
	}
	forEachMapping(n, func(k string, v *yaml.Node) {
		switch k {
		case "interval":
			interval := strings.ToLower(scalarString(v))
			if !isSupportedInterval(interval) {
				*diags = append(*diags, errAt(basePath+".interval", "unsupported interval "+strconv.Quote(interval)+" (supported: daily, weekly, monthly, quarterly, semiannually, yearly, cron)"))
				return
			}
			s.Interval = interval
		case "day":
			s.Day = strings.ToLower(scalarString(v))
		case "time":
			s.Time = scalarString(v)
		case "timezone":
			s.Timezone = scalarString(v)
		case "cronjob":
			s.Cronjob = scalarString(v)
		default:
			*diags = append(*diags, warnAt(basePath+"."+k, "unsupported schedule key ignored"))
		}
	})
	if s.Interval == "cron" && strings.TrimSpace(s.Cronjob) == "" {
		*diags = append(*diags, errAt(basePath+".cronjob", "is required when interval is cron"))
	}
	if s.Interval == "cron" && strings.TrimSpace(s.Cronjob) != "" {
		if _, err := nextCronRun(s.Cronjob, timeNowUTCForValidation()); err != nil {
			*diags = append(*diags, errAt(basePath+".cronjob", strings.TrimPrefix(err.Error(), ErrInvalidSchedule.Error()+": ")))
		}
	}
	if s.Interval == "weekly" && strings.TrimSpace(s.Day) != "" {
		if _, err := parseScheduleWeekday(s.Day); err != nil {
			*diags = append(*diags, errAt(basePath+".day", strings.TrimPrefix(err.Error(), ErrInvalidSchedule.Error()+": ")))
		}
	}
	if strings.TrimSpace(s.Time) != "" {
		if _, _, err := parseScheduleClock(s.Time, ""); err != nil {
			*diags = append(*diags, errAt(basePath+".time", strings.TrimPrefix(err.Error(), ErrInvalidSchedule.Error()+": ")))
		}
	}
	if strings.TrimSpace(s.Timezone) != "" {
		if _, err := scheduleLocation(s.Timezone); err != nil {
			*diags = append(*diags, errAt(basePath+".timezone", strings.TrimPrefix(err.Error(), ErrInvalidSchedule.Error()+": ")))
		}
	}
	return s
}

func timeNowUTCForValidation() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func parseAllowRules(n *yaml.Node, basePath string, diags *[]Diagnostic) []AllowRule {
	if n.Kind != yaml.SequenceNode {
		*diags = append(*diags, errAt(basePath, "must be a sequence"))
		return nil
	}
	rules := make([]AllowRule, 0, len(n.Content))
	for idx, item := range n.Content {
		path := fmt.Sprintf("%s[%d]", basePath, idx)
		if item.Kind != yaml.MappingNode {
			*diags = append(*diags, errAt(path, "must be a mapping"))
			continue
		}
		var rule AllowRule
		forEachMapping(item, func(k string, v *yaml.Node) {
			switch k {
			case "dependency-name":
				rule.DependencyName = scalarString(v)
			case "dependency-type":
				rule.DependencyType = scalarString(v)
			case "update-types":
				rule.UpdateTypes = scalarStringList(v, path+".update-types", diags)
			default:
				*diags = append(*diags, warnAt(path+"."+k, "unsupported allow key ignored"))
			}
		})
		rules = append(rules, rule)
	}
	return rules
}

func parseIgnoreRules(n *yaml.Node, basePath string, diags *[]Diagnostic) []IgnoreRule {
	if n.Kind != yaml.SequenceNode {
		*diags = append(*diags, errAt(basePath, "must be a sequence"))
		return nil
	}
	rules := make([]IgnoreRule, 0, len(n.Content))
	for idx, item := range n.Content {
		path := fmt.Sprintf("%s[%d]", basePath, idx)
		if item.Kind != yaml.MappingNode {
			*diags = append(*diags, errAt(path, "must be a mapping"))
			continue
		}
		var rule IgnoreRule
		forEachMapping(item, func(k string, v *yaml.Node) {
			switch k {
			case "dependency-name":
				rule.DependencyName = scalarString(v)
			case "versions":
				rule.Versions = scalarStringList(v, path+".versions", diags)
			case "update-types":
				rule.UpdateTypes = scalarStringList(v, path+".update-types", diags)
			default:
				*diags = append(*diags, warnAt(path+"."+k, "unsupported ignore key ignored"))
			}
		})
		rules = append(rules, rule)
	}
	return rules
}

func parseGroups(n *yaml.Node, basePath string, diags *[]Diagnostic, unsupported *[]string) map[string]GroupRule {
	groups := map[string]GroupRule{}
	if n.Kind != yaml.MappingNode {
		*diags = append(*diags, errAt(basePath, "must be a mapping"))
		return groups
	}
	for i := 0; i < len(n.Content); i += 2 {
		name := n.Content[i].Value
		value := n.Content[i+1]
		groupPath := basePath + "." + name
		if value.Kind != yaml.MappingNode {
			*diags = append(*diags, errAt(groupPath, "must be a mapping"))
			continue
		}
		var group GroupRule
		forEachMapping(value, func(k string, v *yaml.Node) {
			switch k {
			case "patterns":
				group.Patterns = scalarStringList(v, groupPath+".patterns", diags)
			case "exclude-patterns":
				group.ExcludePatterns = scalarStringList(v, groupPath+".exclude-patterns", diags)
			case "dependency-type":
				group.DependencyType = scalarString(v)
			case "update-types":
				group.UpdateTypes = scalarStringList(v, groupPath+".update-types", diags)
			case "applies-to":
				group.AppliesTo = scalarString(v)
			default:
				p := groupPath + "." + k
				*unsupported = append(*unsupported, p)
				*diags = append(*diags, warnAt(p, "unsupported group key ignored"))
			}
		})
		groups[name] = group
	}
	return groups
}

func normalizeEcosystem(s string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "gomod":
		return "go", "gomod", true
	case "npm":
		return "npm", "npm", true
	default:
		return "", "", false
	}
}

func normalizeDirectory(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !strings.HasPrefix(s, "/") {
		return "", false
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return "", false
		}
	}
	clean := path.Clean(s)
	if clean == "." {
		clean = "/"
	}
	if !strings.HasPrefix(clean, "/") || strings.Contains(clean, "/../") || clean == "/.." {
		return "", false
	}
	return clean, true
}

func isSupportedInterval(s string) bool {
	switch s {
	case "daily", "weekly", "monthly", "quarterly", "semiannually", "yearly", "cron":
		return true
	default:
		return false
	}
}

func scalarString(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(n.Value)
}

func scalarInt(n *yaml.Node) (int, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Value))
	return v, err == nil
}

func scalarStringList(n *yaml.Node, p string, diags *[]Diagnostic) []string {
	if n.Kind == yaml.ScalarNode {
		s := scalarString(n)
		if s == "" {
			return nil
		}
		return []string{s}
	}
	if n.Kind != yaml.SequenceNode {
		*diags = append(*diags, errAt(p, "must be a scalar string or sequence of strings"))
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for idx, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			*diags = append(*diags, errAt(fmt.Sprintf("%s[%d]", p, idx), "must be a scalar string"))
			continue
		}
		if s := scalarString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func forEachMapping(n *yaml.Node, fn func(k string, v *yaml.Node)) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(n.Content); i += 2 {
		fn(n.Content[i].Value, n.Content[i+1])
	}
}

func countAliases(n *yaml.Node, depth int) int {
	if n == nil || depth > 100 {
		return 0
	}
	count := 0
	if n.Kind == yaml.AliasNode {
		count++
	}
	for _, c := range n.Content {
		count += countAliases(c, depth+1)
		if count > MaxYAMLAliases {
			return count
		}
	}
	return count
}

func warnAt(p, msg string) Diagnostic {
	return Diagnostic{Path: p, Message: msg, Severity: Warning}
}

func errAt(p, msg string) Diagnostic {
	return Diagnostic{Path: p, Message: msg, Severity: Error}
}

func hasError(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

func hasErrorAt(diags []Diagnostic, p string) bool {
	for _, d := range diags {
		if d.Severity == Error && (d.Path == p || strings.HasPrefix(d.Path, p+".")) {
			return true
		}
	}
	return false
}

func mustJSON(v any, fallback []byte) []byte {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return fallback
	}
	return b
}
