// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// The matrix is built from three orthogonal axes: actor archetype,
// resource shape, and Action. We enumerate every combination and
// assert the verdict so a future refactor that breaks one cell fails
// loudly.

type actorKind int

const (
	actorAnonymous actorKind = iota
	actorOwner
	actorCollabRead
	actorCollabTriage
	actorCollabWrite
	actorCollabMaintain
	actorCollabAdmin
	actorUnrelated
	actorSuspendedOwner
	actorSuspendedCollabWrite
	actorSiteAdmin
)

func (k actorKind) String() string {
	return [...]string{
		"anonymous", "owner",
		"collab-read", "collab-triage", "collab-write", "collab-maintain", "collab-admin",
		"unrelated", "suspended-owner", "suspended-collab-write", "site-admin",
	}[k]
}

type repoKind int

const (
	repoPublic repoKind = iota
	repoPrivate
	repoArchivedPublic
	repoArchivedPrivate
	repoDeletedPublic
)

func (k repoKind) String() string {
	return [...]string{"public", "private", "archived-public", "archived-private", "deleted-public"}[k]
}

const (
	ownerID int64 = 100
	otherID int64 = 200
	repoIDV int64 = 1000
)

func makeActor(k actorKind) policy.Actor {
	switch k {
	case actorAnonymous:
		return policy.AnonymousActor()
	case actorOwner:
		return policy.UserActor(ownerID, "owner", false, false)
	case actorCollabRead, actorCollabTriage, actorCollabWrite, actorCollabMaintain, actorCollabAdmin:
		return policy.UserActor(otherID, "collab", false, false)
	case actorUnrelated:
		return policy.UserActor(otherID+1, "stranger", false, false)
	case actorSuspendedOwner:
		return policy.UserActor(ownerID, "owner", true, false)
	case actorSuspendedCollabWrite:
		return policy.UserActor(otherID, "collab", true, false)
	case actorSiteAdmin:
		return policy.UserActor(otherID+2, "admin", false, true)
	}
	return policy.AnonymousActor()
}

func makeRepo(k repoKind) policy.RepoRef {
	r := policy.RepoRef{ID: repoIDV, OwnerUserID: ownerID}
	switch k {
	case repoPublic:
		r.Visibility = "public"
	case repoPrivate:
		r.Visibility = "private"
	case repoArchivedPublic:
		r.Visibility = "public"
		r.IsArchived = true
	case repoArchivedPrivate:
		r.Visibility = "private"
		r.IsArchived = true
	case repoDeletedPublic:
		r.Visibility = "public"
		r.IsDeleted = true
	}
	return r
}

// fakeRoleResolver intercepts the cache layer so tests can assert role
// outcomes without hitting Postgres. Each collab actorKind seeds a
// specific role in the cache pre-Can.
func ctxWithCollabRole(t *testing.T, kind actorKind) context.Context {
	t.Helper()
	ctx := policy.WithCache(context.Background())
	role := map[actorKind]policy.Role{
		actorCollabRead:           policy.RoleRead,
		actorCollabTriage:         policy.RoleTriage,
		actorCollabWrite:          policy.RoleWrite,
		actorCollabMaintain:       policy.RoleMaintain,
		actorCollabAdmin:          policy.RoleAdmin,
		actorSuspendedCollabWrite: policy.RoleWrite,
	}[kind]
	if role != policy.RoleNone {
		policy.PrimeCacheForTest(ctx, otherID, repoIDV, role)
	}
	return ctx
}

