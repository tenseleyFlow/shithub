// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codescan normalizes external code-scanning reports into the
// repository-owned alert model. SP27 starts with SARIF 2.1.0 ingestion;
// scanner execution remains outside shithub so we do not claim CodeQL parity.
package codescan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	MaxSARIFBytes = 5 * 1024 * 1024
)

var (
	ErrEmptySARIF    = errors.New("codescan: empty SARIF")
	ErrInvalidSARIF  = errors.New("codescan: invalid SARIF")
	ErrSARIFTooLarge = errors.New("codescan: SARIF exceeds maximum size")
	ErrNoSARIFRuns   = errors.New("codescan: SARIF has no runs")
)

// Upload is the normalized report metadata and findings from one SARIF
// document. Raw artifacts and the raw SARIF JSON are deliberately omitted.
type Upload struct {
	ToolName string
	ToolGUID string
	Category string
	Alerts   []Alert
}

// Alert is one normalized code-scanning finding.
type Alert struct {
	ToolName    string
	ToolGUID    string
	RuleID      string
	RuleName    string
	Severity    string
	Message     string
	Path        string
	StartLine   int32
	EndLine     int32
	StartColumn int32
	EndColumn   int32
	Fingerprint string
}

// ParseSARIF parses a SARIF 2.x payload into normalized alerts. The parser is
// intentionally conservative: malformed or location-less results are skipped
// rather than guessed into misleading alerts.
func ParseSARIF(body []byte) (Upload, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return Upload{}, ErrEmptySARIF
	}
	if len(body) > MaxSARIFBytes {
		return Upload{}, ErrSARIFTooLarge
	}

	var doc sarifLog
	if err := json.Unmarshal(body, &doc); err != nil {
		return Upload{}, fmt.Errorf("%w: %v", ErrInvalidSARIF, err)
	}
	if len(doc.Runs) == 0 {
		return Upload{}, ErrNoSARIFRuns
	}

	out := Upload{}
	for _, run := range doc.Runs {
		toolName := clamp(strings.TrimSpace(run.Tool.Driver.Name), 160)
		if toolName == "" {
			toolName = "SARIF"
		}
		toolGUID := clamp(strings.TrimSpace(run.Tool.Driver.GUID), 160)
		category := clamp(strings.TrimSpace(run.AutomationDetails.ID), 160)
		if out.ToolName == "" {
			out.ToolName = toolName
			out.ToolGUID = toolGUID
			out.Category = category
		}

		rulesByID, rulesByIndex := indexRules(run.Tool.Driver.Rules)
		for _, result := range run.Results {
			alert, ok := normalizeResult(toolName, toolGUID, result, rulesByID, rulesByIndex)
			if !ok {
				continue
			}
			out.Alerts = append(out.Alerts, alert)
		}
	}
	return out, nil
}

// Digest returns the lowercase SHA-256 hex digest stored as upload metadata.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func normalizeResult(toolName, toolGUID string, result sarifResult, rulesByID map[string]sarifRule, rulesByIndex map[int]sarifRule) (Alert, bool) {
	loc, ok := firstPhysicalLocation(result.Locations)
	if !ok {
		return Alert{}, false
	}
	path := normalizePath(loc.ArtifactLocation.URI)
	if path == "" {
		return Alert{}, false
	}
	ruleID := strings.TrimSpace(result.RuleID)
	var rule sarifRule
	if ruleID != "" {
		rule = rulesByID[ruleID]
	}
	if rule.ID == "" && result.RuleIndex != nil {
		rule = rulesByIndex[*result.RuleIndex]
	}
	if ruleID == "" {
		ruleID = strings.TrimSpace(rule.ID)
	}
	if ruleID == "" {
		ruleID = "external-rule"
	}

	startLine := int32(loc.Region.StartLine)
	if startLine <= 0 {
		startLine = 1
	}
	endLine := int32(loc.Region.EndLine)
	if endLine < 0 {
		endLine = 0
	}
	startColumn := int32(loc.Region.StartColumn)
	if startColumn < 0 {
		startColumn = 0
	}
	endColumn := int32(loc.Region.EndColumn)
	if endColumn < 0 {
		endColumn = 0
	}
	message := firstNonEmpty(result.Message.Text, result.Message.Markdown, rule.ShortDescription.Text, rule.FullDescription.Text, ruleID)
	alert := Alert{
		ToolName:    toolName,
		ToolGUID:    toolGUID,
		RuleID:      clamp(ruleID, 512),
		RuleName:    clamp(firstNonEmpty(rule.Name, rule.ShortDescription.Text), 512),
		Severity:    severityFor(result, rule),
		Message:     clamp(message, 2000),
		Path:        clamp(path, 1024),
		StartLine:   startLine,
		EndLine:     endLine,
		StartColumn: startColumn,
		EndColumn:   endColumn,
	}
	alert.Fingerprint = fingerprintFor(alert, result)
	return alert, true
}

