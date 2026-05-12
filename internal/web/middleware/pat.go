// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

var patAuthKey = ctxKey{name: "pat_auth"}

// PATAuth carries the resolved-token state for downstream handlers. When
// the auth check passed via PAT, `Token != nil` and Scopes is the parsed
// scope list. Pure session callers see the zero value.
type PATAuth struct {
	UserID      int64
	Username    string
	TokenID     int64
	Scopes      []string
	IsSuspended bool
	IsSiteAdmin bool
}

// PATAuthFromContext returns the resolved PAT auth state, or the zero
// value when the request was authenticated some other way (or anonymous).
func PATAuthFromContext(ctx context.Context) PATAuth {
	if v, ok := ctx.Value(patAuthKey).(PATAuth); ok {
		return v
	}
	return PATAuth{}
}

// PolicyActor returns the canonical policy actor for a resolved PAT request.
func (p PATAuth) PolicyActor() policy.Actor {
	if p.UserID == 0 {
		return policy.AnonymousActor()
	}
	return policy.UserActor(p.UserID, p.Username, p.IsSuspended, p.IsSiteAdmin)
}

// PATConfig configures the PAT auth middleware.
type PATConfig struct {
	Pool      *pgxpool.Pool
	Debouncer *pat.Debouncer // optional; one is created if nil
	Logger    *slog.Logger
	// Realm is the WWW-Authenticate realm string written on 401.
	Realm string
}

