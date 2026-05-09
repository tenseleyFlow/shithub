// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

// The SSRF defenses originally introduced by S33 were lifted into
// `internal/security/ssrf` during S35 so future outbound-fetch paths
// (avatar mirroring, OG-image scraping, …) reuse the same machinery.
// This file keeps the original webhook-package names as type aliases
// so callers don't churn.

import (
	"github.com/tenseleyFlow/shithub/internal/security/ssrf"
)

// SSRFConfig is the re-exported alias for ssrf.Config.
type SSRFConfig = ssrf.Config

// SSRFError is the re-exported alias for ssrf.Error.
type SSRFError = ssrf.Error

// DefaultSSRFConfig returns the production defaults.
func DefaultSSRFConfig() SSRFConfig { return ssrf.Default() }

// IsSSRF reports whether err is or wraps an SSRFError.
func IsSSRF(err error) bool { return ssrf.Is(err) }
