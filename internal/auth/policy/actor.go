// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

// Actor is the authenticated identity asking for a decision. The web
// layer constructs one from middleware.CurrentUserFromContext + a
// suspended/admin check. SSH and HTTP git transports build their own
// from the resolved auth principal.
//
// An anonymous request has UserID == 0; IsAnonymous == true. Convention
// is that callers fill IsAnonymous explicitly even when UserID == 0
// implies it — duplication is cheap and keeps the boolean visible at
// every call site.
type Actor struct {
	UserID      int64
	Username    string
	IsAnonymous bool
	IsSuspended bool
	IsSiteAdmin bool
	// Impersonating reports whether this actor was constructed from
	// an admin viewing-as another user. ImpersonateWriteOK enables
	// the admin's writes during the impersonation; without it, every
	// write action gets denied (the canonical foot-gun guard).
	Impersonating      bool
	ImpersonateWriteOK bool
}

// AnonymousActor returns the canonical anonymous Actor. Use in tests
// and at unauthenticated entrypoints.
func AnonymousActor() Actor {
	return Actor{IsAnonymous: true}
}

// UserActor wraps a logged-in user. Suspension and site-admin flags
// must be loaded from the DB by the caller — the policy package does
// not query users on its own to keep the decision pure.
//
// New web-layer code should prefer UserActorFromCurrentUser, which
// also propagates the impersonation pair so admin viewing-as flows
// don't silently leak write permission. UserActor is preserved for
// callers that don't have a CurrentUser handy (SSH/HTTP git, worker
// jobs, tests).
func UserActor(userID int64, username string, suspended, siteAdmin bool) Actor {
	return Actor{
		UserID:      userID,
		Username:    username,
		IsSuspended: suspended,
		IsSiteAdmin: siteAdmin,
	}
}

// CurrentUserView is the minimal view of the request-bound user that
// UserActorFromCurrentUser needs. The web's middleware.CurrentUser
// satisfies this implicitly — keeping the type local avoids a
// policy → middleware import cycle.
type CurrentUserView struct {
	ID                 int64
	Username           string
	IsSuspended        bool
	IsSiteAdmin        bool
	ImpersonatedUserID int64
	RealActorID        int64
	ImpersonateWriteOK bool
}

// UserActorFromCurrentUser is the canonical web-layer constructor.
// Beyond UserActor, it propagates the impersonation pair so the
// "read-only by default" promise on policy.go's impersonation gate
// is actually enforced — and so audit rows can be tagged with the
// real actor ID when impersonating.
//
// Convention: every handler that can run for an impersonating admin
// should construct the actor through this constructor. Any callsite
// still on plain UserActor with IsSiteAdmin=false hardcoded is a
// silent permission leak (audit 2026-05-10 C1/C2).
func UserActorFromCurrentUser(v CurrentUserView) Actor {
	return Actor{
		UserID:             v.ID,
		Username:           v.Username,
		IsSuspended:        v.IsSuspended,
		IsSiteAdmin:        v.IsSiteAdmin,
		Impersonating:      v.ImpersonatedUserID != 0,
		ImpersonateWriteOK: v.ImpersonateWriteOK,
	}
}
