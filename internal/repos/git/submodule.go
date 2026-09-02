// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const maxGitmodulesBytes int64 = 256 * 1024

// Submodule is one [submodule "..."] entry from .gitmodules, keyed by path.
type Submodule struct {
	Name   string
	Path   string
	URL    string
	Branch string
}

// Submodules reads and parses <ref>:.gitmodules. Missing or non-blob files
// return an empty map so callers can render plain gitlink rows.
func Submodules(ctx context.Context, gitDir, ref string) (map[string]Submodule, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	kind, _, size, err := StatPath(ctx, gitDir, ref, ".gitmodules")
	if err != nil {
		if errors.Is(err, ErrPathNotFound) {
			return map[string]Submodule{}, nil
		}
		return nil, err
	}
	if kind != EntryBlob {
		return map[string]Submodule{}, nil
	}
	if size > maxGitmodulesBytes {
		return nil, fmt.Errorf("git: .gitmodules size %d exceeds %d: %w", size, maxGitmodulesBytes, ErrBlobTooLarge)
	}
	body, err := ReadBlobBytes(ctx, gitDir, ref, ".gitmodules", maxGitmodulesBytes)
	if err != nil {
		return nil, err
	}
	return ParseGitmodules(body), nil
}

// ParseGitmodules extracts submodule declarations from git-config-style text.
// It intentionally accepts the ordinary shapes git writes rather than trying
// to be a full git-config parser.
func ParseGitmodules(body []byte) map[string]Submodule {
	out := map[string]Submodule{}
	var current Submodule
	inSubmodule := false
	flush := func() {
		if !inSubmodule || current.Path == "" {
			return
		}
		out[current.Path] = current
	}

	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			current = Submodule{}
			inSubmodule = false
			if name, ok := submoduleSectionName(strings.TrimSpace(line[1 : len(line)-1])); ok {
				current.Name = name
				inSubmodule = true
			}
			continue
		}
		if !inSubmodule {
			continue
		}
		key, value, ok := gitmoduleKeyValue(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "path":
			current.Path = cleanGitmodulePath(value)
		case "url":
			current.URL = cleanGitmoduleValue(value)
		case "branch":
			current.Branch = cleanGitmoduleValue(value)
		}
	}
	flush()
	return out
}

func submoduleSectionName(section string) (string, bool) {
	lower := strings.ToLower(section)
	switch {
	case strings.HasPrefix(lower, "submodule "):
		name := strings.TrimSpace(section[len("submodule "):])
		if name == "" {
			return "", false
		}
		return cleanGitmoduleValue(name), true
	case strings.HasPrefix(lower, "submodule."):
		name := strings.TrimSpace(section[len("submodule."):])
		return name, name != ""
	default:
		return "", false
	}
}

func gitmoduleKeyValue(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, "=")
	if ok {
		key = strings.TrimSpace(key)
		if key == "" {
			return "", "", false
		}
		return key, strings.TrimSpace(value), true
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], strings.Join(parts[1:], " "), true
}

func cleanGitmodulePath(value string) string {
	value = cleanGitmoduleValue(value)
	if value == "" || path.IsAbs(value) {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

func cleanGitmoduleValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return strings.Trim(value, `"`)
}
