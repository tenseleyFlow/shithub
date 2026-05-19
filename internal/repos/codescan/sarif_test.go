// SPDX-License-Identifier: AGPL-3.0-or-later

package codescan

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSARIFNormalizesAlerts(t *testing.T) {
	body := []byte(`{
	  "version": "2.1.0",
	  "runs": [{
	    "automationDetails": {"id": "go-security"},
	    "tool": {"driver": {
	      "name": "gosec",
	      "guid": "tool-guid",
	      "rules": [{
	        "id": "G401",
	        "name": "weak crypto",
	        "shortDescription": {"text": "Weak cryptography"},
	        "defaultConfiguration": {"level": "warning"}
	      }]
	    }},
	    "results": [{
	      "ruleId": "G401",
	      "message": {"text": "Use of weak crypto primitive"},
	      "locations": [{"physicalLocation": {
	        "artifactLocation": {"uri": "./internal/app/main.go"},
	        "region": {"startLine": 42, "startColumn": 7}
	      }}],
	      "partialFingerprints": {"primaryLocationLineHash": "stable-fp"},
	      "properties": {"security-severity": "8.1"}
	    }]
	  }]
	}`)

	upload, err := ParseSARIF(body)
	if err != nil {
		t.Fatalf("ParseSARIF: %v", err)
	}
	if upload.ToolName != "gosec" || upload.ToolGUID != "tool-guid" || upload.Category != "go-security" {
		t.Fatalf("upload metadata = %+v", upload)
	}
	if len(upload.Alerts) != 1 {
		t.Fatalf("alerts len=%d, want 1", len(upload.Alerts))
	}
	alert := upload.Alerts[0]
	if alert.RuleID != "G401" || alert.RuleName != "weak crypto" {
		t.Errorf("rule fields = %q/%q", alert.RuleID, alert.RuleName)
	}
	if alert.Severity != "high" {
		t.Errorf("severity=%q, want high", alert.Severity)
	}
	if alert.Path != "internal/app/main.go" || alert.StartLine != 42 || alert.StartColumn != 7 {
		t.Errorf("location = %s:%d:%d", alert.Path, alert.StartLine, alert.StartColumn)
	}
	if alert.Fingerprint != "stable-fp" {
		t.Errorf("fingerprint=%q, want stable-fp", alert.Fingerprint)
	}
}

func TestParseSARIFDerivesFingerprintAndSeverity(t *testing.T) {
	body := []byte(`{
	  "runs": [{
	    "tool": {"driver": {"name": "staticcheck", "rules": [{"id": "SA1019"}]}},
	    "results": [{
	      "ruleIndex": 0,
	      "level": "error",
	      "message": {"text": "deprecated call"},
	      "locations": [{"physicalLocation": {
	        "artifactLocation": {"uri": "pkg\\thing.go"},
	        "region": {"startLine": 3}
	      }}]
	    }]
	  }]
	}`)

	upload, err := ParseSARIF(body)
	if err != nil {
		t.Fatalf("ParseSARIF: %v", err)
	}
	alert := upload.Alerts[0]
	if alert.RuleID != "SA1019" || alert.Severity != "high" {
		t.Fatalf("alert = %+v", alert)
	}
	if alert.Path != "pkg/thing.go" {
		t.Errorf("path=%q", alert.Path)
	}
	if len(alert.Fingerprint) == 0 || strings.Contains(alert.Fingerprint, "deprecated") {
		t.Errorf("derived fingerprint should be stable digest, got %q", alert.Fingerprint)
	}
}

func TestParseSARIFRejectsUnsupportedPayloads(t *testing.T) {
	if _, err := ParseSARIF([]byte("")); !errors.Is(err, ErrEmptySARIF) {
		t.Fatalf("empty err=%v", err)
	}
	if _, err := ParseSARIF([]byte(`{"runs":[]}`)); !errors.Is(err, ErrNoSARIFRuns) {
		t.Fatalf("no runs err=%v", err)
	}
	tooLarge := make([]byte, MaxSARIFBytes+1)
	if _, err := ParseSARIF(tooLarge); !errors.Is(err, ErrSARIFTooLarge) {
		t.Fatalf("too large err=%v", err)
	}
}

func TestParseSARIFAllowsZeroAlertUploads(t *testing.T) {
	upload, err := ParseSARIF([]byte(`{
	  "runs": [{
	    "tool": {"driver": {"name": "semgrep"}},
	    "results": []
	  }]
	}`))
	if err != nil {
		t.Fatalf("ParseSARIF: %v", err)
	}
	if upload.ToolName != "semgrep" {
		t.Fatalf("tool name=%q", upload.ToolName)
	}
	if len(upload.Alerts) != 0 {
		t.Fatalf("alerts len=%d, want 0", len(upload.Alerts))
	}
}
