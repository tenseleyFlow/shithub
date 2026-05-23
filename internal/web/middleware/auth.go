// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
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
	ID                    int64
	Username              string
	IsSuspended           bool
	IsSiteAdmin           bool
	HasConfirmedTwoFactor bool
	// SwitchAccounts are other accounts that completed authentication in
	// this browser session and can be selected from the profile menu. They
	// are UI hints only; /account/switch re-checks the sealed session entry
	// and the target user's current session_epoch before binding.
	SwitchAccounts []SwitchAccount
	// ImpersonatedUserID, when non-zero, identifies the user this admin
	// is impersonating. ID/Username/IsSuspended above reflect the
	// IMPERSONATED user (so policy checks behave as that user); the
	// real admin's ID is preserved in RealActorID for audit rows.
	ImpersonatedUserID int64
	RealActorID        int64
	ImpersonateWriteOK bool
}

// SwitchAccount is the small identity projection needed by the global
// account-switcher menu.
type SwitchAccount struct {
	UserID      int64
	Username    string
	DisplayName string
}

// IsAnonymous reports whether this is an unauthenticated request.
func (u CurrentUser) IsAnonymous() bool { return u.ID == 0 }

// PolicyActor builds a policy.Actor that propagates suspension,
// site-admin, and the impersonation pair. Use this everywhere the
// web layer needs an actor — the alternative (plain
// policy.UserActor) silently drops impersonation/site-admin and
// re-introduces the C1/C2 leaks the SR2 sprint fixed.
func (u CurrentUser) PolicyActor() policy.Actor {
	return policy.UserActorFromCurrentUser(policy.CurrentUserView{
		ID:                    u.ID,
		Username:              u.Username,
		IsSuspended:           u.IsSuspended,
		IsSiteAdmin:           u.IsSiteAdmin,
		HasConfirmedTwoFactor: u.HasConfirmedTwoFactor,
		ImpersonatedUserID:    u.ImpersonatedUserID,
		RealActorID:           u.RealActorID,
		ImpersonateWriteOK:    u.ImpersonateWriteOK,
	})
}

// AuditActor returns the (actor_id, augmented meta) pair to record
// for an audit row. During an impersonated session, the real admin
// is the actor and the impersonated user_id is stashed in meta.
// Outside impersonation, returns u.ID and meta unchanged.
//
// Use this anywhere a handler builds an audit row so impersonation
// trails are uniform across admin and non-admin surfaces (SR2 H2).
//
// Most callers want the higher-level RecordAudit helper instead;
// AuditActor is exposed for the rare case where the recorder/db
// pair isn't readily threaded through.
func (u CurrentUser) AuditActor(meta map[string]any) (int64, map[string]any) {
	if u.RealActorID == 0 {
		return u.ID, meta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["impersonated_user_id"] = u.ImpersonatedUserID
	return u.RealActorID, meta
}

// UserLookupResult is what UserLookup returns. It's a struct so future
// fields (e.g. is_admin once S34 lands) don't keep widening the signature
// and forcing every callsite to update.
type UserLookupResult struct {
	Username              string
	SessionEpoch          int32
	IsSuspended           bool
	IsSiteAdmin           bool
	HasConfirmedTwoFactor bool
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
							u.IsSiteAdmin = res.IsSiteAdmin
							u.HasConfirmedTwoFactor = res.HasConfirmedTwoFactor
						}
					}
				}
				if bind && s.ImpersonatedUserID == 0 {
					u.SwitchAccounts = switchAccountsFromSession(s, s.UserID)
				}
				// Impersonation: when the session carries an
				// ImpersonatedUserID, swap the bound identity to the
				// target user (so policy checks render the target's
				// view), keeping the real admin's id around for audit.
				if bind && s.ImpersonatedUserID != 0 && u.IsSiteAdmin && lookup != nil {
					if res, err := lookup(ctx, s.ImpersonatedUserID); err == nil {
						u.RealActorID = u.ID
						u.ImpersonatedUserID = s.ImpersonatedUserID
						u.ID = s.ImpersonatedUserID
						u.Username = res.Username
						u.IsSuspended = res.IsSuspended
						u.IsSiteAdmin = false // never carry admin into impersonated identity
						u.HasConfirmedTwoFactor = res.HasConfirmedTwoFactor
						u.ImpersonateWriteOK = s.ImpersonateWriteOK
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

func switchAccountsFromSession(s *session.Session, currentID int64) []SwitchAccount {
	if s == nil || len(s.Accounts) == 0 {
		return nil
	}
	out := make([]SwitchAccount, 0, len(s.Accounts))
	for _, acct := range s.Accounts {
		if acct.UserID == 0 || acct.UserID == currentID || acct.Username == "" {
			continue
		}
		displayName := acct.DisplayName
		if displayName == "" {
			displayName = acct.Username
		}
		out = append(out, SwitchAccount{
			UserID:      acct.UserID,
			Username:    acct.Username,
			DisplayName: displayName,
		})
	}
	return out
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

// RequireSiteAdmin gates routes behind users.is_site_admin. Compose
// AFTER RequireUser so an anonymous request hits /login first; this
// middleware then guards the elevated surface.
//
// Non-admin viewers receive 404 (NOT 403) so the existence of /admin
// isn't disclosed by the response shape. The same pattern matches
// every other "shouldn't-know-it-exists" surface across the app.
//
// Impersonating admins lose their admin powers for the duration of
// the impersonation (CurrentUser.IsSiteAdmin is forced false in that
// path), so this gate transparently locks them out of /admin until
// they end the impersonation. Documented in docs/internal/admin.md.
func RequireSiteAdmin(notFound http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := CurrentUserFromContext(r.Context())
			if !u.IsSiteAdmin {
				if notFound != nil {
					notFound.ServeHTTP(w, r)
					return
				}
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CurrentUserFromContext returns the user bound to ctx by OptionalUser /
// RequireUser. Returns the zero value (anonymous) when no user is bound.
func CurrentUserFromContext(ctx context.Context) CurrentUser {
	if u, ok := ctx.Value(currentUserKey).(CurrentUser); ok {
		return u
	}
	return CurrentUser{}
}

// WithCurrentUserForTest binds u onto ctx the same way the OptionalUser
// middleware does. Test-only — production code paths reach a request
// through OptionalUser/RequireUser and never need this. Lives here so
// the context key stays unexported.
func WithCurrentUserForTest(ctx context.Context, u CurrentUser) context.Context {
	return context.WithValue(ctx, currentUserKey, u)
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
