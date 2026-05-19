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
		if strings.ToLower(snap.Dependencies[i].PackageName) != strings.ToLower(snap.Dependencies[j].PackageName) {
			return strings.ToLower(snap.Dependencies[i].PackageName) < strings.ToLower(snap.Dependencies[j].PackageName)
		}
		return snap.Dependencies[i].ManifestPath < snap.Dependencies[j].ManifestPath
	})
	return snap, nil
}

func SupportedManifestPath(p string) bool {
	switch path.Base(p) {
	case "go.mod", "package.json", "package-lock.json":
		return true
	default:
		return false
	}
}

func ParseManifest(p string, body []byte) (Manifest, []Dependency, error) {
	switch path.Base(p) {
	case "go.mod":
		return parseGoMod(p, body)
	case "package.json":
		return parsePackageJSON(p, body)
	case "package-lock.json":
		return parsePackageLock(p, body)
	default:
		return Manifest{}, nil, fmt.Errorf("unsupported manifest %q", p)
	}
}

func parseGoMod(p string, body []byte) (Manifest, []Dependency, error) {
	manifest := Manifest{Path: p, Ecosystem: "go", PackageManager: "gomod"}
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
		Ecosystem:      "go",
		PackageName:    fields[0],
		PackageVersion: fields[1],
		ManifestPath:   manifestPath,
		Scope:          goScope(indirect),
		Direct:         !indirect,
		PackageManager: "gomod",
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
	manifest := Manifest{Path: p, Ecosystem: "npm", PackageManager: "npm"}
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
			Ecosystem:      "npm",
			PackageName:    name,
			PackageVersion: strings.TrimSpace(deps[name]),
			ManifestPath:   manifestPath,
			Scope:          scope,
			Direct:         direct,
			PackageManager: "npm",
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
	manifest := Manifest{Path: p, Ecosystem: "npm", PackageManager: "npm"}
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
			Ecosystem:      "npm",
			PackageName:    name,
			PackageVersion: strings.TrimSpace(pkg.Version),
			ManifestPath:   p,
			LockfilePath:   p,
			Scope:          scope,
			Direct:         false,
			PackageManager: "npm",
			Source:         "package-lock.json",
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
		if strings.ToLower(deps[i].PackageName) != strings.ToLower(deps[j].PackageName) {
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
