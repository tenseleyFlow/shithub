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
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

var patAuthKey = ctxKey{name: "pat_auth"}

// PATAuth carries the resolved-token state for downstream handlers. When
// the auth check passed via PAT, `Token != nil` and Scopes is the parsed
// scope list. Pure session callers see the zero value.
//
// PRO-EXT01-11b: RepoBinding is the repo this token is locked to (or 0
// for "no binding"). When non-zero, downstream repo-scoped routes MUST
// call pat.RepoBindingAllows to verify the request's resolved repo
// matches before serving data. Non-repo routes (e.g. /api/v1/user) are
// not affected — the binding limits which repo the token can act on,
// not whether the token can authenticate at all.
//
// **Contract for new handlers:** any code path that resolves to a repo
// (via owner/name URL params, run id → repo, job id → repo, etc.) must
// gate on patBindingAllowsRepo before returning data. The
// lookupRepoByLogin helper in handlers/api/repos.go already enforces
// this; ad-hoc lookups via GetRepoByID / GetRepoByOwnerUserAndName /
// GetRepoByOwnerOrgAndName must add the check themselves. PRO-EXT_SR2-10
// (audit C2) hardened stars.go, actions_rerun.go, actions_cancel.go
// after the auditor found stars.go bypassing — keep the pattern.
type PATAuth struct {
	UserID      int64
	Username    string
	TokenID     int64
	Scopes      []string
	RepoBinding int64
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

			// PRO-EXT01-11a: IP allowlist enforcement.
			//
			// Semantics:
			//   - Empty allowlist → no restriction (Free + un-restricted Pro tokens).
			//   - Non-empty allowlist → request IP must match a CIDR.
			//
			// We ALWAYS honor an existing allowlist regardless of the
			// PRO07 enforce flag — the flag gates the *write* path
			// (whether a user can attach an allowlist), not whether
			// we honor one the user already attached. Once intent is
			// expressed, denying access to an IP outside the allowlist
			// is the security contract.
			if len(row.IpAllowlist) > 0 {
				clientIP := remoteAddrFromRequest(r)
				if clientIP == nil || !pat.IPMatch(row.IpAllowlist, *clientIP) {
					if cfg.Logger != nil {
						clientIPStr := "unknown"
						if clientIP != nil {
							clientIPStr = clientIP.String()
						}
						cfg.Logger.WarnContext(r.Context(), "pat: ip_allowlist deny",
							"token_id", row.ID,
							"user_id", row.UserID,
							"client_ip", clientIPStr)
					}
					writePATChallenge(w, cfg.Realm, "token not authorized from this address")
					return
				}
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

			var repoBinding int64
			if row.RepoID.Valid {
				repoBinding = row.RepoID.Int64
			}
			ctx := context.WithValue(r.Context(), patAuthKey, PATAuth{
				UserID:      row.UserID,
				Username:    user.Username,
				TokenID:     row.ID,
				Scopes:      row.Scopes,
				RepoBinding: repoBinding,
				IsSuspended: user.SuspendedAt.Valid,
				IsSiteAdmin: user.IsSiteAdmin,
			})

			// PRO-EXT01-11c: record this request for the per-token
			// analytics view. We wrap the writer so we can capture the
			// downstream status, then fire-and-forget the insert from a
			// detached goroutine after the handler returns.
			recorder := &patUsageRecorder{ResponseWriter: w, status: http.StatusOK}
			tokenID := row.ID
			method := r.Method
			path := r.URL.Path
			next.ServeHTTP(recorder, r.WithContext(ctx))
			recordPATUsage(q, cfg, tokenID, method, path, recorder.status)
		})
	}
}

// patUsageRecorder is the minimal ResponseWriter wrapper that captures
// the first WriteHeader call so the analytics insert can record the
// outbound status code. It's intentionally not a hijacker/flusher
// pass-through — the PAT middleware only fronts /api/v1/* + git-over-
// HTTPS, neither of which hijack.
type patUsageRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (p *patUsageRecorder) WriteHeader(code int) {
	if !p.wroteHeader {
		p.status = code
		p.wroteHeader = true
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p *patUsageRecorder) Write(b []byte) (int, error) {
	if !p.wroteHeader {
		// Implicit 200 if handler wrote without WriteHeader first.
		p.wroteHeader = true
	}
	return p.ResponseWriter.Write(b)
}

// recordPATUsage inserts one usage event for this request. Best-effort:
// the goroutine has its own short timeout and any error is logged at
// warn level, never surfaced to the user.
//
// PRO-EXT_SR2-13 (audit Q7): every outcome (ok / timeout / error) bumps
// shithub_pat_usage_events_total{outcome=...} so operators can spot a
// silent drop rate before it shows up as confused users staring at
// empty per-token analytics charts.
func recordPATUsage(q usersdb.Querier, cfg PATConfig, tokenID int64, method, path string, status int) {
	if cfg.Pool == nil {
		return
	}
	prefix := pat.RoutePrefix(path)
	go func() { //nolint:gosec
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		err := q.InsertUserTokenUsageEvent(ctx, cfg.Pool, usersdb.InsertUserTokenUsageEventParams{
			TokenID:     tokenID,
			Method:      method,
			RoutePrefix: prefix,
			StatusCode:  int16(status),
		})
		switch {
		case err == nil:
			metrics.PATUsageEventsTotal.WithLabelValues("ok").Inc()
		case errors.Is(err, context.DeadlineExceeded):
			metrics.PATUsageEventsTotal.WithLabelValues("timeout").Inc()
			if cfg.Logger != nil {
				cfg.Logger.WarnContext(ctx, "pat: insert usage event", "error", err)
			}
		default:
			metrics.PATUsageEventsTotal.WithLabelValues("error").Inc()
			if cfg.Logger != nil {
				cfg.Logger.WarnContext(ctx, "pat: insert usage event", "error", err)
			}
		}
	}()
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
