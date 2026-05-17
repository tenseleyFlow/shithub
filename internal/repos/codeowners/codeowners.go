// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codeowners parses and matches GitHub-compatible CODEOWNERS files.
package codeowners

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

const MaxFileBytes int64 = 3 * 1024 * 1024

var Locations = []string{
	".github/CODEOWNERS",
	"CODEOWNERS",
	"docs/CODEOWNERS",
}

type OwnerKind string

const (
	OwnerUser  OwnerKind = "user"
	OwnerTeam  OwnerKind = "team"
	OwnerEmail OwnerKind = "email"
)

type Owner struct {
	Kind     OwnerKind
	Raw      string
	Username string
	Org      string
	Team     string
	Email    string
}

type Entry struct {
	Line    int
	Pattern string
	Owners  []Owner
	re      *regexp.Regexp
}

type ParseError struct {
	Line    int
	Message string
}

type File struct {
	Path    string
	Entries []Entry
	Errors  []ParseError
}

func Load(ctx context.Context, gitDir, ref string) (File, bool, error) {
	for _, loc := range Locations {
		kind, _, _, err := repogit.StatPath(ctx, gitDir, ref, loc)
		if err != nil {
			if errors.Is(err, repogit.ErrPathNotFound) || errors.Is(err, repogit.ErrNotATree) {
				continue
			}
			return File{}, false, err
		}
		if kind != repogit.EntryBlob {
			continue
		}
		body, err := repogit.ReadBlobBytes(ctx, gitDir, ref, loc, MaxFileBytes)
		if err != nil {
			return File{}, false, err
		}
		return Parse(loc, body), true, nil
	}
	return File{}, false, nil
}

func Parse(filePath string, body []byte) File {
	out := File{Path: filePath}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		tokens := tokenize(line)
		if len(tokens) == 0 {
			continue
		}
		pattern := tokens[0]
		if err := validatePattern(pattern); err != nil {
			out.Errors = append(out.Errors, ParseError{Line: lineNo, Message: err.Error()})
			continue
		}
		owners := make([]Owner, 0, len(tokens)-1)
		ownerTokens := tokens[1:]
		for _, tok := range ownerTokens {
			owner, ok := parseOwner(tok)
			if !ok {
				out.Errors = append(out.Errors, ParseError{Line: lineNo, Message: "invalid owner " + tok})
				continue
			}
			owners = append(owners, owner)
		}
		if len(ownerTokens) > 0 && len(owners) == 0 {
			continue
		}
		re, err := compilePattern(pattern)
		if err != nil {
			out.Errors = append(out.Errors, ParseError{Line: lineNo, Message: err.Error()})
			continue
		}
		out.Entries = append(out.Entries, Entry{
			Line:    lineNo,
			Pattern: pattern,
			Owners:  owners,
			re:      re,
		})
	}
	return out
}

func (f File) OwnersFor(filePath string) (Entry, bool) {
	target := normalizePath(filePath)
	var (
		last Entry
		ok   bool
	)
	for _, entry := range f.Entries {
		if entry.re == nil {
			continue
		}
		if entry.re.MatchString(target) {
			last = entry
			ok = true
		}
	}
	return last, ok
}

func tokenize(line string) []string {
	fields := []string{}
	var b strings.Builder
	inField := false
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			b.WriteRune(r)
			inField = true
			escaped = false
		case r == '\\':
			escaped = true
			inField = true
		case r == ' ' || r == '\t':
			if inField {
				fields = append(fields, b.String())
				b.Reset()
				inField = false
			}
		case r == '#' && !inField:
			return fields
		default:
			b.WriteRune(r)
			inField = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if inField {
		fields = append(fields, b.String())
	}
	return fields
}

func validatePattern(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("empty pattern")
	case strings.HasPrefix(pattern, "!"):
		return fmt.Errorf("negation patterns are not supported")
	case strings.HasPrefix(pattern, "#"):
		return fmt.Errorf("comment patterns are not supported")
	case strings.ContainsAny(pattern, "[]"):
		return fmt.Errorf("character ranges are not supported")
	default:
		return nil
	}
}

func parseOwner(raw string) (Owner, bool) {
	if raw == "" {
		return Owner{}, false
	}
	if strings.HasPrefix(raw, "@") {
		body := strings.TrimPrefix(raw, "@")
		if body == "" {
			return Owner{}, false
		}
		if strings.Contains(body, "/") {
			org, team, ok := strings.Cut(body, "/")
			if !ok || org == "" || team == "" || strings.Contains(team, "/") {
				return Owner{}, false
			}
			return Owner{Kind: OwnerTeam, Raw: raw, Org: strings.ToLower(org), Team: strings.ToLower(team)}, true
		}
		return Owner{Kind: OwnerUser, Raw: raw, Username: strings.ToLower(body)}, true
	}
	if strings.Contains(raw, "@") {
		return Owner{Kind: OwnerEmail, Raw: raw, Email: strings.ToLower(raw)}, true
	}
	return Owner{}, false
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	p := normalizePattern(pattern)
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	anchored := strings.HasPrefix(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	var prefix string
	if !anchored {
		if !strings.Contains(p, "/") {
			prefix = `(?:^|.*/)`
		} else {
			prefix = `(?:^|.*/)?`
		}
	} else {
		prefix = `^`
	}
	body := globRegex(p)
	if dirOnly {
		return regexp.Compile(prefix + body + `(?:/.*)?$`)
	}
	return regexp.Compile(prefix + body + `$`)
}

func globRegex(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		if ch == '*' {
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(`.*`)
				i++
				continue
			}
			b.WriteString(`[^/]*`)
			continue
		}
		if ch == '?' {
			b.WriteString(`[^/]`)
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(ch)))
	}
	return b.String()
}

func normalizePattern(pattern string) string {
	p := strings.ReplaceAll(pattern, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "/" + strings.Trim(path.Clean(strings.TrimPrefix(p, "/")), "/") + suffixSlash(p)
	}
	return strings.Trim(path.Clean(p), "/") + suffixSlash(p)
}

func normalizePath(filePath string) string {
	return strings.Trim(path.Clean(strings.ReplaceAll(filePath, "\\", "/")), "/")
}

func suffixSlash(s string) string {
	if strings.HasSuffix(s, "/") {
		return "/"
	}
	return ""
}
