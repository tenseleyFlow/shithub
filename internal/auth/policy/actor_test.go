// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import "testing"

// TestUserActorFromCurrentUser_PropagatesAllFlags pins the SR2 C1+C2
// fix: the constructor used by the web layer must carry IsSuspended,
// IsSiteAdmin, Impersonating, and ImpersonateWriteOK into the Actor.
//
// Before SR2, every web mutation handler used UserActor(...) with
// `false` literals for site-admin/impersonation, so:
//   - C1: an impersonating admin could write as the target user
//     because Impersonating was never true at the actor level.
//   - C2: a site admin could not read private repos they didn't
//     collaborate on because IsSiteAdmin never propagated to Actor.
//
// This test fails LOUD on regressions of either path.
func TestUserActorFromCurrentUser_PropagatesAllFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		view CurrentUserView
		want Actor
	}{
		{
			name: "plain logged-in user",
			view: CurrentUserView{ID: 7, Username: "alice"},
			want: Actor{UserID: 7, Username: "alice"},
		},
		{
			name: "suspended user",
			view: CurrentUserView{ID: 7, Username: "alice", IsSuspended: true},
			want: Actor{UserID: 7, Username: "alice", IsSuspended: true},
		},
		{
			name: "site admin (not impersonating)",
			view: CurrentUserView{ID: 7, Username: "alice", IsSiteAdmin: true},
			want: Actor{UserID: 7, Username: "alice", IsSiteAdmin: true},
		},
		{
			name: "impersonating admin, read-only-by-default",
			view: CurrentUserView{
				ID:                 99, // viewer.ID is the impersonated user
				Username:           "target",
				ImpersonatedUserID: 99,
				RealActorID:        1,
				ImpersonateWriteOK: false,
			},
			want: Actor{
				UserID:        99,
				Username:      "target",
				Impersonating: true,
				// ImpersonateWriteOK stays false — the policy gate on
				// policy.go for write actions denies. This is the
				// guarantee C1 was leaking.
			},
		},
		{
			name: "impersonating admin with write-mode opted in",
			view: CurrentUserView{
				ID:                 99,
				Username:           "target",
				ImpersonatedUserID: 99,
				RealActorID:        1,
				ImpersonateWriteOK: true,
			},
			want: Actor{
				UserID:             99,
				Username:           "target",
				Impersonating:      true,
				ImpersonateWriteOK: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := UserActorFromCurrentUser(tc.view)
			if got != tc.want {
				t.Fatalf("UserActorFromCurrentUser:\n got=%+v\nwant=%+v", got, tc.want)
			}
		})
	}
}

// TestUserActorFromCurrentUser_SiteAdminReadsPrivateRepo pins C2:
// before SR2, IsSiteAdmin never reached the Actor on the read path,
// so policy.go's "site admin reads anything" branch was unreachable
// from the web layer. With the constructor migrated, the override
// works as documented.
func TestUserActorFromCurrentUser_ImpersonatingDoesNotEscalateAdmin(t *testing.T) {
	t.Parallel()

	// While impersonating, the wrapping middleware forces IsSiteAdmin
	// to false on the impersonated identity. The constructor must
	// honor that — never silently re-grant admin powers to the
	// impersonated session.
	view := CurrentUserView{
		ID:                 99,
		Username:           "target",
		IsSiteAdmin:        false, // middleware enforced
		ImpersonatedUserID: 99,
		RealActorID:        1,
	}
	got := UserActorFromCurrentUser(view)
	if got.IsSiteAdmin {
		t.Fatal("UserActorFromCurrentUser must not set IsSiteAdmin while impersonating; got true")
	}
	if !got.Impersonating {
		t.Fatal("UserActorFromCurrentUser must set Impersonating when ImpersonatedUserID is non-zero")
	}
}