// PATAuthMiddleware returns middleware that resolves a
// `Authorization: token ...`, `Authorization: Bearer ...`, or HTTP Basic
// credential into a populated PATAuth on the request context.
//
// Behavior:
//
//   - Missing Authorization header → next handler runs with an empty
//     PATAuth (the route may still allow session auth).
//   - Malformed credentials, unknown hash, revoked, or expired token →
//     401 with WWW-Authenticate set; chain stops here.
//   - On success → PATAuth populated on context, last-used updated
//     (debounced), chain proceeds.
func PATAuthMiddleware(cfg PATConfig) func(http.Handler) http.Handler {
	if cfg.Debouncer == nil {
		cfg.Debouncer = pat.NewDebouncer(60 * time.Second)
	}
	if cfg.Realm == "" {
		cfg.Realm = "shithub"
	}
	q := usersdb.New()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := extractPAT(r)
			if errors.Is(err, errNoCredentials) {
				next.ServeHTTP(w, r)
				return
			}
			if err != nil {
				writePATChallenge(w, cfg.Realm, "invalid token")
				return
			}

			hash, err := pat.HashOf(raw)
			if err != nil {
				writePATChallenge(w, cfg.Realm, "invalid token")
				return
			}

			row, err := q.GetUserTokenByHash(r.Context(), cfg.Pool, hash)
			if err != nil {
				// pgx.ErrNoRows or any other DB error: respond identically
				// so we don't leak hash existence via timing/messaging.
				writePATChallenge(w, cfg.Realm, "invalid token")
				return
			}
			if row.RevokedAt.Valid {
				writePATChallenge(w, cfg.Realm, "token revoked")
				return
			}
			if row.ExpiresAt.Valid && time.Now().After(row.ExpiresAt.Time) {
				writePATChallenge(w, cfg.Realm, "token expired")
				return
			}

			// Verify owner is not suspended / deleted. The cascade DELETE
			// on user removal handles deletion; suspension we check explicitly.
			user, err := q.GetUserByID(r.Context(), cfg.Pool, row.UserID)
			if err != nil {
				writePATChallenge(w, cfg.Realm, "invalid token")
				return
			}
			if user.SuspendedAt.Valid {
				writePATChallenge(w, cfg.Realm, "account suspended")
				return
			}

			// Debounced last-used update — never blocks the request.
			// G118: we INTENTIONALLY detach from r.Context() so the
			// update survives client disconnect (a debounced touch is
			// already a best-effort write; we'd rather complete it than
			// drop on cancel).
			if cfg.Debouncer.ShouldTouch(row.ID) {
				ip := remoteAddrFromRequest(r)
				rowID := row.ID
				go func() { //nolint:gosec
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
					defer cancel()
					if err := q.TouchUserTokenLastUsed(ctx, cfg.Pool, usersdb.TouchUserTokenLastUsedParams{
						ID:         rowID,
						LastUsedIp: ip,
					}); err != nil && cfg.Logger != nil {
						cfg.Logger.WarnContext(ctx, "pat: touch last_used", "error", err)
					}
				}()
			}

			// X-OAuth-Scopes echoes the token's scopes on every response —
			// even errors emitted further down the chain — so the CLI's
			// error path (shithub-cli/internal/api/errors.go) can report
			// provided scopes alongside the required scope on 403.
			w.Header().Set("X-OAuth-Scopes", strings.Join(row.Scopes, ", "))

			ctx := context.WithValue(r.Context(), patAuthKey, PATAuth{
				UserID:      row.UserID,
				Username:    user.Username,
				TokenID:     row.ID,
				Scopes:      row.Scopes,
				IsSuspended: user.SuspendedAt.Valid,
				IsSiteAdmin: user.IsSiteAdmin,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope rejects with 403 if the request was authenticated via PAT
// and the token's scopes don't include required. Pure-session callers
// (PATAuth zero) pass through — sessions have implicit full scope.
//
// The 403 response is the canonical /api/v1 JSON envelope
// `{"error": "token lacks required scope: <scope>"}` and carries
// X-Accepted-OAuth-Scopes so the CLI can format an actionable error
// without parsing the message body.
func RequireScope(required pat.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := PATAuthFromContext(r.Context())
			if auth.TokenID == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if !pat.HasScope(auth.Scopes, required) {
				w.Header().Set("X-Accepted-OAuth-Scopes", string(required))
				writeAPIJSONError(w, http.StatusForbidden, "token lacks required scope: "+string(required))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeAPIJSONError writes the canonical /api/v1 error envelope. Kept
// inline so package middleware doesn't depend on the api handler package
// (which would import-cycle).
func writeAPIJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// Escape the inner message just enough for embedding in a JSON
	// string literal. The reason strings here are package-controlled —
	// no user input — but defensive escaping keeps this honest if a
	// caller ever passes through external data.
	_, _ = w.Write([]byte(`{"error":` + jsonString(msg) + `}` + "\n"))
}

// jsonString returns s wrapped in JSON-string quoting, escaping the
// minimum required characters. Avoiding encoding/json here keeps
// allocations down on the hot challenge path.
func jsonString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				// Control chars: emit as \u00XX.
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[byte(r)>>4])
				b.WriteByte(hex[byte(r)&0x0f])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// errNoCredentials is the sentinel that says "no Authorization header at
// all" — distinct from "Authorization present but malformed."
var errNoCredentials = errors.New("middleware: no credentials")

// extractPAT parses the inbound credential into the raw token string.
// Supports: `Authorization: token <pat>`, `Authorization: Bearer <pat>`,
// and HTTP Basic where the password is the PAT (matches `git`'s
// credential-helper output, used by git-over-HTTPS in S12).
func extractPAT(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errNoCredentials
	}
	scheme, rest, ok := strings.Cut(auth, " ")
	if !ok {
		return "", errors.New("malformed Authorization header")
	}
	switch strings.ToLower(scheme) {
	case "token", "bearer":
		return strings.TrimSpace(rest), nil
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", err
		}
		_, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return "", errors.New("malformed Basic credentials")
		}
		return pass, nil
	default:
		return "", errors.New("unsupported auth scheme")
	}
}

// writePATChallenge writes the canonical 401 with a Bearer challenge.
// Body is the /api/v1 JSON error envelope so the shithub-cli client and
// any other JSON consumer can decode the failure uniformly.
func writePATChallenge(w http.ResponseWriter, realm, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`", error="invalid_token", error_description="`+reason+`"`)
	writeAPIJSONError(w, http.StatusUnauthorized, reason)
}

// remoteAddrFromRequest pulls the client IP for last_used_ip. Reuses the
// request's RealIP if set; otherwise falls back to RemoteAddr's host part.
func remoteAddrFromRequest(r *http.Request) *netip.Addr {
	candidates := []string{
		RealIPFromContext(r.Context(), r),
		r.RemoteAddr,
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		// Strip ":port" suffix if present — RemoteAddr always has one.
		host := c
		if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[:i], "]") {
			// IPv6 hosts come bracketed [::1]:1234 — those keep the bracket
			// and we'd want netip.ParseAddrPort instead.
			host = host[:i]
		}
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		if addr, err := netip.ParseAddr(host); err == nil {
			return &addr
		}
	}
	return nil
}