// expect computes the canonical verdict for a (actorKind, repoKind, Action)
// triple. The function below is the policy spec restated in plain Go;
// if Can() and expect() ever disagree, one of them has a bug.
//
//nolint:gocyclo // exhaustive shape is by design.
func expect(actor actorKind, repo repoKind, action policy.Action) bool {
	// Deleted repo → nothing.
	if repo == repoDeletedPublic {
		return false
	}
	isWrite := action != policy.ActionRepoRead && action != policy.ActionIssueRead && action != policy.ActionPullRead

	// Site admin can read everything (except deleted handled above).
	if actor == actorSiteAdmin && !isWrite {
		return true
	}

	// Suspended actors: writes always blocked. Reads against public repos
	// still allowed (matches the suspended-then-read public code path).
	if actor == actorSuspendedOwner || actor == actorSuspendedCollabWrite {
		if isWrite {
			return false
		}
		// fall through to the visibility check
	}

	isPrivate := repo == repoPrivate || repo == repoArchivedPrivate
	isArchived := repo == repoArchivedPublic || repo == repoArchivedPrivate

	// Anonymous + private → deny.
	if actor == actorAnonymous && isPrivate {
		return false
	}

	// Public repo reads: anyone.
	if !isWrite && !isPrivate {
		return true
	}

	// Public issue participation: any logged-in user can open/comment
	// on issues in a non-archived public repo.
	if (action == policy.ActionIssueCreate || action == policy.ActionIssueComment) && !isPrivate {
		if actor == actorAnonymous || isArchived {
			return false
		}
		return true
	}

	// Compute role.
	var have policy.Role
	switch actor {
	case actorOwner, actorSuspendedOwner:
		have = policy.RoleAdmin
	case actorCollabRead:
		have = policy.RoleRead
	case actorCollabTriage:
		have = policy.RoleTriage
	case actorCollabWrite, actorSuspendedCollabWrite:
		have = policy.RoleWrite
	case actorCollabMaintain:
		have = policy.RoleMaintain
	case actorCollabAdmin:
		have = policy.RoleAdmin
	}

	// Archived: writes denied (covers owner too).
	if isArchived && isWrite {
		return false
	}

	// Login-only social actions.
	if action == policy.ActionStarCreate || action == policy.ActionForkCreate ||
		action == policy.ActionWatchSet {
		return have != policy.RoleNone || (actor != actorAnonymous)
	}

	// Role check.
	want := mirrorMinRoleFor(action)
	if want == policy.RoleNone {
		return actor != actorAnonymous
	}
	return policy.RoleAtLeast(have, want)
}

// mirrorMinRoleFor mirrors policy.minRoleFor for test side. We could
// expose the internal helper; keeping the mirror enforces that the
// matrix knows about every action explicitly.
func mirrorMinRoleFor(a policy.Action) policy.Role {
	switch a {
	case policy.ActionRepoRead, policy.ActionIssueRead, policy.ActionPullRead:
		return policy.RoleRead
	case policy.ActionIssueClose, policy.ActionIssueLabel, policy.ActionIssueAssign:
		return policy.RoleTriage
	case policy.ActionIssueCreate, policy.ActionIssueComment:
		return policy.RoleRead
	case policy.ActionRepoWrite, policy.ActionPullCreate, policy.ActionPullReview, policy.ActionPullClose:
		return policy.RoleWrite
	case policy.ActionRepoSettingsGeneral, policy.ActionRepoSettingsBranches:
		return policy.RoleMaintain
	case policy.ActionRepoAdmin, policy.ActionRepoSettingsCollaborators,
		policy.ActionRepoArchive, policy.ActionRepoDelete, policy.ActionRepoTransfer, policy.ActionRepoVisibility,
		policy.ActionPullMerge:
		return policy.RoleAdmin
	case policy.ActionStarCreate, policy.ActionForkCreate, policy.ActionWatchSet:
		return policy.RoleNone
	}
	return policy.RoleAdmin
}

func TestCan_Matrix(t *testing.T) {
	t.Parallel()
	d := policy.Deps{} // pool nil OK; cache primes the role

	for _, ak := range []actorKind{
		actorAnonymous, actorOwner,
		actorCollabRead, actorCollabTriage, actorCollabWrite, actorCollabMaintain, actorCollabAdmin,
		actorUnrelated, actorSuspendedOwner, actorSuspendedCollabWrite, actorSiteAdmin,
	} {
		for _, rk := range []repoKind{
			repoPublic, repoPrivate, repoArchivedPublic, repoArchivedPrivate, repoDeletedPublic,
		} {
			for _, action := range policy.AllActions {
				ak, rk, action := ak, rk, action
				name := ak.String() + "/" + rk.String() + "/" + string(action)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					ctx := ctxWithCollabRole(t, ak)
					actor := makeActor(ak)
					repo := makeRepo(rk)
					got := policy.Can(ctx, d, actor, action, repo).Allow
					want := expect(ak, rk, action)
					if got != want {
						t.Errorf("Can(%s, %s, %s) = %v, want %v",
							ak, rk, action, got, want)
					}
				})
			}
		}
	}
}

