// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencies

import "testing"

func TestDiffDependencies(t *testing.T) {
	t.Parallel()
	base := Snapshot{Dependencies: []Dependency{
		testDep("go", "example.com/unchanged", "v1.0.0", "go.mod"),
		testDep("go", "example.com/removed", "v1.0.0", "go.mod"),
		testDep("npm", "@scope/pkg", "1.0.0", "package.json"),
	}}
	head := Snapshot{Dependencies: []Dependency{
		testDep("go", "example.com/unchanged", "v1.0.0", "go.mod"),
		testDep("go", "example.com/added", "v0.1.0", "go.mod"),
		testDep("npm", "@scope/pkg", "1.2.0", "package.json"),
	}}

	got := Diff(base, head)
	if len(got) != 3 {
		t.Fatalf("len(Diff) = %d, want 3: %#v", len(got), got)
	}
	assertChange(t, got[0], ChangeAdded, "go", "example.com/added", "", "v0.1.0")
	assertChange(t, got[1], ChangeRemoved, "go", "example.com/removed", "v1.0.0", "")
	assertChange(t, got[2], ChangeChanged, "npm", "@scope/pkg", "1.0.0", "1.2.0")
}

func testDep(ecosystem, name, version, manifest string) Dependency {
	return Dependency{
		Ecosystem:      ecosystem,
		PackageName:    name,
		PackageVersion: version,
		ManifestPath:   manifest,
		Scope:          "runtime",
		Direct:         true,
		PackageManager: ecosystem,
		Source:         manifest,
	}
}

func assertChange(t *testing.T, got Change, kind ChangeKind, ecosystem, name, oldVersion, newVersion string) {
	t.Helper()
	if got.Kind != kind || got.Ecosystem != ecosystem || got.PackageName != name ||
		got.OldVersion != oldVersion || got.NewVersion != newVersion {
		t.Fatalf("change = %#v, want %s %s/%s %q -> %q",
			got, kind, ecosystem, name, oldVersion, newVersion)
	}
}
