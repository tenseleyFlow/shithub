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
type CurrentUser struct {
	ID       int64
	Username string
}

// IsAnonymous reports whether this is an unauthenticated request.
func (u CurrentUser) IsAnonymous() bool { return u.ID == 0 }

// OptionalUser populates CurrentUser into context from the loaded session
// when present. Does not redirect or reject — pages that don't need a
// user (homepage, public repo views) just ignore an empty CurrentUser.
//
// Lookup is the function that resolves a user_id to a username. Pass nil
// to skip the lookup (CurrentUser.Username will be empty); handlers that
// need the username supply Lookup.
func OptionalUser(lookup func(ctx context.Context, userID int64) (string, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			s := SessionFromContext(ctx)
			if s.UserID != 0 {
				u := CurrentUser{ID: s.UserID}
				if lookup != nil {
					if name, err := lookup(ctx, s.UserID); err == nil {
						u.Username = name
					}
				}
				ctx = context.WithValue(ctx, currentUserKey, u)
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
