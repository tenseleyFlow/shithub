// SPDX-License-Identifier: AGPL-3.0-or-later

// Package advisoryimport imports operator-controlled vulnerability advisory
// files into shithub's local advisory catalog.
package advisoryimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

const DefaultMaxOSVImportBytes int64 = 100 << 20

type ImportOptions struct {
	SourceName  string
	SourceURL   string
	License     string
	Attribution string
	MaxBytes    int64
}

type ImportResult struct {
	AdvisoryCount  int
	UpsertedCount  int
	WithdrawnCount int
	SkippedCount   int
}

// ImportOSV imports one OSV object or an array of OSV objects from r. It does
// not fetch remote data; callers must provide an operator-approved file stream.
func ImportOSV(ctx context.Context, db reposdb.DBTX, q *reposdb.Queries, r io.Reader, opts ImportOptions) (ImportResult, error) {
	if q == nil {
		q = reposdb.New()
	}
	sourceName := strings.TrimSpace(opts.SourceName)
	if sourceName == "" {
		return ImportResult{}, errors.New("advisory import: missing source name")
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOSVImportBytes
	}
	vulns, err := decodeOSV(r, maxBytes)
	if err != nil {
		return ImportResult{}, err
	}
	if _, err := q.UpsertDependencyAdvisorySource(ctx, db, reposdb.UpsertDependencyAdvisorySourceParams{
		Name:           sourceName,
		Kind:           "osv",
		DisplayName:    defaultString(sourceName, "OSV"),
		Url:            strings.TrimSpace(opts.SourceURL),
		License:        strings.TrimSpace(opts.License),
		Attribution:    strings.TrimSpace(opts.Attribution),
		Enabled:        true,
		LastSyncStatus: "running",
		Metadata:       []byte("{}"),
	}); err != nil {
		return ImportResult{}, fmt.Errorf("upsert advisory source: %w", err)
	}

	result := ImportResult{AdvisoryCount: len(vulns)}
	for _, vuln := range vulns {
		record, ok := normalizeOSV(vuln, sourceName)
		if !ok {
			result.SkippedCount++
			continue
		}
		advisory, err := q.UpsertDependencyAdvisoryWithMetadata(ctx, db, reposdb.UpsertDependencyAdvisoryWithMetadataParams{
			Source:          record.Source,
			ExternalID:      record.ExternalID,
			Ecosystem:       record.Ecosystem,
			PackageName:     record.PackageName,
			AffectedRange:   record.AffectedRange,
			PatchedVersions: record.PatchedVersions,
			Severity:        record.Severity,
			Summary:         record.Summary,
			Description:     record.Description,
			ReferenceUrls:   mustJSON(record.ReferenceURLs),
			PublishedAt:     timestamptz(record.PublishedAt),
			WithdrawnAt:     timestamptz(record.WithdrawnAt),
			ModifiedAt:      timestamptz(record.ModifiedAt),
			SourceUrl:       record.SourceURL,
			CvssScore:       record.CVSSScore,
			CvssVector:      record.CVSSVector,
			CweIds:          mustJSON(record.CWEIDs),
		})
		if err != nil {
			return result, fmt.Errorf("upsert advisory %s: %w", record.ExternalID, err)
		}
		if err := q.DeleteDependencyAdvisoryAliases(ctx, db, advisory.ID); err != nil {
			return result, fmt.Errorf("replace advisory aliases %s: %w", record.ExternalID, err)
		}
		for _, alias := range record.Aliases {
			if err := q.InsertDependencyAdvisoryAlias(ctx, db, reposdb.InsertDependencyAdvisoryAliasParams{
				AdvisoryID: advisory.ID,
				AliasKind:  alias.Kind,
				AliasValue: alias.Value,
			}); err != nil {
				return result, fmt.Errorf("insert advisory alias %s: %w", record.ExternalID, err)
			}
		}
		if err := q.DeleteDependencyAdvisoryAffectedRanges(ctx, db, advisory.ID); err != nil {
			return result, fmt.Errorf("replace affected ranges %s: %w", record.ExternalID, err)
		}
		for _, affected := range record.Affected {
			if err := q.InsertDependencyAdvisoryAffectedRange(ctx, db, reposdb.InsertDependencyAdvisoryAffectedRangeParams{
				AdvisoryID:      advisory.ID,
				Ecosystem:       affected.Ecosystem,
				PackageName:     affected.PackageName,
				RangeExpression: affected.RangeExpression,
				Introduced:      affected.Introduced,
				Fixed:           affected.Fixed,
				LastAffected:    affected.LastAffected,
				Metadata:        []byte("{}"),
			}); err != nil {
				return result, fmt.Errorf("insert affected range %s: %w", record.ExternalID, err)
			}
		}
		result.UpsertedCount++
		if record.WithdrawnAt != nil {
			result.WithdrawnCount++
		}
	}
	return result, nil
}