func TestCan_PublicIssueParticipation(t *testing.T) {
	t.Parallel()

	d := policy.Deps{}
	publicRepo := makeRepo(repoPublic)
	privateRepo := makeRepo(repoPrivate)
	archivedPublicRepo := makeRepo(repoArchivedPublic)
	stranger := makeActor(actorUnrelated)
	readCollab := makeActor(actorCollabRead)

	for _, action := range []policy.Action{policy.ActionIssueCreate, policy.ActionIssueComment} {
		action := action
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()

			if got := policy.Can(context.Background(), d, stranger, action, publicRepo); !got.Allow {
				t.Fatalf("logged-in non-collab on public repo should be allowed to %s: %#v", action, got)
			}
			if got := policy.Can(context.Background(), d, policy.AnonymousActor(), action, publicRepo); got.Allow || got.Code != policy.DenyAnonymous {
				t.Fatalf("anonymous public repo %s = %#v, want DenyAnonymous", action, got)
			}
			ctx := ctxWithCollabRole(t, actorCollabRead)
			if got := policy.Can(ctx, d, readCollab, action, privateRepo); !got.Allow {
				t.Fatalf("read collab on private repo should be allowed to %s: %#v", action, got)
			}
			if got := policy.Can(context.Background(), d, stranger, action, privateRepo); got.Allow || got.Code != policy.DenyVisibility {
				t.Fatalf("stranger private repo %s = %#v, want DenyVisibility", action, got)
			}
			if got := policy.Can(context.Background(), d, stranger, action, archivedPublicRepo); got.Allow || got.Code != policy.DenyArchived {
				t.Fatalf("archived public repo %s = %#v, want DenyArchived", action, got)
			}
		})
	}
}

