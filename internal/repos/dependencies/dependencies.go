// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dependencies builds a repository dependency inventory from
// supported manifests and lockfiles. It intentionally reports only
// ecosystems it can parse locally; unsupported manifests are skipped so
// product copy cannot overclaim coverage.
package dependencies

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

const (
	SchemaVersion       = 1
	DefaultMaxFileBytes = 1024 * 1024
)

type BuildOptions struct {
	Ref          string
	MaxFileBytes int64
}

type Snapshot struct {
	Version       int
	DefaultBranch string
	HeadSHA       string
	Manifests     []Manifest
	Dependencies  []Dependency
}

type Manifest struct {
	Path            string
	Ecosystem       string
	PackageManager  string
	DependencyCount int
}

type Dependency struct {
	Ecosystem      string
	PackageName    string
	PackageVersion string
	ManifestPath   string
	LockfilePath   string
	Scope          string
	Direct         bool
	PackageManager string
	Source         string
}

// Build reads supported manifest files at ref and returns a stable,
// de-duplicated dependency inventory.
func Build(ctx context.Context, gitDir string, opts BuildOptions) (Snapshot, error) {
	ref := strings.TrimSpace(opts.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	maxBytes := opts.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	head, err := repogit.ResolveRefOID(ctx, gitDir, ref)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return Snapshot{Version: SchemaVersion, DefaultBranch: ref}, nil
		}
		return Snapshot{}, err
	}
	paths, err := repogit.ListAllPaths(ctx, gitDir, ref)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{
		Version:       SchemaVersion,
		DefaultBranch: ref,
		HeadSHA:       head,
	}
	seen := map[string]struct{}{}
	for _, p := range paths {
		if !SupportedManifestPath(p) {
			continue
		}
		body, err := repogit.ReadBlobBytes(ctx, gitDir, ref, p, maxBytes)
		if err != nil {
			if errors.Is(err, repogit.ErrBlobTooLarge) {
				continue
			}
			return Snapshot{}, fmt.Errorf("read %s: %w", p, err)
		}
		manifest, deps, err := ParseManifest(p, body)
		if err != nil {
			return Snapshot{}, fmt.Errorf("parse %s: %w", p, err)
		}
		manifest.DependencyCount = len(deps)
		snap.Manifests = append(snap.Manifests, manifest)
		for _, dep := range deps {
			key := dependencyKey(dep)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			snap.Dependencies = append(snap.Dependencies, dep)
		}
	}
	sort.Slice(snap.Manifests, func(i, j int) bool { return snap.Manifests[i].Path < snap.Manifests[j].Path })
	sort.Slice(snap.Dependencies, func(i, j int) bool {
		if snap.Dependencies[i].Ecosystem != snap.Dependencies[j].Ecosystem {
			return snap.Dependencies[i].Ecosystem < snap.Dependencies[j].Ecosystem
		}
		if !strings.EqualFold(snap.Dependencies[i].PackageName, snap.Dependencies[j].PackageName) {
			return strings.ToLower(snap.Dependencies[i].PackageName) < strings.ToLower(snap.Dependencies[j].PackageName)
		}
		return snap.Dependencies[i].ManifestPath < snap.Dependencies[j].ManifestPath
	})
	return snap, nil
}

func ParseManifest(p string, body []byte) (Manifest, []Dependency, error) {
	switch path.Base(p) {
	case "go.mod":
		return parseGoMod(p, body)
	case "package.json":
		return parsePackageJSON(p, body)
	case "package-lock.json":
		return parsePackageLock(p, body)
	case "Cargo.toml":
		return parseCargoToml(p, body)
	case "Cargo.lock":
		return parseCargoLock(p, body)
	default:
		return Manifest{}, nil, fmt.Errorf("unsupported manifest %q", p)
	}
}

