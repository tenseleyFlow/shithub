// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxNameLen / MaxDescriptionLen mirror the DB CHECK constraints in
// migration 0017. Validate before insert so we surface a friendly error
// rather than a 500 from the constraint.
const (
	MaxNameLen        = 100
	MaxDescriptionLen = 350
)

// nameRE matches the name shape we accept: lowercase letters, digits,
// dots, hyphens, underscores. Edges can't be a separator. Length is
// bounded separately to give a more specific error message.
var nameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9_])?$`)

// reservedRepoNames is the set of names a repo may NOT take. Most are
// special filenames git itself uses (or that would break our routing).
// The list is kept short on purpose; it's all-lowercase because we
// always lowercase the name before comparing.
var reservedRepoNames = map[string]struct{}{
	".git":           {},
	".gitignore":     {},
	".gitmodules":    {},
	".gitattributes": {},
	".well-known":    {},
	".github":        {},
	"head":           {},
	"refs":           {},
	"objects":        {},
	"info":           {},
	"hooks":          {},
	"branches":       {},
}

// IsReservedRepoName reports whether name (case-insensitive) is on the
// reserved list. Callers MUST also reject leading dots and the empty
// string via ValidateName.
func IsReservedRepoName(name string) bool {
	_, ok := reservedRepoNames[strings.ToLower(name)]
	return ok
}

// NormalizeName lowercases and trims. Repos are case-preserved in the
// DB (citext does case-insensitive uniqueness), but disk paths are
// lowercased. We lowercase up-front so the same string drives both.
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ValidateName enforces the shape whitelist. Returns ErrInvalidName /
// ErrReservedName wrapped with a precise reason on failure.
//
// Caller is expected to have lowercased the name first; uppercase input
// is rejected as a defensive check in case a future caller forgets.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return fmt.Errorf("%w: too long (max %d)", ErrInvalidName, MaxNameLen)
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("%w: must be lowercase", ErrInvalidName)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: contains dot-dot", ErrInvalidName)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: starts with dot", ErrInvalidName)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("%w: ends with separator", ErrInvalidName)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: must match [a-z0-9._-] with non-separator edges", ErrInvalidName)
	}
	if IsReservedRepoName(name) {
		return fmt.Errorf("%w: %q", ErrReservedName, name)
	}
	return nil
}

// ValidateDescription enforces the DB CHECK length cap.
func ValidateDescription(desc string) error {
	if utf8.RuneCountInString(desc) > MaxDescriptionLen {
		return fmt.Errorf("%w: max %d characters", ErrDescriptionTooLong, MaxDescriptionLen)
	}
	return nil
}
