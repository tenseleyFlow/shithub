// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencies

import "testing"

func TestParseGoMod(t *testing.T) {
	t.Parallel()
	_, deps, err := ParseManifest("go.mod", []byte(`module example.test/app

require (
	github.com/go-chi/chi/v5 v5.2.5
	golang.org/x/net v0.53.0 // indirect
)

require github.com/jackc/pgx/v5 v5.9.2
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("deps len = %d, want 3: %#v", len(deps), deps)
	}
	assertDep(t, deps, "github.com/go-chi/chi/v5", "v5.2.5", "runtime", true)
	assertDep(t, deps, "golang.org/x/net", "v0.53.0", "indirect", false)
	assertDep(t, deps, "github.com/jackc/pgx/v5", "v5.9.2", "runtime", true)
}

func TestParsePackageJSON(t *testing.T) {
	t.Parallel()
	_, deps, err := ParseManifest("web/package.json", []byte(`{
  "dependencies": {"@primer/css": "21.5.1"},
  "devDependencies": {"eslint": "^9.0.0"},
  "optionalDependencies": {"fsevents": "2.3.3"}
}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	assertDep(t, deps, "@primer/css", "21.5.1", "runtime", true)
	assertDep(t, deps, "eslint", "^9.0.0", "development", true)
	assertDep(t, deps, "fsevents", "2.3.3", "optional", true)
}

func TestParsePackageLock(t *testing.T) {
	t.Parallel()
	_, deps, err := ParseManifest("package-lock.json", []byte(`{
  "packages": {
    "": {"name": "demo"},
    "node_modules/@scope/pkg": {"version": "1.2.3"},
    "node_modules/parent/node_modules/child": {"version": "4.5.6", "dev": true}
  }
}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	assertDep(t, deps, "@scope/pkg", "1.2.3", "transitive", false)
	assertDep(t, deps, "child", "4.5.6", "development", false)
}

func assertDep(t *testing.T, deps []Dependency, name, version, scope string, direct bool) {
	t.Helper()
	for _, dep := range deps {
		if dep.PackageName != name {
			continue
		}
		if dep.PackageVersion != version || dep.Scope != scope || dep.Direct != direct {
			t.Fatalf("dep %s = version=%q scope=%q direct=%v, want %q/%q/%v",
				name, dep.PackageVersion, dep.Scope, dep.Direct, version, scope, direct)
		}
		return
	}
	t.Fatalf("dep %s not found in %#v", name, deps)
}
