// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencyupdates

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSupportedConfig(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
      day: monday
      time: "09:30"
      timezone: America/New_York
    open-pull-requests-limit: 8
    target-branch: trunk
    ignore:
      - dependency-name: example.com/legacy
        versions: [ "1.x", "2.x" ]
    groups:
      go-minors:
        patterns: [ "example.com/*" ]
        update-types: [ "minor", "patch" ]
  - package-ecosystem: npm
    directory: /web
    schedule:
      interval: daily
    registries: [ npm-private ]
    allow:
      - dependency-type: production
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if file.Version != 2 {
		t.Fatalf("Version = %d, want 2", file.Version)
	}
	if file.RawConfigHash == "" || len(file.RawConfigHash) != 64 {
		t.Fatalf("RawConfigHash = %q", file.RawConfigHash)
	}
	if len(file.Configs) != 2 {
		t.Fatalf("len(Configs) = %d, want 2", len(file.Configs))
	}

	goCfg := file.Configs[0]
	if goCfg.PackageEcosystem != "gomod" || goCfg.Ecosystem != "go" || goCfg.PackageManager != "gomod" {
		t.Fatalf("go ecosystem fields = %+v", goCfg)
	}
	if goCfg.Directory != "/" || goCfg.Schedule.Interval != "weekly" || goCfg.Schedule.Day != "monday" {
		t.Fatalf("go config directory/schedule = %+v", goCfg)
	}
	if goCfg.OpenPullRequestMax != 8 || goCfg.TargetBranch != "trunk" {
		t.Fatalf("go config limits/target = %+v", goCfg)
	}
	if got := goCfg.Groups["go-minors"].Patterns; len(got) != 1 || got[0] != "example.com/*" {
		t.Fatalf("group patterns = %#v", got)
	}
	if !json.Valid(goCfg.GroupsJSON) || !bytes.Contains(goCfg.GroupsJSON, []byte("go-minors")) {
		t.Fatalf("GroupsJSON = %s", goCfg.GroupsJSON)
	}

	npmCfg := file.Configs[1]
	if npmCfg.Ecosystem != "npm" || npmCfg.Directory != "/web" {
		t.Fatalf("npm config = %+v", npmCfg)
	}
	if len(npmCfg.Registries) != 1 || npmCfg.Registries[0] != "npm-private" {
		t.Fatalf("registries = %#v", npmCfg.Registries)
	}
	if len(npmCfg.AllowRules) != 1 || npmCfg.AllowRules[0].DependencyType != "production" {
		t.Fatalf("allow rules = %#v", npmCfg.AllowRules)
	}
}

func TestParseWarningsForUnsupportedKeys(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
enable-beta-ecosystems: true
updates:
  - package-ecosystem: npm
    directory: /
    vendor: true
    schedule:
      interval: monthly
      extra: ignored
    groups:
      runtime:
        patterns: [ "*" ]
        exclude-paths: [ test ]
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if file == nil || len(file.Configs) != 1 {
		t.Fatalf("file/configs = %+v", file)
	}
	if countSeverity(diags, Warning) != 4 {
		t.Fatalf("warnings = %+v", diags)
	}
	got := strings.Join(file.Configs[0].UnsupportedKeys, "\n")
	if !strings.Contains(got, "updates[0].vendor") || !strings.Contains(got, "updates[0].groups.runtime.exclude-paths") {
		t.Fatalf("UnsupportedKeys = %#v", file.Configs[0].UnsupportedKeys)
	}
}

func TestParseRejectsUnsupportedEcosystem(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: pip
    directory: /
    schedule:
      interval: weekly
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse returned Go error: %v", err)
	}
	if file != nil {
		t.Fatalf("file = %+v, want nil", file)
	}
	if countSeverity(diags, Error) == 0 {
		t.Fatalf("expected error diagnostic, got %+v", diags)
	}
}

func TestParseRejectsBadDirectoryAndCronWithoutCronjob(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: gomod
    directory: /../private
    schedule:
      interval: cron
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse returned Go error: %v", err)
	}
	if file != nil {
		t.Fatalf("file = %+v, want nil", file)
	}
	var sawDirectory bool
	var sawCron bool
	for _, d := range diags {
		if d.Severity != Error {
			continue
		}
		sawDirectory = sawDirectory || strings.Contains(d.Path, "directory")
		sawCron = sawCron || strings.Contains(d.Path, "cronjob")
	}
	if !sawDirectory || !sawCron {
		t.Fatalf("diags = %+v, want directory and cronjob errors", diags)
	}
}

func TestParseCronSchedule(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: cron
      cronjob: "0 6 * * 1"
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if got := file.Configs[0].Schedule.Cronjob; got != "0 6 * * 1" {
		t.Fatalf("Cronjob = %q", got)
	}
}

func TestParseRejectsInvalidScheduleFields(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
      day: someday
      time: "24:99"
      timezone: Mars/Olympus
`)
	file, diags, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse returned Go error: %v", err)
	}
	if file != nil {
		t.Fatalf("file = %+v, want nil", file)
	}
	var sawDay bool
	var sawTime bool
	var sawTimezone bool
	for _, d := range diags {
		if d.Severity != Error {
			continue
		}
		sawDay = sawDay || strings.Contains(d.Path, "day")
		sawTime = sawTime || strings.Contains(d.Path, "time")
		sawTimezone = sawTimezone || strings.Contains(d.Path, "timezone")
	}
	if !sawDay || !sawTime || !sawTimezone {
		t.Fatalf("diags = %+v, want day, time, and timezone errors", diags)
	}
}

func countSeverity(diags []Diagnostic, severity Severity) int {
	var count int
	for _, d := range diags {
		if d.Severity == severity {
			count++
		}
	}
	return count
}

// TestParseRendersEmptyJSONColumnsNotNull pins the jsonb shapes the
// dependency_update_configs check constraints require: a minimal config
// declares no allow/ignore rules, groups or registries, and those must
// serialize as [] / {}, not null.
func TestParseRendersEmptyJSONColumnsNotNull(t *testing.T) {
	t.Parallel()
	src := []byte(`
version: 2
updates:
  - package-ecosystem: npm
    directory: /
    schedule:
      interval: weekly
`)
	file, _, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if file == nil || len(file.Configs) != 1 {
		t.Fatalf("file/configs = %+v", file)
	}
	cfg := file.Configs[0]
	for name, got := range map[string][]byte{
		"AllowRulesJSON":      cfg.AllowRulesJSON,
		"IgnoreRulesJSON":     cfg.IgnoreRulesJSON,
		"RegistriesJSON":      cfg.RegistriesJSON,
		"UnsupportedKeysJSON": cfg.UnsupportedKeysJSON,
	} {
		if string(got) != "[]" {
			t.Errorf("%s = %s, want []", name, got)
		}
	}
	if string(cfg.GroupsJSON) != "{}" {
		t.Errorf("GroupsJSON = %s, want {}", cfg.GroupsJSON)
	}
}
