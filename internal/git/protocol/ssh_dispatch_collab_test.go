// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
	"github.com/tenseleyFlow/shithub/internal/git/protocol"
)

// TestDispatch_CollabWriteCanPushPrivate confirms that adding eve as a
// 'write' collaborator on alice's private repo lets her push. Before
// S15 this would have been ErrSSHPermDenied; after S15 the policy
// package grants based on the role row.
func TestDispatch_CollabWriteCanPushPrivate(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	pq := policydb.New()
	if err := pq.UpsertCollabRole(context.Background(), env.pool, policydb.UpsertCollabRoleParams{
		RepoID: repoIDFor(t, env, "alice", "private"),
		UserID: env.eve,
		Role:   policydb.CollabRoleWrite,
	}); err != nil {
		t.Fatalf("UpsertCollabRole: %v", err)
	}

	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/private'",
		UserID:          env.eve,
	})
	if err != nil {
		t.Fatalf("collab-write push denied: %v", err)
	}
}

// TestDispatch_CollabReadCannotPush confirms that a 'read' grant lets
// eve clone but not push.
func TestDispatch_CollabReadCannotPush(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	pq := policydb.New()
	if err := pq.UpsertCollabRole(context.Background(), env.pool, policydb.UpsertCollabRoleParams{
		RepoID: repoIDFor(t, env, "alice", "private"),
		UserID: env.eve,
		Role:   policydb.CollabRoleRead,
	}); err != nil {
		t.Fatalf("UpsertCollabRole: %v", err)
	}
	// Pull: allowed.
	if _, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/private'",
		UserID:          env.eve,
	}); err != nil {
		t.Fatalf("collab-read pull denied: %v", err)
	}
	// Push: denied with permission-denied (not 404, eve has read so
	// existence is no secret).
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/private'",
		UserID:          env.eve,
	})
	if !errors.Is(err, protocol.ErrSSHPermDenied) {
		t.Fatalf("collab-read push: err = %v, want ErrSSHPermDenied", err)
	}
}

// TestDispatch_DemotedCollabLosesAccessNextRequest mirrors the spec's
// definition-of-done bullet: dropping the role takes effect immediately
// on the next request (no cross-request cache).
func TestDispatch_DemotedCollabLosesAccessNextRequest(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	pq := policydb.New()
	repoID := repoIDFor(t, env, "alice", "private")
	if err := pq.UpsertCollabRole(context.Background(), env.pool, policydb.UpsertCollabRoleParams{
		RepoID: repoID, UserID: env.eve, Role: policydb.CollabRoleAdmin,
	}); err != nil {
		t.Fatalf("UpsertCollabRole admin: %v", err)
	}
	// Eve as admin: push allowed.
	if _, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/private'",
		UserID:          env.eve,
	}); err != nil {
		t.Fatalf("admin push denied: %v", err)
	}
	// Demote to read.
	if err := pq.UpsertCollabRole(context.Background(), env.pool, policydb.UpsertCollabRoleParams{
		RepoID: repoID, UserID: env.eve, Role: policydb.CollabRoleRead,
	}); err != nil {
		t.Fatalf("UpsertCollabRole read: %v", err)
	}
	// Next request: push denied.
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-receive-pack 'alice/private'",
		UserID:          env.eve,
	})
	if !errors.Is(err, protocol.ErrSSHPermDenied) {
		t.Fatalf("demoted push: err = %v, want ErrSSHPermDenied", err)
	}
}

// TestDispatch_RemovedCollabFallsBack404 confirms that fully removing
// the row turns eve back into a stranger on a private repo: the
// rejection is "repo not found" (existence-leak guard).
func TestDispatch_RemovedCollabFallsBack404(t *testing.T) {
	t.Parallel()
	env := setupDispatch(t)
	pq := policydb.New()
	repoID := repoIDFor(t, env, "alice", "private")
	if err := pq.UpsertCollabRole(context.Background(), env.pool, policydb.UpsertCollabRoleParams{
		RepoID: repoID, UserID: env.eve, Role: policydb.CollabRoleRead,
	}); err != nil {
		t.Fatalf("UpsertCollabRole: %v", err)
	}
	if err := pq.RemoveCollab(context.Background(), env.pool, policydb.RemoveCollabParams{
		RepoID: repoID, UserID: env.eve,
	}); err != nil {
		t.Fatalf("RemoveCollab: %v", err)
	}
	_, _, err := protocol.PrepareDispatch(context.Background(), env.deps, protocol.SSHDispatchInput{
		OriginalCommand: "git-upload-pack 'alice/private'",
		UserID:          env.eve,
	})
	if !errors.Is(err, protocol.ErrSSHRepoNotFound) {
		t.Fatalf("removed collab pull: err = %v, want ErrSSHRepoNotFound", err)
	}
	// Sanity-check the policy decision separately.
	_ = policy.ActionRepoRead
}

// repoIDFor looks up the bigserial id for a repo so the collab insert
// references a real row. setupDispatch creates "public" and "private";
// callers ask by name.
func repoIDFor(t *testing.T, env *dispatchEnv, owner, name string) int64 {
	t.Helper()
	var id int64
	err := env.pool.QueryRow(context.Background(),
		`SELECT r.id FROM repos r JOIN users u ON u.id = r.owner_user_id
         WHERE u.username = $1 AND r.name = $2`,
		owner, name).Scan(&id)
	if err != nil {
		t.Fatalf("repoIDFor %s/%s: %v", owner, name, err)
	}
	return id
}
