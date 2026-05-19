// SPDX-License-Identifier: AGPL-3.0-or-later

// Package advisorymatch evaluates local dependency advisory range expressions.
//
// The matcher is deliberately local and deterministic: push, pull-request, and
// request paths must not call external advisory services to decide whether a
// dependency is vulnerable.
package advisorymatch

import (
	"regexp"
	"strings"
)

// MatchVersion reports whether version is affected by rangeExpr for ecosystem.
// Empty and "*" ranges match every non-empty version, preserving the SP25 manual
// advisory behavior. Unsupported ecosystems fall back to normalized exact
// matching so existing local advisories keep working without overclaiming range
// support.
func MatchVersion(ecosystem, version, rangeExpr string) bool {
	version = strings.TrimSpace(version)
	rangeExpr = strings.TrimSpace(rangeExpr)
	if version == "" {
		return false
	}
	if rangeExpr == "" || rangeExpr == "*" {
		return true
	}
	if exactVersionEqual(version, rangeExpr) {
		return true
	}
	if !supportedEcosystem(ecosystem) {
		return false
	}
	v, ok := parseConcreteVersion(version)
	if !ok {
		return false
	}
	for _, alternative := range strings.Split(rangeExpr, "||") {
		if matchConjunction(v, strings.TrimSpace(alternative)) {
			return true
		}
	}
	return false
}

func supportedEcosystem(ecosystem string) bool {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "go", "gomod", "npm":
		return true
	default:
		return false
	}
}

func matchConjunction(v semanticVersion, expr string) bool {
	if expr == "" || expr == "*" {
		return true
	}
	if lower, upper, ok := parseHyphenRange(expr); ok {
		return compareVersions(v, lower) >= 0 && compareVersions(v, upper) <= 0
	}
	parts := splitRangeParts(expr)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		matcher, ok := parseComparator(part)
		if !ok || !matcher(v) {
			return false
		}
	}
	return true
}

func splitRangeParts(expr string) []string {
	expr = strings.ReplaceAll(expr, ",", " ")
	fields := strings.Fields(expr)
	parts := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		token := fields[i]
		if isComparatorOperator(token) && i+1 < len(fields) {
			parts = append(parts, token+fields[i+1])
			i++
			continue
		}
		parts = append(parts, token)
	}
	return parts
}

func isComparatorOperator(token string) bool {
	switch token {
	case "<", "<=", ">", ">=", "=", "==":
		return true
	default:
		return false
	}
}

type versionMatcher func(semanticVersion) bool

func parseComparator(expr string) (versionMatcher, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" || strings.EqualFold(expr, "x") {
		return func(semanticVersion) bool { return true }, true
	}
	if strings.HasPrefix(expr, "^") {
		return caretMatcher(strings.TrimSpace(strings.TrimPrefix(expr, "^")))
	}
	if strings.HasPrefix(expr, "~") {
		return tildeMatcher(strings.TrimSpace(strings.TrimPrefix(expr, "~")))
	}
	if lower, upper, ok := wildcardMatcher(expr); ok {
		return func(v semanticVersion) bool {
			return compareVersions(v, lower) >= 0 && compareVersions(v, upper) < 0
		}, true
	}

	for _, op := range []string{">=", "<=", ">", "<", "==", "="} {
		if strings.HasPrefix(expr, op) {
			want, ok := parsePartialVersion(strings.TrimSpace(strings.TrimPrefix(expr, op)))
			if !ok {
				return nil, false
			}
			return comparatorMatcher(op, want), true
		}
	}
	want, ok := parseConcreteVersion(expr)
	if !ok {
		return nil, false
	}
	return comparatorMatcher("=", want), true
}

func comparatorMatcher(op string, want semanticVersion) versionMatcher {
	return func(v semanticVersion) bool {
		cmp := compareVersions(v, want)
		switch op {
		case ">=":
			return cmp >= 0
		case "<=":
			return cmp <= 0
		case ">":
			return cmp > 0
		case "<":
			return cmp < 0
		case "=", "==":
			return cmp == 0
		default:
			return false
		}
	}
}

func caretMatcher(value string) (versionMatcher, bool) {
	base, ok := parsePartialVersion(value)
	if !ok {
		return nil, false
	}
	upper := base
	switch {
	case base.major > 0:
		upper.major++
		upper.minor, upper.patch, upper.pre = 0, 0, nil
	case base.minor > 0:
		upper.minor++
		upper.patch, upper.pre = 0, nil
	default:
		upper.patch++
		upper.pre = nil
	}
	return func(v semanticVersion) bool {
		return compareVersions(v, base) >= 0 && compareVersions(v, upper) < 0
	}, true
}