func parseGoMod(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: EcosystemGo, PackageManager: PackageManagerGoMod}
	var deps []Dependency
	inRequireBlock := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 16<<10), 256<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "require (") {
			inRequireBlock = true
			continue
		}
		if inRequireBlock && line == ")" {
			inRequireBlock = false
			continue
		}
		if !inRequireBlock {
			if !strings.HasPrefix(line, "require ") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		}
		dep, ok := parseGoRequireLine(p, line)
		if ok {
			deps = append(deps, dep)
		}
	}
	if err := sc.Err(); err != nil {
		return Manifest{}, nil, err
	}
	return manifest, deps, nil
}

func parseGoRequireLine(manifestPath, line string) (Dependency, bool) {
	indirect := strings.Contains(line, "// indirect")
	if idx := strings.Index(line, "//"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Dependency{}, false
	}
	return Dependency{
		Ecosystem:      EcosystemGo,
		PackageName:    fields[0],
		PackageVersion: fields[1],
		ManifestPath:   manifestPath,
		Scope:          goScope(indirect),
		Direct:         !indirect,
		PackageManager: PackageManagerGoMod,
		Source:         "go.mod",
	}, true
}

func goScope(indirect bool) string {
	if indirect {
		return "indirect"
	}
	return "runtime"
}

type packageJSON struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

func parsePackageJSON(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: EcosystemNPM, PackageManager: PackageManagerNPM}
	var doc packageJSON
	if err := json.Unmarshal(body, &doc); err != nil {
		return Manifest{}, nil, err
	}
	var deps []Dependency
	deps = appendNPMManifestDeps(deps, p, doc.Dependencies, "runtime", true, "package.json")
	deps = appendNPMManifestDeps(deps, p, doc.DevDependencies, "development", true, "package.json")
	deps = appendNPMManifestDeps(deps, p, doc.OptionalDependencies, "optional", true, "package.json")
	deps = appendNPMManifestDeps(deps, p, doc.PeerDependencies, "peer", true, "package.json")
	sortDependencies(deps)
	return manifest, deps, nil
}

func appendNPMManifestDeps(out []Dependency, manifestPath string, deps map[string]string, scope string, direct bool, source string) []Dependency {
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, Dependency{
			Ecosystem:      EcosystemNPM,
			PackageName:    name,
			PackageVersion: strings.TrimSpace(deps[name]),
			ManifestPath:   manifestPath,
			Scope:          scope,
			Direct:         direct,
			PackageManager: PackageManagerNPM,
			Source:         source,
		})
	}
	return out
}

type packageLock struct {
	Packages map[string]packageLockPackage `json:"packages"`
}

type packageLockPackage struct {
	Version  string `json:"version"`
	Dev      bool   `json:"dev"`
	Optional bool   `json:"optional"`
}

func parsePackageLock(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: EcosystemNPM, PackageManager: PackageManagerNPM}
	var doc packageLock
	if err := json.Unmarshal(body, &doc); err != nil {
		return Manifest{}, nil, err
	}
	if len(doc.Packages) == 0 {
		return manifest, nil, nil
	}
	var deps []Dependency
	for pkgPath, pkg := range doc.Packages {
		name, ok := packageNameFromNodeModulesPath(pkgPath)
		if !ok {
			continue
		}
		scope := "transitive"
		if pkg.Dev {
			scope = "development"
		}
		if pkg.Optional {
			scope = "optional"
		}
		deps = append(deps, Dependency{
			Ecosystem:      EcosystemNPM,
			PackageName:    name,
			PackageVersion: strings.TrimSpace(pkg.Version),
			ManifestPath:   p,
			LockfilePath:   p,
			Scope:          scope,
			Direct:         false,
			PackageManager: PackageManagerNPM,
			Source:         "package-lock.json",
		})
	}
	sortDependencies(deps)
	return manifest, deps, nil
}

type cargoManifest struct {
	Dependencies      map[string]cargoDependencySpec `toml:"dependencies"`
	DevDependencies   map[string]cargoDependencySpec `toml:"dev-dependencies"`
	BuildDependencies map[string]cargoDependencySpec `toml:"build-dependencies"`
}

type cargoDependencySpec struct {
	Version  string
	Package  string
	Optional bool
}

