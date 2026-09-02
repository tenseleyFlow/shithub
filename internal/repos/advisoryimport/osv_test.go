// SPDX-License-Identifier: AGPL-3.0-or-later

package advisoryimport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestImportOSVStoresAdvisoryIntelligence(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := reposdb.New()

	result, err := ImportOSVTransactional(ctx, pool, q, strings.NewReader(`[
		{
			"id": "GHSA-test-0001",
			"modified": "2026-05-18T14:12:00Z",
			"published": "2026-05-17T10:11:12Z",
			"aliases": ["CVE-2026-0001", "CWE-79"],
			"summary": "unsafe parser accepts active content",
			"details": "Parser vulnerability in supported package managers.",
			"affected": [
				{
					"package": {"ecosystem": "npm", "name": "@scope/pkg"},
					"ranges": [{"type": "SEMVER", "events": [
						{"introduced": "2.0.0"},
						{"fixed": "2.0.5"}
					]}]
				},
				{
					"package": {"ecosystem": "Go", "name": "example.com/app"},
					"ranges": [{"type": "SEMVER", "events": [
						{"introduced": "0"},
						{"fixed": "1.2.3"}
					]}]
				},
				{
					"package": {"ecosystem": "crates.io", "name": "serde"},
					"ranges": [{"type": "SEMVER", "events": [
						{"introduced": "1.0.0"},
						{"fixed": "1.0.197"}
					]}]
				}
			],
			"references": [
				{"type": "ADVISORY", "url": "https://example.test/advisory/GHSA-test-0001"},
				{"type": "WEB", "url": "https://example.test/details"}
			],
			"severity": [{"type": "CVSS_V3", "score": "8.8"}],
			"database_specific": {
				"severity": "HIGH",
				"cwe_ids": ["CWE-79", "CWE-352"]
			}
		},
		{
			"id": "OSV-unsupported",
			"affected": [{"package": {"ecosystem": "PyPI", "name": "ignored"}, "versions": ["1.0.0"]}]
		}
	]`), ImportOptions{
		SourceName:  "osv",
		SourceURL:   "https://osv.dev",
		License:     "CC-BY-4.0",
		Attribution: "Open Source Vulnerabilities",
	})
	if err != nil {
		t.Fatalf("ImportOSVTransactional: %v", err)
	}
	if result.AdvisoryCount != 2 || result.UpsertedCount != 1 || result.SkippedCount != 1 {
		t.Fatalf("unexpected import result: %+v", result)
	}

	source, err := q.GetDependencyAdvisorySource(ctx, pool, "osv")
	if err != nil {
		t.Fatalf("GetDependencyAdvisorySource: %v", err)
	}
	if source.Kind != "osv" || source.Url != "https://osv.dev" || source.License != "CC-BY-4.0" {
		t.Fatalf("unexpected source row: %+v", source)
	}

	advisory, err := q.GetDependencyAdvisoryBySourceExternalID(ctx, pool, reposdb.GetDependencyAdvisoryBySourceExternalIDParams{
		Source:     "osv",
		ExternalID: "GHSA-test-0001",
	})
	if err != nil {
		t.Fatalf("GetDependencyAdvisoryBySourceExternalID: %v", err)
	}
	if advisory.Ecosystem != "go" || advisory.PackageName != "example.com/app" || advisory.AffectedRange != "< 1.2.3" {
		t.Fatalf("unexpected primary advisory package/range: %+v", advisory)
	}
	if advisory.Severity != "high" || advisory.SourceUrl != "https://example.test/advisory/GHSA-test-0001" {
		t.Fatalf("unexpected advisory metadata: %+v", advisory)
	}
	score, ok := numericFloat(advisory.CvssScore)
	if !ok || score != 8.8 {
		t.Fatalf("unexpected CVSS score: %#v", advisory.CvssScore)
	}
	var cwes []string
	if err := json.Unmarshal(advisory.CweIds, &cwes); err != nil {
		t.Fatalf("unmarshal CWEs: %v", err)
	}
	if strings.Join(cwes, ",") != "CWE-352,CWE-79" {
		t.Fatalf("unexpected CWEs: %v", cwes)
	}

	aliases, err := q.ListDependencyAdvisoryAliases(ctx, pool, advisory.ID)
	if err != nil {
		t.Fatalf("ListDependencyAdvisoryAliases: %v", err)
	}
	if got, want := aliasValues(aliases), "CVE-2026-0001,GHSA-test-0001,CWE-79"; got != want {
		t.Fatalf("unexpected aliases: got %q want %q", got, want)
	}

	ranges, err := q.ListDependencyAdvisoryAffectedRanges(ctx, pool, advisory.ID)
	if err != nil {
		t.Fatalf("ListDependencyAdvisoryAffectedRanges: %v", err)
	}
	if len(ranges) != 3 {
		t.Fatalf("expected 3 affected ranges, got %d: %+v", len(ranges), ranges)
	}
	if ranges[0].Ecosystem != "go" || ranges[0].PackageName != "example.com/app" || ranges[0].RangeExpression != "< 1.2.3" {
		t.Fatalf("unexpected Go range: %+v", ranges[0])
	}
	if ranges[1].Ecosystem != "npm" || ranges[1].PackageName != "@scope/pkg" || ranges[1].RangeExpression != ">= 2.0.0, < 2.0.5" {
		t.Fatalf("unexpected npm range: %+v", ranges[1])
	}
	if ranges[2].Ecosystem != "rust" || ranges[2].PackageName != "serde" || ranges[2].RangeExpression != ">= 1.0.0, < 1.0.197" {
		t.Fatalf("unexpected Rust range: %+v", ranges[2])
	}
}