type OSVVulnerability struct {
	ID               string                 `json:"id"`
	Modified         string                 `json:"modified"`
	Published        string                 `json:"published"`
	Withdrawn        string                 `json:"withdrawn"`
	Aliases          []string               `json:"aliases"`
	Summary          string                 `json:"summary"`
	Details          string                 `json:"details"`
	Affected         []OSVAffected          `json:"affected"`
	References       []OSVReference         `json:"references"`
	Severity         []OSVSeverity          `json:"severity"`
	DatabaseSpecific map[string]interface{} `json:"database_specific"`
}

type OSVAffected struct {
	Package          OSVPackage             `json:"package"`
	Ranges           []OSVRange             `json:"ranges"`
	Versions         []string               `json:"versions"`
	DatabaseSpecific map[string]interface{} `json:"database_specific"`
}

type OSVPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

type OSVRange struct {
	Type   string     `json:"type"`
	Events []OSVEvent `json:"events"`
}

type OSVEvent struct {
	Introduced   string `json:"introduced"`
	Fixed        string `json:"fixed"`
	LastAffected string `json:"last_affected"`
}

type OSVReference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type OSVSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

type advisoryRecord struct {
	Source          string
	ExternalID      string
	Ecosystem       string
	PackageName     string
	AffectedRange   string
	PatchedVersions string
	Severity        string
	Summary         string
	Description     string
	ReferenceURLs   []string
	PublishedAt     *time.Time
	WithdrawnAt     *time.Time
	ModifiedAt      *time.Time
	SourceURL       string
	CVSSScore       pgtype.Numeric
	CVSSVector      string
	CWEIDs          []string
	Aliases         []advisoryAlias
	Affected        []affectedRange
}

type advisoryAlias struct {
	Kind  string
	Value string
}

type affectedRange struct {
	Ecosystem       string
	PackageName     string
	RangeExpression string
	Introduced      string
	Fixed           string
	LastAffected    string
}

func decodeOSV(r io.Reader, maxBytes int64) ([]OSVVulnerability, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OSV import: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("read OSV import: exceeds %d byte limit", maxBytes)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("read OSV import: empty file")
	}
	if body[0] == '[' {
		var out []OSVVulnerability
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decode OSV array: %w", err)
		}
		return out, nil
	}
	var one OSVVulnerability
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, fmt.Errorf("decode OSV object: %w", err)
	}
	return []OSVVulnerability{one}, nil
}

func normalizeOSV(v OSVVulnerability, sourceName string) (advisoryRecord, bool) {
	id := strings.TrimSpace(v.ID)
	if id == "" {
		return advisoryRecord{}, false
	}
	affected := normalizeAffected(v.Affected)
	if len(affected) == 0 {
		return advisoryRecord{}, false
	}
	sort.Slice(affected, func(i, j int) bool {
		if affected[i].Ecosystem != affected[j].Ecosystem {
			return affected[i].Ecosystem < affected[j].Ecosystem
		}
		if !strings.EqualFold(affected[i].PackageName, affected[j].PackageName) {
			return strings.ToLower(affected[i].PackageName) < strings.ToLower(affected[j].PackageName)
		}
		return affected[i].RangeExpression < affected[j].RangeExpression
	})
	primary := affected[0]
	patched := firstFixedVersion(affected)
	sourceURL := firstReferenceURL(v.References)
	if sourceURL == "" {
		sourceURL = "https://osv.dev/vulnerability/" + id
	}
	cvssScore, cvssVector := normalizeCVSS(v.Severity)
	return advisoryRecord{
		Source:          sourceName,
		ExternalID:      id,
		Ecosystem:       primary.Ecosystem,
		PackageName:     primary.PackageName,
		AffectedRange:   primary.RangeExpression,
		PatchedVersions: patched,
		Severity:        normalizeSeverity(v),
		Summary:         defaultString(strings.TrimSpace(v.Summary), id),
		Description:     strings.TrimSpace(v.Details),
		ReferenceURLs:   referenceURLs(v.References),
		PublishedAt:     parseOptionalTime(v.Published),
		WithdrawnAt:     parseOptionalTime(v.Withdrawn),
		ModifiedAt:      parseOptionalTime(v.Modified),
		SourceURL:       sourceURL,
		CVSSScore:       cvssScore,
		CVSSVector:      cvssVector,
		CWEIDs:          cweIDs(v),
		Aliases:         aliases(v),
		Affected:        affected,
	}, true
}

