// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func setup(t *testing.T) (*pgxpool.Pool, orgs.Deps, int64) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	u, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	deps := orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return pool, deps, u.ID
}

func mustUser(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: name, DisplayName: name, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u.ID
}

func mustEmail(t *testing.T, pool *pgxpool.Pool, userID int64, addr string) {
	t.Helper()
	em, err := usersdb.New().CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: userID, Email: addr,
	})
	if err != nil {
		t.Fatalf("create email: %v", err)
	}
	if err := usersdb.New().MarkUserEmailVerified(context.Background(), pool, em.ID); err != nil {
		t.Fatalf("verify email: %v", err)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	_, deps, alice := setup(t)
	row, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if string(row.Slug) != "acme" {
		t.Fatalf("slug: want acme, got %q", row.Slug)
	}
	// Creator is the sole owner.
	owner, err := orgs.IsOwner(context.Background(), deps, row.ID, alice)
	if err != nil || !owner {
		t.Fatalf("alice should be owner: ok=%v err=%v", owner, err)
	}
}

func TestCreate_RejectsReservedSlug(t *testing.T) {
	_, deps, alice := setup(t)
	_, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "settings", CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrSlugReserved) {
		t.Fatalf("want ErrSlugReserved, got %v", err)
	}
}

func TestCreate_RejectsCollisionWithUsername(t *testing.T) {
	_, deps, alice := setup(t)
	// alice exists; trying to create org with slug "alice" must fail.
	_, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "alice", CreatedByUserID: alice,
	})
	if !errors.Is(err, orgs.ErrSlugTaken) {
		t.Fatalf("want ErrSlugTaken, got %v", err)
	}
}

func TestPrincipals_ResolveUserAndOrg(t *testing.T) {
	pool, deps, alice := setup(t)
	if _, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "beta", CreatedByUserID: alice,
	}); err != nil {
		t.Fatalf("create org: %v", err)
	}
	pUser, err := orgs.Resolve(context.Background(), pool, "alice")
	if err != nil || pUser.Kind != orgs.PrincipalUser {
		t.Fatalf("resolve alice: kind=%v err=%v", pUser.Kind, err)
	}
	pOrg, err := orgs.Resolve(context.Background(), pool, "beta")
	if err != nil || pOrg.Kind != orgs.PrincipalOrg {
		t.Fatalf("resolve beta: kind=%v err=%v", pOrg.Kind, err)
	}
	if _, err := orgs.Resolve(context.Background(), pool, "nope"); !errors.Is(err, orgs.ErrNoPrincipal) {
		t.Fatalf("want ErrNoPrincipal for unknown slug, got %v", err)
	}
}

func TestChangeRole_LastOwnerProtection(t *testing.T) {
	_, deps, alice := setup(t)
	row, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Demoting the only owner must refuse.
	err = orgs.ChangeRole(context.Background(), deps, row.ID, alice, "member")
	if !errors.Is(err, orgs.ErrLastOwner) {
		t.Fatalf("want ErrLastOwner, got %v", err)
	}
	// Removing the only owner must refuse.
	err = orgs.RemoveMember(context.Background(), deps, row.ID, alice)
	if !errors.Is(err, orgs.ErrLastOwner) {
		t.Fatalf("want ErrLastOwner on remove, got %v", err)
	}
}

func TestInvite_AcceptByUsername(t *testing.T) {
	pool, deps, alice := setup(t)
	bob := mustUser(t, pool, "bob")
	row, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: row.ID, InvitedByUserID: alice,
		TargetUsername: "bob", Role: "member",
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if !res.Invitation.TargetUserID.Valid || res.Invitation.TargetUserID.Int64 != bob {
		t.Fatalf("target user id mismatch")
	}
	if err := orgs.AcceptInvitation(context.Background(), deps, res.Invitation, bob); err != nil {
		t.Fatalf("accept: %v", err)
	}
	ok, _ := orgs.IsMember(context.Background(), deps, row.ID, bob)
	if !ok {
		t.Fatalf("bob should be a member")
	}
}

func TestInvite_AcceptByEmail(t *testing.T) {
	pool, deps, alice := setup(t)
	carol := mustUser(t, pool, "carol")
	mustEmail(t, pool, carol, "carol@test.com")
	row, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: row.ID, InvitedByUserID: alice,
		TargetEmail: "carol@test.com", Role: "member",
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if !res.Invitation.TargetEmail.Valid {
		t.Fatalf("target email should be valid")
	}
	if err := orgs.AcceptInvitation(context.Background(), deps, res.Invitation, carol); err != nil {
		t.Fatalf("accept: %v", err)
	}
	ok, _ := orgs.IsMember(context.Background(), deps, row.ID, carol)
	if !ok {
		t.Fatalf("carol should be a member")
	}
}

func TestInvite_AcceptByEmailRejectsWrongUser(t *testing.T) {
	pool, deps, alice := setup(t)
	carol := mustUser(t, pool, "carol")
	mustEmail(t, pool, carol, "carol@test.com")
	dave := mustUser(t, pool, "dave")
	mustEmail(t, pool, dave, "dave@test.com")
	row, _ := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", CreatedByUserID: alice,
	})
	res, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: row.ID, InvitedByUserID: alice,
		TargetEmail: "carol@test.com", Role: "member",
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	// Dave (different verified email) tries to claim carol's invite.
	err = orgs.AcceptInvitation(context.Background(), deps, res.Invitation, dave)
	if !errors.Is(err, orgs.ErrUnauthorizedAcceptor) {
		t.Fatalf("want ErrUnauthorizedAcceptor, got %v", err)
	}
}

func TestInvite_DuplicatePending(t *testing.T) {
	_, deps, alice := setup(t)
	row, _ := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", CreatedByUserID: alice,
	})
	bob := mustUser(t, deps.Pool, "bob")
	if _, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: row.ID, InvitedByUserID: alice, TargetUsername: "bob", Role: "member",
	}); err != nil {
		t.Fatalf("first invite: %v", err)
	}
	_ = bob
	_, err := orgs.Invite(context.Background(), deps, orgs.InviteParams{
		OrgID: row.ID, InvitedByUserID: alice, TargetUsername: "bob", Role: "member",
	})
	if !errors.Is(err, orgs.ErrInvitationDuplicate) {
		t.Fatalf("want ErrInvitationDuplicate, got %v", err)
	}
}

// ensure pgtype is referenced even if a future refactor removes the
// only inline use.
var _ = pgtype.Int8{}
