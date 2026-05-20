// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan

import "testing"

func TestPatternCapabilitiesCoverBuiltInPatterns(t *testing.T) {
	for _, pattern := range Patterns {
		cap := CapabilityForPattern(pattern.Name)
		if cap.PatternName != pattern.Name {
			t.Fatalf("%s capability pattern name=%q", pattern.Name, cap.PatternName)
		}
		if cap.SecretType == "" || cap.DisplayName == "" || cap.Category == "" {
			t.Fatalf("%s incomplete capability: %+v", pattern.Name, cap)
		}
		if cap.InstanceValidityCheckSupported {
			t.Fatalf("%s unexpectedly claims instance validity support", pattern.Name)
		}
		if cap.InstanceProviderNotificationSupported {
			t.Fatalf("%s unexpectedly claims instance provider notification support", pattern.Name)
		}
		if got := cap.ValidityState(); got != ValidityUnsupported {
			t.Fatalf("%s validity=%q want unsupported", pattern.Name, got)
		}
		if got := cap.ProviderNotificationState(); got != ProviderNotificationUnsupported {
			t.Fatalf("%s provider notification=%q want unsupported", pattern.Name, got)
		}
	}
}

func TestCapabilityForCustomPatternIsLocalOnly(t *testing.T) {
	cap := CapabilityForPattern("custom/team-token")
	if cap.Category != "custom" {
		t.Fatalf("category=%q want custom", cap.Category)
	}
	if cap.ProviderSlug != "" {
		t.Fatalf("provider slug=%q want empty", cap.ProviderSlug)
	}
	if cap.GitHubValidityCheckSupported || cap.GitHubProviderNotificationSupported {
		t.Fatalf("custom pattern should not claim GitHub provider support: %+v", cap)
	}
	if cap.ValidityState() != ValidityUnsupported || cap.ProviderNotificationState() != ProviderNotificationUnsupported {
		t.Fatalf("custom pattern should be unsupported: %+v", cap)
	}
}