func normalizeAffected(in []OSVAffected) []affectedRange {
	var out []affectedRange
	for _, affected := range in {
		ecosystem, ok := normalizeEcosystem(affected.Package.Ecosystem)
		if !ok {
			continue
		}
		name := strings.TrimSpace(affected.Package.Name)
		if name == "" {
			continue
		}
		out = append(out, rangesFromEvents(ecosystem, name, affected.Ranges)...)
		if len(affected.Ranges) == 0 {
			for _, version := range affected.Versions {
				version = strings.TrimSpace(version)
				if version == "" {
					continue
				}
				out = append(out, affectedRange{
					Ecosystem:       ecosystem,
					PackageName:     name,
					RangeExpression: version,
				})
			}
		}
	}
	return dedupeAffected(out)
}

func rangesFromEvents(ecosystem, packageName string, ranges []OSVRange) []affectedRange {
	var out []affectedRange
	for _, r := range ranges {
		if !supportedOSVRangeType(r.Type) {
			continue
		}
		introduced := ""
		for _, event := range r.Events {
			switch {
			case strings.TrimSpace(event.Introduced) != "":
				introduced = strings.TrimSpace(event.Introduced)
			case strings.TrimSpace(event.Fixed) != "":
				fixed := strings.TrimSpace(event.Fixed)
				out = append(out, affectedRange{
					Ecosystem:       ecosystem,
					PackageName:     packageName,
					RangeExpression: rangeExpression(introduced, fixed, ""),
					Introduced:      introduced,
					Fixed:           fixed,
				})
				introduced = ""
			case strings.TrimSpace(event.LastAffected) != "":
				last := strings.TrimSpace(event.LastAffected)
				out = append(out, affectedRange{
					Ecosystem:       ecosystem,
					PackageName:     packageName,
					RangeExpression: rangeExpression(introduced, "", last),
					Introduced:      introduced,
					LastAffected:    last,
				})
			}
		}
		if introduced != "" {
			out = append(out, affectedRange{
				Ecosystem:       ecosystem,
				PackageName:     packageName,
				RangeExpression: rangeExpression(introduced, "", ""),
				Introduced:      introduced,
			})
		}
	}
	return out
}

func supportedOSVRangeType(kind string) bool {
	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case "", "SEMVER", "ECOSYSTEM":
		return true
	default:
		return false
	}
}

func rangeExpression(introduced, fixed, lastAffected string) string {
	introduced = strings.TrimSpace(introduced)
	fixed = strings.TrimSpace(fixed)
	lastAffected = strings.TrimSpace(lastAffected)
	switch {
	case fixed != "" && (introduced == "" || introduced == "0"):
		return "< " + fixed
	case fixed != "":
		return ">= " + introduced + ", < " + fixed
	case lastAffected != "" && (introduced == "" || introduced == "0"):
		return "<= " + lastAffected
	case lastAffected != "":
		return ">= " + introduced + ", <= " + lastAffected
	case introduced != "":
		return ">= " + introduced
	default:
		return "*"
	}
}

func dedupeAffected(in []affectedRange) []affectedRange {
	seen := make(map[string]struct{}, len(in))
	out := make([]affectedRange, 0, len(in))
	for _, item := range in {
		if strings.TrimSpace(item.RangeExpression) == "" {
			item.RangeExpression = "*"
		}
		key := item.Ecosystem + "\x00" + strings.ToLower(item.PackageName) + "\x00" + item.RangeExpression + "\x00" + item.Introduced + "\x00" + item.Fixed + "\x00" + item.LastAffected
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func normalizeEcosystem(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "go", "gomod":
		return "go", true
	case "npm":
		return "npm", true
	case "crates.io", "cargo", "rust":
		return "rust", true
	default:
		return "", false
	}
}

func normalizeSeverity(v OSVVulnerability) string {
	if raw, ok := v.DatabaseSpecific["severity"].(string); ok {
		if sev, ok := knownSeverity(raw); ok {
			return sev
		}
	}
	score, _ := normalizeCVSS(v.Severity)
	if !score.Valid {
		return "moderate"
	}
	f, ok := numericFloat(score)
	if !ok {
		return "moderate"
	}
	switch {
	case f >= 9:
		return "critical"
	case f >= 7:
		return "high"
	case f >= 4:
		return "moderate"
	default:
		return "low"
	}
}

func knownSeverity(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "low":
		return "low", true
	case "moderate", "medium":
		return "moderate", true
	case "high":
		return "high", true
	case "critical":
		return "critical", true
	default:
		return "", false
	}
}