func indexRules(rules []sarifRule) (map[string]sarifRule, map[int]sarifRule) {
	byID := make(map[string]sarifRule, len(rules))
	byIndex := make(map[int]sarifRule, len(rules))
	for i, rule := range rules {
		if rule.ID != "" {
			byID[rule.ID] = rule
		}
		byIndex[i] = rule
	}
	return byID, byIndex
}

func firstPhysicalLocation(locations []sarifLocation) (sarifPhysicalLocation, bool) {
	for _, loc := range locations {
		if loc.PhysicalLocation.ArtifactLocation.URI != "" {
			return loc.PhysicalLocation, true
		}
	}
	return sarifPhysicalLocation{}, false
}

func normalizePath(uri string) string {
	uri = strings.TrimSpace(uri)
	uri = strings.TrimPrefix(uri, "file://")
	uri = strings.TrimPrefix(uri, "./")
	uri = strings.TrimLeft(uri, "/")
	uri = strings.ReplaceAll(uri, "\\", "/")
	for strings.Contains(uri, "//") {
		uri = strings.ReplaceAll(uri, "//", "/")
	}
	return uri
}

func severityFor(result sarifResult, rule sarifRule) string {
	if sec := propertyString(result.Properties, "security-severity"); sec != "" {
		if v, err := strconv.ParseFloat(sec, 64); err == nil {
			switch {
			case v >= 9:
				return "critical"
			case v >= 7:
				return "high"
			case v >= 4:
				return "moderate"
			default:
				return "low"
			}
		}
	}
	level := firstNonEmpty(result.Level, rule.DefaultConfiguration.Level)
	switch strings.ToLower(level) {
	case "error":
		return "high"
	case "warning":
		return "moderate"
	default:
		return "low"
	}
}

func fingerprintFor(alert Alert, result sarifResult) string {
	for _, values := range []map[string]string{result.Fingerprints, result.PartialFingerprints} {
		if len(values) == 0 {
			continue
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := strings.TrimSpace(values[k]); v != "" {
				return clamp(v, 128)
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		alert.ToolName,
		alert.RuleID,
		alert.Path,
		strconv.Itoa(int(alert.StartLine)),
		alert.Message,
	}, "\x00")))
	return hex.EncodeToString(sum[:20])
}

func propertyString(props map[string]any, key string) string {
	if len(props) == 0 {
		return ""
	}
	raw, ok := props[key]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func clamp(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}

type sarifLog struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool              sarifTool              `json:"tool"`
	AutomationDetails sarifAutomationDetails `json:"automationDetails"`
	Results           []sarifResult          `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name  string      `json:"name"`
	GUID  string      `json:"guid"`
	Rules []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string                    `json:"id"`
	Name                 string                    `json:"name"`
	ShortDescription     sarifMessage              `json:"shortDescription"`
	FullDescription      sarifMessage              `json:"fullDescription"`
	DefaultConfiguration sarifDefaultConfiguration `json:"defaultConfiguration"`
	Properties           map[string]any            `json:"properties"`
}

type sarifDefaultConfiguration struct {
	Level string `json:"level"`
}

type sarifAutomationDetails struct {
	ID string `json:"id"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           *int              `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	Fingerprints        map[string]string `json:"fingerprints"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Properties          map[string]any    `json:"properties"`
}

type sarifMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           sarifRegion           `json:"region"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine   int `json:"startLine"`
	EndLine     int `json:"endLine"`
	StartColumn int `json:"startColumn"`
	EndColumn   int `json:"endColumn"`
}
