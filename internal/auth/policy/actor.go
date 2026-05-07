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
}

// AnonymousActor returns the canonical anonymous Actor. Use in tests
// and at unauthenticated entrypoints.
func AnonymousActor() Actor {
	return Actor{IsAnonymous: true}
}

// UserActor wraps a logged-in user. Suspension and site-admin flags
// must be loaded from the DB by the caller — the policy package does
// not query users on its own to keep the decision pure.
func UserActor(userID int64, username string, suspended, siteAdmin bool) Actor {
	return Actor{
		UserID:      userID,
		Username:    username,
		IsSuspended: suspended,
		IsSiteAdmin: siteAdmin,
	}
}