func tildeMatcher(value string) (versionMatcher, bool) {
	base, ok := parsePartialVersion(value)
	if !ok {
		return nil, false
	}
	upper := base
	if base.parts >= 2 {
		upper.minor++
		upper.patch, upper.pre = 0, nil
	} else {
		upper.major++
		upper.minor, upper.patch, upper.pre = 0, 0, nil
	}
	return func(v semanticVersion) bool {
		return compareVersions(v, base) >= 0 && compareVersions(v, upper) < 0
	}, true
}

func wildcardMatcher(expr string) (semanticVersion, semanticVersion, bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(expr), "v")
	raw = strings.ReplaceAll(raw, "*", "x")
	parts := strings.Split(raw, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semanticVersion{}, semanticVersion{}, false
	}
	nums := []int{0, 0, 0}
	wildcard := -1
	for i, part := range parts {
		if strings.EqualFold(part, "x") {
			wildcard = i
			break
		}
		n, ok := parseSmallInt(part)
		if !ok {
			return semanticVersion{}, semanticVersion{}, false
		}
		nums[i] = n
	}
	if wildcard < 0 {
		return semanticVersion{}, semanticVersion{}, false
	}
	lower := semanticVersion{major: nums[0], minor: nums[1], patch: nums[2], parts: 3}
	upper := lower
	switch wildcard {
	case 0:
		upper.major = lower.major + 1
		upper.minor, upper.patch = 0, 0
	case 1:
		upper.major = lower.major + 1
		upper.minor, upper.patch = 0, 0
	default:
		upper.minor = lower.minor + 1
		upper.patch = 0
	}
	return lower, upper, true
}

func parseHyphenRange(expr string) (semanticVersion, semanticVersion, bool) {
	parts := strings.Split(expr, " - ")
	if len(parts) != 2 {
		return semanticVersion{}, semanticVersion{}, false
	}
	lower, ok := parsePartialVersion(parts[0])
	if !ok {
		return semanticVersion{}, semanticVersion{}, false
	}
	upper, ok := parsePartialVersion(parts[1])
	if !ok {
		return semanticVersion{}, semanticVersion{}, false
	}
	return lower, upper, true
}

type semanticVersion struct {
	major int
	minor int
	patch int
	pre   []string
	parts int
}

var versionRe = regexp.MustCompile(`^v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseConcreteVersion(value string) (semanticVersion, bool) {
	v, ok := parsePartialVersion(value)
	if !ok || v.parts < 3 {
		return semanticVersion{}, false
	}
	return v, true
}

func parsePartialVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(value)
	matches := versionRe.FindStringSubmatch(value)
	if len(matches) == 0 {
		return semanticVersion{}, false
	}
	major, ok := parseSmallInt(matches[1])
	if !ok {
		return semanticVersion{}, false
	}
	out := semanticVersion{major: major, parts: 1}
	if matches[2] != "" {
		minor, ok := parseSmallInt(matches[2])
		if !ok {
			return semanticVersion{}, false
		}
		out.minor = minor
		out.parts = 2
	}
	if matches[3] != "" {
		patch, ok := parseSmallInt(matches[3])
		if !ok {
			return semanticVersion{}, false
		}
		out.patch = patch
		out.parts = 3
	}
	if matches[4] != "" {
		out.pre = strings.Split(matches[4], ".")
	}
	return out, true
}

func parseSmallInt(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	var n int
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, true
}

func compareVersions(a, b semanticVersion) int {
	switch {
	case a.major != b.major:
		return sign(a.major - b.major)
	case a.minor != b.minor:
		return sign(a.minor - b.minor)
	case a.patch != b.patch:
		return sign(a.patch - b.patch)
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	default:
		return comparePrerelease(a.pre, b.pre)
	}
}

func comparePrerelease(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		cmp := compareIdentifier(a[i], b[i])
		if cmp != 0 {
			return cmp
		}
	}
	return sign(len(a) - len(b))
}

func compareIdentifier(a, b string) int {
	aNum, bNum := isNumericIdentifier(a), isNumericIdentifier(b)
	switch {
	case aNum && bNum:
		return compareNumericString(a, b)
	case aNum:
		return -1
	case bNum:
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func isNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func compareNumericString(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	switch {
	case len(a) != len(b):
		return sign(len(a) - len(b))
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func exactVersionEqual(a, b string) bool {
	av, aok := parseConcreteVersion(a)
	bv, bok := parseConcreteVersion(b)
	if aok && bok {
		return compareVersions(av, bv) == 0
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
