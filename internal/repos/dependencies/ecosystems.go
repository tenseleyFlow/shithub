// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencies

import (
	"path"
	"strings"
)

const (
	EcosystemGo   = "go"
	EcosystemNPM  = "npm"
	EcosystemRust = "rust"

	PackageManagerGoMod = "gomod"
	PackageManagerNPM   = "npm"
	PackageManagerCargo = "cargo"
)

// EcosystemSupport describes the production behavior shithub currently ships.
// Product and docs copy should derive from this shape instead of implying
// package-manager parity that does not exist yet.
type EcosystemSupport struct {
	ID                    string
	DisplayName           string
	PackageManagers       []string
	ManifestFiles         []string
	Inventory             bool
	AdvisoryRangeMatching bool
	DependencyReview      bool
	SecurityUpdatePRs     bool
	VersionUpdatePRs      bool
}

var supportMatrix = []EcosystemSupport{
	{
		ID:                    EcosystemGo,
		DisplayName:           "Go",
		PackageManagers:       []string{PackageManagerGoMod},
		ManifestFiles:         []string{"go.mod"},
		Inventory:             true,
		AdvisoryRangeMatching: true,
		DependencyReview:      true,
		SecurityUpdatePRs:     true,
		VersionUpdatePRs:      true,
	},
	{
		ID:                    EcosystemNPM,
		DisplayName:           "npm",
		PackageManagers:       []string{PackageManagerNPM},
		ManifestFiles:         []string{"package.json", "package-lock.json"},
		Inventory:             true,
		AdvisoryRangeMatching: true,
		DependencyReview:      true,
		SecurityUpdatePRs:     true,
		VersionUpdatePRs:      true,
	},
	{
		ID:                    EcosystemRust,
		DisplayName:           "Rust",
		PackageManagers:       []string{PackageManagerCargo},
		ManifestFiles:         []string{"Cargo.toml", "Cargo.lock"},
		Inventory:             true,
		AdvisoryRangeMatching: true,
		DependencyReview:      true,
		SecurityUpdatePRs:     false,
		VersionUpdatePRs:      false,
	},
}

var supportedManifestBases = func() map[string]struct{} {
	out := make(map[string]struct{})
	for _, ecosystem := range supportMatrix {
		if !ecosystem.Inventory {
			continue
		}
		for _, manifest := range ecosystem.ManifestFiles {
			out[manifest] = struct{}{}
		}
	}
	return out
}()

// SupportMatrix returns a copy of shithub's dependency ecosystem support
// matrix, including capabilities that intentionally remain unshipped.
func SupportMatrix() []EcosystemSupport {
	out := make([]EcosystemSupport, len(supportMatrix))
	for i, row := range supportMatrix {
		out[i] = row
		out[i].PackageManagers = append([]string(nil), row.PackageManagers...)
		out[i].ManifestFiles = append([]string(nil), row.ManifestFiles...)
	}
	return out
}

func SupportedManifestPath(p string) bool {
	_, ok := supportedManifestBases[path.Base(p)]
	return ok
}

func SupportedReviewManifestSummary() string {
	names := make([]string, 0, len(supportMatrix))
	for _, row := range supportMatrix {
		if row.DependencyReview {
			names = append(names, row.DisplayName)
		}
	}
	return formatDisplayList(names) + " manifests"
}

func formatDisplayList(values []string) string {
	switch len(values) {
	case 0:
		return "supported"
	case 1:
		return values[0]
	case 2:
		return values[0] + " and " + values[1]
	default:
		return strings.Join(values[:len(values)-1], ", ") + ", and " + values[len(values)-1]
	}
}
