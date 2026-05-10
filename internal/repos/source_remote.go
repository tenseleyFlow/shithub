// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/security/ssrf"
)

const MaxSourceRemoteURLLen = 2048

var ErrInvalidSourceRemote = errors.New("repos: invalid source remote URL")

// NormalizeSourceRemoteURL validates and canonicalizes the public Git
// remote URL shithub is allowed to fetch from for source imports and
// submodule commit backfills. Credentials are deliberately not allowed
// here; private import credentials need a separate secret-backed design.
func NormalizeSourceRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > MaxSourceRemoteURLLen {
		return "", fmt.Errorf("%w: too long", ErrInvalidSourceRemote)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: malformed URL", ErrInvalidSourceRemote)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: source imports currently support http(s) git remotes", ErrInvalidSourceRemote)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%w: missing host", ErrInvalidSourceRemote)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: credentials are not supported in source remote URLs", ErrInvalidSourceRemote)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: query strings and fragments are not supported", ErrInvalidSourceRemote)
	}
	if strings.Trim(u.EscapedPath(), "/") == "" {
		return "", fmt.Errorf("%w: missing repository path", ErrInvalidSourceRemote)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// ValidateSourceRemoteURL runs the same SSRF defenses used for webhooks
// before a URL is persisted or fetched by git. Git still receives the URL
// as argv (never through a shell), and fetch disables submodule recursion.
func ValidateSourceRemoteURL(ctx context.Context, raw string) (string, error) {
	normalized, err := NormalizeSourceRemoteURL(raw)
	if err != nil || normalized == "" {
		return normalized, err
	}
	if err := ssrf.Default().ValidateWithResolve(ctx, normalized); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSourceRemote, err)
	}
	return normalized, nil
}