func normalizeCVSS(severities []OSVSeverity) (pgtype.Numeric, string) {
	for _, severity := range severities {
		score := strings.TrimSpace(severity.Score)
		if score == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(score), "CVSS:") {
			return pgtype.Numeric{}, score
		}
		numeric, ok := parseCVSSNumeric(score)
		if ok {
			return numeric, ""
		}
	}
	return pgtype.Numeric{}, ""
}

func parseCVSSNumeric(value string) (pgtype.Numeric, bool) {
	value = strings.TrimSpace(value)
	m := regexp.MustCompile(`^\d+(?:\.\d)?$`).FindString(value)
	if m == "" {
		return pgtype.Numeric{}, false
	}
	var whole, frac int64
	parts := strings.SplitN(m, ".", 2)
	for _, ch := range parts[0] {
		whole = whole*10 + int64(ch-'0')
	}
	if len(parts) == 2 && parts[1] != "" {
		frac = int64(parts[1][0] - '0')
	}
	if whole > 10 || (whole == 10 && frac > 0) {
		return pgtype.Numeric{}, false
	}
	return pgtype.Numeric{Int: bigInt(whole*10 + frac), Exp: -1, Valid: true}, true
}

func numericFloat(n pgtype.Numeric) (float64, bool) {
	if !n.Valid || n.Int == nil || n.Exp != -1 {
		return 0, false
	}
	return float64(n.Int.Int64()) / 10, true
}

func bigInt(v int64) *big.Int {
	return big.NewInt(v)
}

func aliases(v OSVVulnerability) []advisoryAlias {
	values := append([]string{v.ID}, v.Aliases...)
	seen := map[string]struct{}{}
	out := make([]advisoryAlias, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		kind := aliasKind(value)
		key := kind + "\x00" + strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, advisoryAlias{Kind: kind, Value: value})
	}
	return out
}

func aliasKind(value string) string {
	upper := strings.ToUpper(value)
	switch {
	case strings.HasPrefix(upper, "CVE-"):
		return "cve"
	case strings.HasPrefix(upper, "GHSA-"):
		return "ghsa"
	case strings.HasPrefix(upper, "OSV-"):
		return "osv"
	default:
		return "other"
	}
}

func cweIDs(v OSVVulnerability) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range v.Aliases {
		if strings.HasPrefix(strings.ToUpper(value), "CWE-") {
			if _, ok := seen[strings.ToUpper(value)]; !ok {
				seen[strings.ToUpper(value)] = struct{}{}
				out = append(out, value)
			}
		}
	}
	if raw, ok := v.DatabaseSpecific["cwe_ids"].([]interface{}); ok {
		for _, item := range raw {
			value, ok := item.(string)
			if !ok || strings.TrimSpace(value) == "" {
				continue
			}
			key := strings.ToUpper(strings.TrimSpace(value))
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, strings.TrimSpace(value))
		}
	}
	sort.Strings(out)
	return out
}

func referenceURLs(refs []OSVReference) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		u := strings.TrimSpace(ref.URL)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func firstReferenceURL(refs []OSVReference) string {
	urls := referenceURLs(refs)
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}

func firstFixedVersion(ranges []affectedRange) string {
	for _, item := range ranges {
		if strings.TrimSpace(item.Fixed) != "" {
			return strings.TrimSpace(item.Fixed)
		}
	}
	return ""
}

func parseOptionalTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return &ts
	}
	return nil
}

func timestamptz(ts *time.Time) pgtype.Timestamptz {
	if ts == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: ts.UTC(), Valid: true}
}

func mustJSON(value interface{}) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		return []byte("[]")
	}
	return body
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fallback)
}

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// ImportOSVTransactional wraps ImportOSV in one transaction.
func ImportOSVTransactional(ctx context.Context, db txBeginner, q *reposdb.Queries, r io.Reader, opts ImportOptions) (ImportResult, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	result, err := ImportOSV(ctx, tx, q, r, opts)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	committed = true
	return result, nil
}
