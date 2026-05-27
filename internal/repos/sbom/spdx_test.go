// SPDX-License-Identifier: AGPL-3.0-or-later

package sbom

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildSPDXJSON(t *testing.T) {
	t.Parallel()
	body, err := BuildSPDXJSON(Input{
		Owner:       "tenseleyFlow",
		Repository:  "shithub",
		BaseURL:     "https://shithub.sh",
		HeadSHA:     "0123456789abcdef",
		GeneratedAt: time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
		Dependencies: []Dependency{
			{
				Ecosystem:      "npm",
				PackageName:    "@primer/octicons",
				PackageVersion: "19.8.0",
				ManifestPath:   "web/package.json",
				PackageManager: "npm",
				Direct:         true,
			},
			{
				Ecosystem:      "go",
				PackageName:    "github.com/go-chi/chi/v5",
				PackageVersion: "v5.2.1",
				ManifestPath:   "go.mod",
				PackageManager: "gomod",
				Direct:         true,
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildSPDXJSON: %v", err)
	}
	var doc struct {
		SPDXVersion       string `json:"spdxVersion"`
		DocumentNamespace string `json:"documentNamespace"`
		Packages          []struct {
			Name         string `json:"name"`
			SPDXID       string `json:"SPDXID"`
			ExternalRefs []struct {
				ReferenceLocator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
		Relationships []struct {
			RelationshipType string `json:"relationshipType"`
		} `json:"relationships"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal SPDX JSON: %v\n%s", err, body)
	}
	if doc.SPDXVersion != "SPDX-2.3" {
		t.Fatalf("spdxVersion=%q", doc.SPDXVersion)
	}
	if doc.DocumentNamespace != "https://shithub.sh/sbom/tenseleyFlow/shithub/0123456789abcdef" {
		t.Fatalf("namespace=%q", doc.DocumentNamespace)
	}
	if len(doc.Packages) != 2 || len(doc.Relationships) != 2 {
		t.Fatalf("packages=%d relationships=%d", len(doc.Packages), len(doc.Relationships))
	}
	if doc.Packages[0].Name != "github.com/go-chi/chi/v5" {
		t.Fatalf("packages not sorted stably: %+v", doc.Packages)
	}
	got := string(body)
	for _, want := range []string{
		`"referenceLocator": "pkg:golang/github.com/go-chi/chi/v5@v5.2.1"`,
		`"referenceLocator": "pkg:npm/%40primer/octicons@19.8.0"`,
		`"relationshipType": "DESCRIBES"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func TestBuildSPDXJSONRequiresRepositoryIdentity(t *testing.T) {
	t.Parallel()
	_, err := BuildSPDXJSON(Input{Owner: "alice", Repository: "repo"})
	if err == nil {
		t.Fatal("expected missing head sha error")
	}
}