func (s *cargoDependencySpec) UnmarshalTOML(value any) error {
	switch value := value.(type) {
	case string:
		s.Version = strings.TrimSpace(value)
	case map[string]any:
		for key, raw := range value {
			switch key {
			case "version":
				if version, ok := raw.(string); ok {
					s.Version = strings.TrimSpace(version)
				}
			case "package":
				if packageName, ok := raw.(string); ok {
					s.Package = strings.TrimSpace(packageName)
				}
			case "optional":
				if optional, ok := raw.(bool); ok {
					s.Optional = optional
				}
			}
		}
	}
	return nil
}

func parseCargoToml(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: EcosystemRust, PackageManager: PackageManagerCargo}
	var doc cargoManifest
	if _, err := toml.Decode(string(body), &doc); err != nil {
		return Manifest{}, nil, err
	}
	var deps []Dependency
	deps = appendCargoManifestDeps(deps, p, doc.Dependencies, "runtime")
	deps = appendCargoManifestDeps(deps, p, doc.DevDependencies, "development")
	deps = appendCargoManifestDeps(deps, p, doc.BuildDependencies, "build")
	sortDependencies(deps)
	return manifest, deps, nil
}

func appendCargoManifestDeps(out []Dependency, manifestPath string, deps map[string]cargoDependencySpec, scope string) []Dependency {
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, alias := range names {
		spec := deps[alias]
		name := strings.TrimSpace(alias)
		if spec.Package != "" {
			name = spec.Package
		}
		version := strings.TrimSpace(spec.Version)
		if name == "" || version == "" {
			continue
		}
		depScope := scope
		if depScope == "runtime" && spec.Optional {
			depScope = "optional"
		}
		out = append(out, Dependency{
			Ecosystem:      EcosystemRust,
			PackageName:    name,
			PackageVersion: version,
			ManifestPath:   manifestPath,
			Scope:          depScope,
			Direct:         true,
			PackageManager: PackageManagerCargo,
			Source:         "Cargo.toml",
		})
	}
	return out
}

type cargoLock struct {
	Packages []cargoLockPackage `toml:"package"`
}

type cargoLockPackage struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
	Source  string `toml:"source"`
}

func parseCargoLock(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: EcosystemRust, PackageManager: PackageManagerCargo}
	var doc cargoLock
	if _, err := toml.Decode(string(body), &doc); err != nil {
		return Manifest{}, nil, err
	}
	deps := make([]Dependency, 0, len(doc.Packages))
	for _, pkg := range doc.Packages {
		name := strings.TrimSpace(pkg.Name)
		version := strings.TrimSpace(pkg.Version)
		if name == "" || version == "" {
			continue
		}
		deps = append(deps, Dependency{
			Ecosystem:      EcosystemRust,
			PackageName:    name,
			PackageVersion: version,
			ManifestPath:   p,
			LockfilePath:   p,
			Scope:          "transitive",
			Direct:         false,
			PackageManager: PackageManagerCargo,
			Source:         "Cargo.lock",
		})
	}
	sortDependencies(deps)
	return manifest, deps, nil
}

func packageNameFromNodeModulesPath(p string) (string, bool) {
	parts := strings.Split(p, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "node_modules" || i == len(parts)-1 {
			continue
		}
		name := parts[i+1]
		if strings.HasPrefix(name, "@") && i+2 < len(parts) {
			name += "/" + parts[i+2]
		}
		if name == "" {
			return "", false
		}
		return name, true
	}
	return "", false
}

func sortDependencies(deps []Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		if !strings.EqualFold(deps[i].PackageName, deps[j].PackageName) {
			return strings.ToLower(deps[i].PackageName) < strings.ToLower(deps[j].PackageName)
		}
		return deps[i].Source < deps[j].Source
	})
}

func dependencyKey(d Dependency) string {
	return strings.Join([]string{
		strings.ToLower(d.Ecosystem),
		strings.ToLower(d.PackageName),
		d.ManifestPath,
	}, "\x00")
}
