// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	customPatternPrefix     = "custom/"
	CustomPatternNameMaxLen = 60
	CustomPatternExprMaxLen = 1000
	CustomPatternDescMaxLen = 500
	CustomPatternMinMatch   = 8
	CustomPatternMaxMatch   = 256
)

var (
	ErrCustomPatternNameRequired       = errors.New("custom secret pattern name is required")
	ErrCustomPatternNameInvalid        = errors.New("custom secret pattern name must start and end with a letter or number and contain only letters, numbers, dots, dashes, or underscores")
	ErrCustomPatternNameTooLong        = errors.New("custom secret pattern name must be 60 characters or fewer")
	ErrCustomPatternNameReserved       = errors.New("custom secret pattern name is reserved")
	ErrCustomPatternDescriptionTooLong = errors.New("custom secret pattern description must be 500 characters or fewer")
	ErrCustomPatternExpressionRequired = errors.New("custom secret pattern expression is required")
	ErrCustomPatternExpressionTooLong  = errors.New("custom secret pattern expression must be 1000 characters or fewer")
	ErrCustomPatternExpressionInvalid  = errors.New("custom secret pattern expression is invalid")
	ErrCustomPatternMatchesEmpty       = errors.New("custom secret pattern expression must not match an empty string")
	ErrCustomPatternMinMatchInvalid    = errors.New("custom secret pattern minimum match length must be between 8 and 256")
)

var customPatternNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,58}[A-Za-z0-9])?$`)

// CustomPatternSpec is the persisted shape needed to compile an
// organization-defined detector. The raw expression is configuration,
// not a secret; matched bytes are still redacted by Scan.
type CustomPatternSpec struct {
	Name        string
	Description string
	Pattern     string
	MinMatchLen int
}

// CustomPatternFindingName returns the stable finding/allowlist label
// for an organization-defined pattern.
func CustomPatternFindingName(name string) string {
	return customPatternPrefix + strings.TrimSpace(name)
}

// CompileCustomPattern validates and compiles one org-defined secret
// pattern into the same Pattern type used by built-in detectors.
func CompileCustomPattern(spec CustomPatternSpec) (Pattern, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return Pattern{}, ErrCustomPatternNameRequired
	}
	if utf8.RuneCountInString(name) > CustomPatternNameMaxLen {
		return Pattern{}, ErrCustomPatternNameTooLong
	}
	if !customPatternNameRe.MatchString(name) {
		return Pattern{}, ErrCustomPatternNameInvalid
	}
	if strings.HasPrefix(strings.ToLower(name), customPatternPrefix) || builtinPatternName(name) {
		return Pattern{}, ErrCustomPatternNameReserved
	}

	desc := strings.TrimSpace(spec.Description)
	if utf8.RuneCountInString(desc) > CustomPatternDescMaxLen {
		return Pattern{}, ErrCustomPatternDescriptionTooLong
	}
	expr := strings.TrimSpace(spec.Pattern)
	if expr == "" {
		return Pattern{}, ErrCustomPatternExpressionRequired
	}
	if utf8.RuneCountInString(expr) > CustomPatternExprMaxLen {
		return Pattern{}, ErrCustomPatternExpressionTooLong
	}
	minLen := spec.MinMatchLen
	if minLen == 0 {
		minLen = CustomPatternMinMatch
	}
	if minLen < CustomPatternMinMatch || minLen > CustomPatternMaxMatch {
		return Pattern{}, ErrCustomPatternMinMatchInvalid
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Pattern{}, fmt.Errorf("%w: %v", ErrCustomPatternExpressionInvalid, err)
	}
	if re.MatchString("") {
		return Pattern{}, ErrCustomPatternMatchesEmpty
	}

	return Pattern{
		Name:        CustomPatternFindingName(name),
		Description: desc,
		Re:          re,
		MinMatchLen: minLen,
	}, nil
}

// CompileCustomPatterns validates and compiles a list. The first bad
// row returns an error; persisted rows should already have been
// validated on write, so failing closed here keeps broken
// configuration out of scans.
func CompileCustomPatterns(specs []CustomPatternSpec) ([]Pattern, error) {
	out := make([]Pattern, 0, len(specs))
	for _, spec := range specs {
		p, err := CompileCustomPattern(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// PatternsWithCustom appends custom detectors to the built-in set
// without mutating the package-level Patterns slice.
func PatternsWithCustom(custom []Pattern) []Pattern {
	if len(custom) == 0 {
		return Patterns
	}
	out := make([]Pattern, 0, len(Patterns)+len(custom))
	out = append(out, Patterns...)
	out = append(out, custom...)
	return out
}

func builtinPatternName(name string) bool {
	for _, p := range Patterns {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}