func TestImportOSVReplacesAliasesAndRanges(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := reposdb.New()

	first := `{
		"id": "GHSA-replace-0001",
		"aliases": ["CVE-2026-1111"],
		"affected": [{"package": {"ecosystem": "Go", "name": "example.com/old"}, "versions": ["1.0.0"]}]
	}`
	if _, err := ImportOSVTransactional(ctx, pool, q, strings.NewReader(first), ImportOptions{SourceName: "osv"}); err != nil {
		t.Fatalf("first import: %v", err)
	}

	second := `{
		"id": "GHSA-replace-0001",
		"aliases": ["CVE-2026-2222"],
		"affected": [{"package": {"ecosystem": "npm", "name": "new-package"}, "ranges": [{"events": [{"introduced": "1.0.0"}, {"fixed": "1.1.0"}]}]}]
	}`
	if _, err := ImportOSVTransactional(ctx, pool, q, strings.NewReader(second), ImportOptions{SourceName: "osv"}); err != nil {
		t.Fatalf("second import: %v", err)
	}

	advisory, err := q.GetDependencyAdvisoryBySourceExternalID(ctx, pool, reposdb.GetDependencyAdvisoryBySourceExternalIDParams{
		Source:     "osv",
		ExternalID: "GHSA-replace-0001",
	})
	if err != nil {
		t.Fatalf("GetDependencyAdvisoryBySourceExternalID: %v", err)
	}
	aliases, err := q.ListDependencyAdvisoryAliases(ctx, pool, advisory.ID)
	if err != nil {
		t.Fatalf("ListDependencyAdvisoryAliases: %v", err)
	}
	if got, want := aliasValues(aliases), "CVE-2026-2222,GHSA-replace-0001"; got != want {
		t.Fatalf("unexpected aliases after replacement: got %q want %q", got, want)
	}
	ranges, err := q.ListDependencyAdvisoryAffectedRanges(ctx, pool, advisory.ID)
	if err != nil {
		t.Fatalf("ListDependencyAdvisoryAffectedRanges: %v", err)
	}
	if len(ranges) != 1 || ranges[0].Ecosystem != "npm" || ranges[0].PackageName != "new-package" {
		t.Fatalf("unexpected ranges after replacement: %+v", ranges)
	}
}

func TestDecodeOSVRejectsOversizedInput(t *testing.T) {
	_, err := decodeOSV(strings.NewReader(`{"id":"GHSA-too-large"}`), 4)
	if err == nil {
		t.Fatal("expected oversized import to fail")
	}
	if !strings.Contains(err.Error(), "exceeds 4 byte limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func aliasValues(rows []reposdb.DependencyAdvisoryAlias) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.AliasValue)
	}
	return strings.Join(values, ",")
}

// TestMustJSONRendersEmptyArrayForNilSlice pins the jsonb array shape
// the dependency_advisories check constraints require: an advisory with
// no CWE IDs or no references must store [], not null.
func TestMustJSONRendersEmptyArrayForNilSlice(t *testing.T) {
	var nilSlice []string
	for name, value := range map[string]interface{}{
		"nil slice":   nilSlice,
		"nil":         nil,
		"empty slice": []string{},
	} {
		if got := string(mustJSON(value)); got != "[]" {
			t.Errorf("mustJSON(%s) = %s, want []", name, got)
		}
	}
	if got := string(mustJSON([]string{"CWE-79"})); got != `["CWE-79"]` {
		t.Errorf(`mustJSON(["CWE-79"]) = %s`, got)
	}
}
