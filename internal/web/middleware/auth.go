// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"net/url"
)

var currentUserKey = ctxKey{name: "current_user"}

// CurrentUser carries the loaded user identity in request context. Handlers
// pull this out via CurrentUserFromContext. Anonymous requests have ID == 0.
//
// IsSuspended is sourced from users.suspended_at and is the canonical input
// to policy.UserActor for web requests. Without it, every handler that
// constructs an actor with a hard-coded `false` lets a suspended account
// keep writing — the audit found this gap (S00-S25 audit, finding C1).
type CurrentUser struct {
	ID          int64
	Username    string
	IsSuspended bool
}

// IsAnonymous reports whether this is an unauthenticated request.
func (u CurrentUser) IsAnonymous() bool { return u.ID == 0 }

// UserLookupResult is what UserLookup returns. It's a struct so future
// fields (e.g. is_admin once S34 lands) don't keep widening the signature
// and forcing every callsite to update.
type UserLookupResult struct {
	Username     string
	SessionEpoch int32
	IsSuspended  bool
}

// UserLookup resolves a user_id into the data the auth middleware needs.
// SessionEpoch is the users.session_epoch column; the middleware compares
// it to the session's recorded epoch on every request so "log out
// everywhere" (which bumps the column) invalidates stale cookies on the
// next hit.
type UserLookup func(ctx context.Context, userID int64) (UserLookupResult, error)

// OptionalUser populates CurrentUser into context from the loaded session
// when present. Does not redirect or reject — pages that don't need a
// user (homepage, public repo views) just ignore an empty CurrentUser.
//
// When lookup returns successfully and the user's current session_epoch
// does NOT match the session's recorded epoch, the binding is skipped so
// the request looks anonymous. The stale cookie itself isn't actively
// cleared — RequireUser will redirect the next protected hit to /login,
// at which point a fresh session is minted with the current epoch.
//
// Pass nil to skip the lookup entirely (CurrentUser.Username will be
// empty and epoch checks are bypassed).
func OptionalUser(lookup UserLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			s := SessionFromContext(ctx)
			if s.UserID != 0 {
				u := CurrentUser{ID: s.UserID}
				bind := true
				if lookup != nil {
					if res, err := lookup(ctx, s.UserID); err == nil {
						if res.SessionEpoch != s.Epoch {
							bind = false
						} else {
							u.Username = res.Username
							u.IsSuspended = res.IsSuspended
						}
					}
				}
				if bind {
					ctx = context.WithValue(ctx, currentUserKey, u)
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireUser is a stricter wrapper: redirects to /login (preserving the
// requested path in the `next` query param) when there is no user bound
// to the session. Pages that demand a user (settings, dashboard) compose
// this on top of OptionalUser.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := CurrentUserFromContext(r.Context())
		if u.IsAnonymous() {
			to := "/login"
			if r.Method == http.MethodGet {
				q := url.Values{"next": []string{r.URL.RequestURI()}}
				to = "/login?" + q.Encode()
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CurrentUserFromContext returns the user bound to ctx by OptionalUser /
// RequireUser. Returns the zero value (anonymous) when no user is bound.
func CurrentUserFromContext(ctx context.Context) CurrentUser {
	if u, ok := ctx.Value(currentUserKey).(CurrentUser); ok {
		return u
	}
	return CurrentUser{}
}

// MaxBodySize returns middleware that caps r.Body at maxBytes via
// http.MaxBytesReader. Use this on auth POST endpoints (signup, login,
// password reset) so a misbehaving client can't ship a 10 MB password
// to weaponize the argon2 hashing path.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}
