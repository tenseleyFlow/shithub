// SPDX-License-Identifier: AGPL-3.0-or-later

// Package templates embeds the curated set of license, gitignore, and
// README templates offered by the repo-creation form. Both pickers
// expose only the keys returned by Licenses() / Gitignores() — anything
// else is rejected at the form layer with ErrUnknownLicense /
// ErrUnknownGitignore.
//
// License sources: SPDX canonical text via gitea's options/license set.
// Gitignore sources: gitea's options/gitignore set (originally
// github.com/github/gitignore, MIT/CC0).
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed licenses/*.txt
var licensesFS embed.FS

//go:embed gitignore/*.gitignore
var gitignoreFS embed.FS

// LicenseKey is the SPDX-style identifier surfaced in the picker. Order
// of keys returned by Licenses() is alphabetical so the form is stable.
type LicenseKey = string

// GitignoreKey is the language identifier surfaced in the picker.
type GitignoreKey = string

// Licenses returns the sorted list of available license keys.
func Licenses() []LicenseKey {
	return listKeys(licensesFS, "licenses", ".txt")
}

// Gitignores returns the sorted list of available gitignore keys.
func Gitignores() []GitignoreKey {
	return listKeys(gitignoreFS, "gitignore", ".gitignore")
}

func listKeys(efs embed.FS, dir, suffix string) []string {
	entries, err := fs.ReadDir(efs, dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		out = append(out, strings.TrimSuffix(name, suffix))
	}
	sort.Strings(out)
	return out
}

// HasLicense reports whether key is on the curated list.
func HasLicense(key LicenseKey) bool { return contains(Licenses(), key) }

// LicenseName returns the human-readable name for a curated SPDX key.
// G13 (F8): the repo response envelope's `license.name` was emitted
// as an empty string when the key was set, breaking gh-compat clients
// that displayed `license.name` in their UI. Names use the canonical
// SPDX titles (https://spdx.org/licenses/) so consumers can render or
// match against the same strings GitHub uses. Returns "" for keys not
// in the curated list — `repo create` validates against HasLicense
// first, so an empty Name here means the user picked something
// off-catalog through a path that bypassed the picker.
func LicenseName(key LicenseKey) string {
	switch key {
	case "AGPL-3.0":
		return "GNU Affero General Public License v3.0"
	case "Apache-2.0":
		return "Apache License 2.0"
	case "BSD-2-Clause":
		return `BSD 2-Clause "Simplified" License`
	case "BSD-3-Clause":
		return `BSD 3-Clause "New" or "Revised" License`
	case "CC0-1.0":
		return "Creative Commons Zero v1.0 Universal"
	case "GPL-3.0":
		return "GNU General Public License v3.0"
	case "ISC":
		return "ISC License"
	case "MIT":
		return "MIT License"
	case "MPL-2.0":
		return "Mozilla Public License 2.0"
	case "Unlicense":
		return "The Unlicense"
	}
	return ""
}

// HasGitignore reports whether key is on the curated list.
func HasGitignore(key GitignoreKey) bool { return contains(Gitignores(), key) }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// LicenseText returns the canonical license body with year and author
// substituted. The substitution handles the canonical placeholders used
// in the source templates: <year>, [year], {{ year }}, [yyyy], plus
// <copyright holders>, <owner>, [fullname], etc. — collapsing them to
// the supplied year + author.
//
// year is typically time.Now().Year(); pass an explicit value when you
// want deterministic output (tests).
func LicenseText(key LicenseKey, year int, author string) (string, error) {
	if !HasLicense(key) {
		return "", fmt.Errorf("templates: unknown license %q", key)
	}
	raw, err := licensesFS.ReadFile(path.Join("licenses", key+".txt"))
	if err != nil {
		return "", err
	}
	return substituteLicense(string(raw), year, author), nil
}

// GitignoreText returns the canonical .gitignore body for key.
func GitignoreText(key GitignoreKey) (string, error) {
	if !HasGitignore(key) {
		return "", fmt.Errorf("templates: unknown gitignore %q", key)
	}
	raw, err := gitignoreFS.ReadFile(path.Join("gitignore", key+".gitignore"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ReadmeText returns the minimal README body. The format is intentionally
// boring — the spec calls out "always exactly this — no fancy
// boilerplate." If description is empty, the second line is dropped.
func ReadmeText(name, description string) string {
	if description == "" {
		return "# " + name + "\n"
	}
	return "# " + name + "\n\n" + description + "\n"
}

// substituteLicense replaces the various year/author placeholders found
// in the gitea-derived templates with the supplied values. We aim for
// the most common canonical placeholders SPDX uses; anything missed
// stays in the output and is harmless (just less personalized).
func substituteLicense(body string, year int, author string) string {
	yearStr := fmt.Sprintf("%d", year)
	if year <= 0 {
		yearStr = fmt.Sprintf("%d", time.Now().Year())
	}
	pairs := []struct{ old, new string }{
		{"<year>", yearStr},
		{"[year]", yearStr},
		{"[yyyy]", yearStr},
		{"{{ year }}", yearStr},
		{"{year}", yearStr},
		{"<copyright holders>", author},
		{"<owner>", author},
		{"<name of author>", author},
		{"<author>", author},
		{"[fullname]", author},
		{"[name of copyright owner]", author},
		{"{{ fullname }}", author},
		{"{author}", author},
	}
	for _, p := range pairs {
		body = strings.ReplaceAll(body, p.old, p.new)
	}
	return body
}
