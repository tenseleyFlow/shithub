// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan

// ValidityState describes whether shithub has verified a detected credential
// with the provider. SP26d starts conservatively: no built-in provider pattern
// has a configured validator, so findings present as unsupported until a real,
// operator-configured provider integration ships.
type ValidityState string

const (
	ValidityUnsupported ValidityState = "unsupported"
	ValidityUnknown     ValidityState = "unknown"
	ValidityActive      ValidityState = "active"
	ValidityInactive    ValidityState = "inactive"
)

// ProviderNotificationState describes whether shithub notified a third-party
// secret provider about a finding. User/repository notifications are separate
// and live in alert.go; this state is only for provider notification parity.
type ProviderNotificationState string

const (
	ProviderNotificationUnsupported ProviderNotificationState = "unsupported"
	ProviderNotificationDisabled    ProviderNotificationState = "disabled"
	ProviderNotificationPending     ProviderNotificationState = "pending"
	ProviderNotificationSent        ProviderNotificationState = "sent"
	ProviderNotificationFailed      ProviderNotificationState = "failed"
)

// Capability describes a secret pattern's provider-facing behavior. The GitHub
// support flags capture reference parity; instance support is false until
// shithub has a real provider integration and operator opt-in.
type Capability struct {
	PatternName                           string
	SecretType                            string
	DisplayName                           string
	ProviderSlug                          string
	Category                              string
	GitHubProviderNotificationSupported   bool
	GitHubValidityCheckSupported          bool
	InstanceProviderNotificationSupported bool
	InstanceValidityCheckSupported        bool
}

func (c Capability) ValidityState() ValidityState {
	if c.InstanceValidityCheckSupported {
		return ValidityUnknown
	}
	return ValidityUnsupported
}

func (c Capability) ProviderNotificationState() ProviderNotificationState {
	if c.InstanceProviderNotificationSupported {
		return ProviderNotificationDisabled
	}
	return ProviderNotificationUnsupported
}

func (c Capability) ValidityDescription() string {
	switch {
	case c.InstanceValidityCheckSupported:
		return "No provider validity check has run yet."
	case c.GitHubValidityCheckSupported:
		return "GitHub supports validity checks for this provider pattern, but shithub does not currently implement provider validation."
	default:
		return "Provider validity checks are not supported for this pattern."
	}
}

func (c Capability) ProviderNotificationDescription() string {
	switch {
	case c.InstanceProviderNotificationSupported:
		return "Provider notification is available but disabled for this finding."
	case c.GitHubProviderNotificationSupported:
		return "GitHub supports partner/provider notification for this pattern, but shithub does not currently implement provider notification."
	default:
		return "Provider notification is not supported for this pattern."
	}
}

// #nosec G101 -- These values are secret-type identifiers and display labels,
// not credentials.
var patternCapabilities = map[string]Capability{
	"aws-access-key-id": {
		PatternName:                         "aws-access-key-id",
		SecretType:                          "aws_access_key_id",
		DisplayName:                         "AWS access key ID",
		ProviderSlug:                        "aws",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        true,
	},
	"github-token": {
		PatternName:                         "github-token",
		SecretType:                          "github_token",
		DisplayName:                         "GitHub token",
		ProviderSlug:                        "github",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        true,
	},
	"gitlab-pat": {
		PatternName:                         "gitlab-pat",
		SecretType:                          "gitlab_personal_access_token",
		DisplayName:                         "GitLab personal access token",
		ProviderSlug:                        "gitlab",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        false,
	},
	"stripe-live-key": {
		PatternName:                         "stripe-live-key",
		SecretType:                          "stripe_live_secret_key",
		DisplayName:                         "Stripe live secret key",
		ProviderSlug:                        "stripe",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        true,
	},
	"stripe-test-key": {
		PatternName:                         "stripe-test-key",
		SecretType:                          "stripe_test_secret_key",
		DisplayName:                         "Stripe test secret key",
		ProviderSlug:                        "stripe",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        true,
	},
	"slack-token": {
		PatternName:                         "slack-token",
		SecretType:                          "slack_token",
		DisplayName:                         "Slack token",
		ProviderSlug:                        "slack",
		Category:                            "provider",
		GitHubProviderNotificationSupported: true,
		GitHubValidityCheckSupported:        true,
	},
	"private-key-block": {
		PatternName: "private-key-block",
		SecretType:  "private_key",
		DisplayName: "Private key",
		Category:    "generic",
	},
}

// CapabilityForPattern returns a stable capability description for a finding
// pattern. Custom patterns and legacy rows are treated as locally-defined
// detector metadata with no provider integrations.
func CapabilityForPattern(pattern string) Capability {
	if cap, ok := patternCapabilities[pattern]; ok {
		return cap
	}
	cap := Capability{
		PatternName: pattern,
		SecretType:  pattern,
		DisplayName: pattern,
		Category:    "custom",
	}
	return cap
}

// PatternCapabilities returns a copy of the built-in capability registry for
// docs, diagnostics, and future provider-integration tests.
func PatternCapabilities() []Capability {
	out := make([]Capability, 0, len(Patterns))
	for _, pattern := range Patterns {
		out = append(out, CapabilityForPattern(pattern.Name))
	}
	return out
}