func TestRoleAtLeast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		have, want policy.Role
		ok         bool
	}{
		{policy.RoleAdmin, policy.RoleRead, true},
		{policy.RoleAdmin, policy.RoleAdmin, true},
		{policy.RoleWrite, policy.RoleAdmin, false},
		{policy.RoleRead, policy.RoleTriage, false},
		{policy.RoleNone, policy.RoleRead, false},
		{policy.RoleRead, policy.RoleNone, false}, // RoleNone is meaningless as a target
	}
	for _, c := range cases {
		if got := policy.RoleAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("RoleAtLeast(%q, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestMaybe404(t *testing.T) {
	t.Parallel()
	priv := policy.RepoRef{ID: 1, OwnerUserID: ownerID, Visibility: "private"}
	pub := policy.RepoRef{ID: 1, OwnerUserID: ownerID, Visibility: "public"}
	owner := policy.UserActor(ownerID, "o", false, false)
	stranger := policy.UserActor(otherID, "s", false, false)

	// Anonymous on private → 404 leak guard.
	if got := policy.Maybe404(policy.Decision{Allow: false}, priv, policy.AnonymousActor()); got != 404 {
		t.Errorf("anonymous on private: got %d, want 404", got)
	}
	// Stranger on private → 404 (don't reveal existence).
	if got := policy.Maybe404(policy.Decision{Allow: false}, priv, stranger); got != 404 {
		t.Errorf("stranger on private: got %d, want 404", got)
	}
	// Owner on private (e.g. archived push) → real 403.
	if got := policy.Maybe404(policy.Decision{Allow: false}, priv, owner); got != 403 {
		t.Errorf("owner deny on private: got %d, want 403", got)
	}
	// Public deny → 403 always.
	if got := policy.Maybe404(policy.Decision{Allow: false}, pub, policy.AnonymousActor()); got != 403 {
		t.Errorf("anonymous deny on public: got %d, want 403", got)
	}
	// Allow → 200 regardless of repo shape.
	if got := policy.Maybe404(policy.Decision{Allow: true}, priv, owner); got != 200 {
		t.Errorf("allow: got %d, want 200", got)
	}
}

func TestCacheInvalidate(t *testing.T) {
	t.Parallel()
	ctx := policy.WithCache(context.Background())
	policy.PrimeCacheForTest(ctx, otherID, repoIDV, policy.RoleAdmin)

	// Confirm primed.
	d := policy.Deps{}
	actor := policy.UserActor(otherID, "u", false, false)
	repo := policy.RepoRef{ID: repoIDV, OwnerUserID: ownerID, Visibility: "private"}
	if !policy.Can(ctx, d, actor, policy.ActionRepoAdmin, repo).Allow {
		t.Fatalf("primed cache should grant admin")
	}

	// Invalidate; with no DB pool the next lookup falls through to RoleNone.
	policy.InvalidateRepo(ctx, repoIDV)
	if policy.Can(ctx, d, actor, policy.ActionRepoAdmin, repo).Allow {
		t.Errorf("after invalidate, admin should deny without role row")
	}
}

// TestPermissionsDoc_CoversEveryAction guards against drift between
// the policy package and docs/internal/permissions.md. The doc's
// "Action → minimum role" table must mention every Action constant
// exposed via AllActions, so an action added in code without a doc
// update fails this test loudly. (S00-S25 audit, finding for doc/code
// sync.)
//
// Matching is by the action's string value (e.g. "repo:read") which
// is what the doc renders verbatim in backticks. We do not check the
// exact min-role column — that lives in `mirrorMinRoleFor` already.
func TestPermissionsDoc_CoversEveryAction(t *testing.T) {
	t.Parallel()
	docPath := filepath.Join("..", "..", "..", "docs", "internal", "permissions.md")
	body, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read permissions.md: %v", err)
	}
	doc := string(body)
	for _, a := range policy.AllActions {
		needle := "`" + string(a) + "`"
		if !strings.Contains(doc, needle) {
			t.Errorf("permissions.md does not mention action %q (looking for %s)", a, needle)
		}
	}
}

// TestImpersonation_ReadOnlyDeniesWrites pins the canonical foot-gun
// guard: an impersonating admin without ImpersonateWriteOK must not
// be able to write, regardless of the underlying actor's role.
func TestImpersonation_ReadOnlyDeniesWrites(t *testing.T) {
	ctx := t.Context()
	d := policy.Deps{Pool: nil}
	repo := policy.RepoRef{ID: 1, OwnerUserID: 99, Visibility: "public"}
	actor := policy.Actor{
		UserID:        99, // the impersonated user
		Impersonating: true,
	}
	dec := policy.Can(ctx, d, actor, policy.ActionIssueComment, repo)
	if dec.Allow {
		t.Fatalf("impersonation read-only must deny writes; got allow")
	}
	if dec.Code != policy.DenyImpersonationReadOnly {
		t.Errorf("Code = %v; want DenyImpersonationReadOnly", dec.Code)
	}
}

// TestImpersonation_WriteModeOverridesReadOnly: with ImpersonateWriteOK
// flipped on, the policy proceeds to the normal role checks (the
// underlying role + repo state still gate, this just lifts the
// impersonation-specific deny).
func TestImpersonation_WriteModeOverridesReadOnly(t *testing.T) {
	ctx := t.Context()
	d := policy.Deps{Pool: nil}
	repo := policy.RepoRef{ID: 1, OwnerUserID: 99, Visibility: "public"}
	actor := policy.Actor{
		UserID:             99,
		Impersonating:      true,
		ImpersonateWriteOK: true,
	}
	dec := policy.Can(ctx, d, actor, policy.ActionIssueComment, repo)
	// We don't assert Allow here because the action still passes
	// through role checks (which will deny without DB-backed role
	// info). The relevant assertion is "not the impersonation deny."
	if !dec.Allow && dec.Code == policy.DenyImpersonationReadOnly {
		t.Fatalf("write-mode should clear impersonation deny; got %v", dec)
	}
}
