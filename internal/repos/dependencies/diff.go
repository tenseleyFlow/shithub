// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencies

import (
	"sort"
	"strings"
)

type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"
	ChangeRemoved ChangeKind = "removed"
	ChangeChanged ChangeKind = "changed"
)

type Change struct {
	Kind           ChangeKind
	Ecosystem      string
	PackageName    string
	ManifestPath   string
	LockfilePath   string
	OldVersion     string
	NewVersion     string
	Scope          string
	Direct         bool
	PackageManager string
	Source         string
}

// Diff compares two dependency snapshots and returns deterministic
// added/removed/changed rows keyed by ecosystem, package, and manifest path.
// It deliberately ignores unchanged dependencies so pull-request reviews stay
// focused on what the PR changed.
func Diff(base, head Snapshot) []Change {
	baseByKey := make(map[string]Dependency, len(base.Dependencies))
	headByKey := make(map[string]Dependency, len(head.Dependencies))
	keys := make(map[string]struct{}, len(base.Dependencies)+len(head.Dependencies))
	for _, dep := range base.Dependencies {
		key := dependencyKey(dep)
		baseByKey[key] = dep
		keys[key] = struct{}{}
	}
	for _, dep := range head.Dependencies {
		key := dependencyKey(dep)
		headByKey[key] = dep
		keys[key] = struct{}{}
	}

	out := make([]Change, 0, len(keys))
	for key := range keys {
		oldDep, hadOld := baseByKey[key]
		newDep, hasNew := headByKey[key]
		switch {
		case !hadOld && hasNew:
			out = append(out, changeFromDependency(ChangeAdded, Dependency{}, newDep))
		case hadOld && !hasNew:
			out = append(out, changeFromDependency(ChangeRemoved, oldDep, Dependency{}))
		case hadOld && hasNew && oldDep.PackageVersion != newDep.PackageVersion:
			out = append(out, changeFromDependency(ChangeChanged, oldDep, newDep))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ecosystem != out[j].Ecosystem {
			return out[i].Ecosystem < out[j].Ecosystem
		}
		if !strings.EqualFold(out[i].PackageName, out[j].PackageName) {
			return strings.ToLower(out[i].PackageName) < strings.ToLower(out[j].PackageName)
		}
		if out[i].ManifestPath != out[j].ManifestPath {
			return out[i].ManifestPath < out[j].ManifestPath
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func changeFromDependency(kind ChangeKind, oldDep, newDep Dependency) Change {
	dep := newDep
	if kind == ChangeRemoved {
		dep = oldDep
	}
	return Change{
		Kind:           kind,
		Ecosystem:      dep.Ecosystem,
		PackageName:    dep.PackageName,
		ManifestPath:   dep.ManifestPath,
		LockfilePath:   dep.LockfilePath,
		OldVersion:     oldDep.PackageVersion,
		NewVersion:     newDep.PackageVersion,
		Scope:          dep.Scope,
		Direct:         dep.Direct,
		PackageManager: dep.PackageManager,
		Source:         dep.Source,
	}
}
