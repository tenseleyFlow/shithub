// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sbom renders repository dependency snapshots as Software Bills of
// Materials. SPDX JSON is intentionally generated from shithub's stored
// dependency inventory; request paths do not execute package managers.
package sbom

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	FormatSPDXJSON = "spdx-json"
)

type Input struct {
	Owner        string
	Repository   string
	BaseURL      string
	HeadSHA      string
	GeneratedAt  time.Time
	Dependencies []Dependency
}

type Dependency struct {
	Ecosystem      string
	PackageName    string
	PackageVersion string
	ManifestPath   string
	PackageManager string
	Direct         bool
}

type document struct {
	SPDXVersion       string         `json:"spdxVersion"`
	DataLicense       string         `json:"dataLicense"`
	SPDXID            string         `json:"SPDXID"`
	Name              string         `json:"name"`
	DocumentNamespace string         `json:"documentNamespace"`
	CreationInfo      creationInfo   `json:"creationInfo"`
	Packages          []pkg          `json:"packages"`
	Relationships     []relationship `json:"relationships"`
}

type creationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type pkg struct {
	Name             string        `json:"name"`
	SPDXID           string        `json:"SPDXID"`
	VersionInfo      string        `json:"versionInfo,omitempty"`
	DownloadLocation string        `json:"downloadLocation"`
	FilesAnalyzed    bool          `json:"filesAnalyzed"`
	Supplier         string        `json:"supplier"`
	ExternalRefs     []externalRef `json:"externalRefs,omitempty"`
}

type externalRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type relationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

// BuildSPDXJSON returns a stable SPDX 2.3 JSON document for dependencies.
func BuildSPDXJSON(in Input) ([]byte, error) {
	owner := strings.TrimSpace(in.Owner)
	repo := strings.TrimSpace(in.Repository)
	head := strings.TrimSpace(in.HeadSHA)
	if owner == "" || repo == "" || head == "" {
		return nil, fmt.Errorf("sbom: owner, repository, and head sha are required")
	}
	generatedAt := in.GeneratedAt.UTC()
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	baseURL := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://shithub.local"
	}
	deps := append([]Dependency(nil), in.Dependencies...)
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].Ecosystem != deps[j].Ecosystem {
			return deps[i].Ecosystem < deps[j].Ecosystem
		}
		if !strings.EqualFold(deps[i].PackageName, deps[j].PackageName) {
			return strings.ToLower(deps[i].PackageName) < strings.ToLower(deps[j].PackageName)
		}
		if deps[i].ManifestPath != deps[j].ManifestPath {
			return deps[i].ManifestPath < deps[j].ManifestPath
		}
		return deps[i].PackageVersion < deps[j].PackageVersion
	})

	doc := document{
		SPDXVersion:       "SPDX-2.3",
		DataLicense:       "CC0-1.0",
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              owner + "/" + repo,
		DocumentNamespace: baseURL + "/sbom/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + url.PathEscape(head),
		CreationInfo: creationInfo{
			Created:  generatedAt.Format(time.RFC3339),
			Creators: []string{"Tool: shithub"},
		},
	}
	seenIDs := map[string]int{}
	for _, dep := range deps {
		name := strings.TrimSpace(dep.PackageName)
		if name == "" {
			continue
		}
		id := packageID(dep)
		if n := seenIDs[id]; n > 0 {
			seenIDs[id] = n + 1
			id = fmt.Sprintf("%s-%d", id, n+1)
		} else {
			seenIDs[id] = 1
		}
		p := pkg{
			Name:             name,
			SPDXID:           id,
			VersionInfo:      strings.TrimSpace(dep.PackageVersion),
			DownloadLocation: "NOASSERTION",
			FilesAnalyzed:    false,
			Supplier:         "NOASSERTION",
		}
		if purl := packageURL(dep); purl != "" {
			p.ExternalRefs = []externalRef{{
				ReferenceCategory: "PACKAGE-MANAGER",
				ReferenceType:     "purl",
				ReferenceLocator:  purl,
			}}
		}
		doc.Packages = append(doc.Packages, p)
		doc.Relationships = append(doc.Relationships, relationship{
			SPDXElementID:      "SPDXRef-DOCUMENT",
			RelationshipType:   "DESCRIBES",
			RelatedSPDXElement: id,
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

var spdxIDPartRE = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

func packageID(dep Dependency) string {
	parts := []string{
		strings.TrimSpace(dep.Ecosystem),
		strings.TrimSpace(dep.PackageName),
		strings.TrimSpace(dep.PackageVersion),
		strings.TrimSpace(dep.ManifestPath),
	}
	id := spdxIDPartRE.ReplaceAllString(strings.Join(parts, "-"), "-")
	id = strings.Trim(id, "-.")
	if id == "" {
		id = "package"
	}
	return "SPDXRef-Package-" + id
}

func packageURL(dep Dependency) string {
	typ := purlType(dep.Ecosystem, dep.PackageManager)
	if typ == "" || strings.TrimSpace(dep.PackageName) == "" {
		return ""
	}
	name := purlEscapePath(strings.TrimSpace(dep.PackageName))
	if dep.PackageVersion != "" {
		name += "@" + purlEscapeSegment(strings.TrimSpace(dep.PackageVersion))
	}
	return "pkg:" + typ + "/" + name
}

func purlEscapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = purlEscapeSegment(part)
	}
	return strings.Join(parts, "/")
}

func purlEscapeSegment(s string) string {
	escaped := url.PathEscape(s)
	return strings.ReplaceAll(escaped, "@", "%40")
}

func purlType(ecosystem, manager string) string {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "go":
		return "golang"
	case "npm":
		return "npm"
	case "rust":
		return "cargo"
	}
	switch strings.ToLower(strings.TrimSpace(manager)) {
	case "gomod":
		return "golang"
	case "npm":
		return "npm"
	case "cargo":
		return "cargo"
	default:
		return ""
	}
}
