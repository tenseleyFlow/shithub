// SPDX-License-Identifier: AGPL-3.0-or-later

package social_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// setup gives every test its own fresh DB + a public repo + an author
// user. Returns the pool (for direct sqlc reads), the social.Deps
// (without a Limiter so tests don't trip the rate cap), the author
// user id, and the repo id.
func setup(t *testing.T) (*pgxpool.Pool, social.Deps, int64, int64) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	deps := social.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return pool, deps, user.ID, repo.ID
}

func mustCreateUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	return u.ID
}

func mustCreateOrg(t *testing.T, pool *pgxpool.Pool, slug string, creatorID int64) int64 {
	t.Helper()
	o, err := orgsdb.New().CreateOrg(context.Background(), pool, orgsdb.CreateOrgParams{
		Slug:            slug,
		DisplayName:     slug,
		BillingEmail:    slug + "@example.test",
		CreatedByUserID: pgtype.Int8{Int64: creatorID, Valid: creatorID != 0},
	})
	if err != nil {
		t.Fatalf("CreateOrg %s: %v", slug, err)
	}
	return o.ID
}

func repoStarCount(t *testing.T, pool *pgxpool.Pool, repoID int64) int64 {
	t.Helper()
	r, err := reposdb.New().GetRepoByID(context.Background(), pool, repoID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	return r.StarCount
}

func repoWatcherCount(t *testing.T, pool *pgxpool.Pool, repoID int64) int64 {
	t.Helper()
	r, err := reposdb.New().GetRepoByID(context.Background(), pool, repoID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	return r.WatcherCount
}

func TestStar_IncrementsCount_AndIsIdempotent(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.Star(ctx, deps, uid, repoID, true); err != nil {
		t.Fatalf("Star: %v", err)
	}
	if got := repoStarCount(t, pool, repoID); got != 1 {
		t.Errorf("after first star: got %d, want 1", got)
	}
	// Re-star is a no-op (ON CONFLICT DO NOTHING + trigger fires only on
	// real INSERT). Count must not double.
	if err := social.Star(ctx, deps, uid, repoID, true); err != nil {
		t.Fatalf("Star (idempotent): %v", err)
	}
	if got := repoStarCount(t, pool, repoID); got != 1 {
		t.Errorf("after re-star: got %d, want 1 (idempotent)", got)
	}
}

func TestFollowUser_IdempotentCountsAndEvent(t *testing.T) {
	pool, deps, targetID, _ := setup(t)
	followerID := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.FollowUser(ctx, deps, followerID, targetID); err != nil {
		t.Fatalf("FollowUser: %v", err)
	}
	if err := social.FollowUser(ctx, deps, followerID, targetID); err != nil {
		t.Fatalf("FollowUser duplicate: %v", err)
	}
	q := socialdb.New()
	followers, err := q.CountFollowersForUser(ctx, pool, pgtype.Int8{Int64: targetID, Valid: true})
	if err != nil {
		t.Fatalf("CountFollowersForUser: %v", err)
	}
	if followers != 1 {
		t.Fatalf("followers = %d, want 1", followers)
	}
	following, err := q.CountFollowingForUser(ctx, pool, followerID)
	if err != nil {
		t.Fatalf("CountFollowingForUser: %v", err)
	}
	if following != 1 {
		t.Fatalf("following = %d, want 1", following)
	}
	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM domain_events WHERE actor_user_id = $1 AND kind = 'followed_user'`,
		followerID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count follow events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("event count = %d, want 1", eventCount)
	}
}

func TestFollowUser_RejectsSelf(t *testing.T) {
	_, deps, userID, _ := setup(t)
	if err := social.FollowUser(context.Background(), deps, userID, userID); !errors.Is(err, social.ErrCannotFollowSelf) {
		t.Fatalf("FollowUser self err = %v, want ErrCannotFollowSelf", err)
	}
}

func TestFollowOrg_IdempotentCountsAndEvent(t *testing.T) {
	pool, deps, creatorID, _ := setup(t)
	followerID := mustCreateUser(t, pool, "bob")
	orgID := mustCreateOrg(t, pool, "octo-org", creatorID)
	ctx := context.Background()

	if err := social.FollowOrg(ctx, deps, followerID, orgID); err != nil {
		t.Fatalf("FollowOrg: %v", err)
	}
	if err := social.FollowOrg(ctx, deps, followerID, orgID); err != nil {
		t.Fatalf("FollowOrg duplicate: %v", err)
	}
	followers, err := socialdb.New().CountFollowersForOrg(ctx, pool, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		t.Fatalf("CountFollowersForOrg: %v", err)
	}
	if followers != 1 {
		t.Fatalf("org followers = %d, want 1", followers)
	}
	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM domain_events WHERE actor_user_id = $1 AND kind = 'followed_org'`,
		followerID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count org follow events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("event count = %d, want 1", eventCount)
	}
}

func TestUnfollowUser_Idempotent(t *testing.T) {
	pool, deps, targetID, _ := setup(t)
	followerID := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.FollowUser(ctx, deps, followerID, targetID); err != nil {
		t.Fatalf("FollowUser: %v", err)
	}
	if err := social.UnfollowUser(ctx, deps, followerID, targetID); err != nil {
		t.Fatalf("UnfollowUser: %v", err)
	}
	if err := social.UnfollowUser(ctx, deps, followerID, targetID); err != nil {
		t.Fatalf("UnfollowUser duplicate: %v", err)
	}
	followers, err := socialdb.New().CountFollowersForUser(ctx, pool, pgtype.Int8{Int64: targetID, Valid: true})
	if err != nil {
		t.Fatalf("CountFollowersForUser: %v", err)
	}
	if followers != 0 {
		t.Fatalf("followers = %d, want 0", followers)
	}
}

func TestUnstar_DecrementsCount_AndIsIdempotent(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()
	_ = social.Star(ctx, deps, uid, repoID, true)

	if err := social.Unstar(ctx, deps, uid, repoID, true); err != nil {
		t.Fatalf("Unstar: %v", err)
	}
	if got := repoStarCount(t, pool, repoID); got != 0 {
		t.Errorf("after unstar: got %d, want 0", got)
	}
	// Re-unstar is a no-op.
	if err := social.Unstar(ctx, deps, uid, repoID, true); err != nil {
		t.Fatalf("Unstar (idempotent): %v", err)
	}
	if got := repoStarCount(t, pool, repoID); got != 0 {
		t.Errorf("after re-unstar: got %d, want 0", got)
	}
}

func TestStar_RequiresLogin(t *testing.T) {
	_, deps, _, repoID := setup(t)
	if err := social.Star(context.Background(), deps, 0, repoID, true); err == nil {
		t.Errorf("expected ErrNotLoggedIn for actor=0, got nil")
	}
}

func TestStar_EmitsDomainEvent(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()
	if err := social.Star(ctx, deps, uid, repoID, true); err != nil {
		t.Fatalf("Star: %v", err)
	}
	rows, err := socialdb.New().ListEventsForRepo(ctx, pool, socialdb.ListEventsForRepoParams{
		RepoID: pgtype.Int8{Int64: repoID, Valid: true},
		Limit:  10, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListEventsForRepo: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "star" || !rows[0].Public {
		t.Errorf("expected one public 'star' event, got %+v", rows)
	}
}

func TestSetWatch_All_IncrementsWatcherCount(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.SetWatch(ctx, deps, uid, repoID, social.WatchAll); err != nil {
		t.Fatalf("SetWatch: %v", err)
	}
	if got := repoWatcherCount(t, pool, repoID); got != 1 {
		t.Errorf("after SetWatch=all: got %d, want 1", got)
	}
}

func TestSetWatch_Ignore_DoesNotCountAsWatcher(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.SetWatch(ctx, deps, uid, repoID, social.WatchIgnore); err != nil {
		t.Fatalf("SetWatch: %v", err)
	}
	// `ignore` is the explicit "do not notify" — must not bump the count.
	if got := repoWatcherCount(t, pool, repoID); got != 0 {
		t.Errorf("after SetWatch=ignore: got %d, want 0", got)
	}
}

func TestSetWatch_TransitionsAcrossIgnore(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	// all → 1
	_ = social.SetWatch(ctx, deps, uid, repoID, social.WatchAll)
	if got := repoWatcherCount(t, pool, repoID); got != 1 {
		t.Fatalf("step 1: got %d, want 1", got)
	}
	// all → ignore: 1 → 0 (transition out of "watching").
	_ = social.SetWatch(ctx, deps, uid, repoID, social.WatchIgnore)
	if got := repoWatcherCount(t, pool, repoID); got != 0 {
		t.Errorf("after ignore: got %d, want 0", got)
	}
	// ignore → participating: 0 → 1 (transition back in).
	_ = social.SetWatch(ctx, deps, uid, repoID, social.WatchParticipating)
	if got := repoWatcherCount(t, pool, repoID); got != 1 {
		t.Errorf("after participating: got %d, want 1", got)
	}
}

func TestUnsetWatch_RestoresImplicitDefault(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	_ = social.SetWatch(ctx, deps, uid, repoID, social.WatchAll)
	if err := social.UnsetWatch(ctx, deps, uid, repoID); err != nil {
		t.Fatalf("UnsetWatch: %v", err)
	}
	// CurrentLevel resolves the absent-row case to participating.
	got, err := social.CurrentLevel(ctx, deps, uid, repoID)
	if err != nil {
		t.Fatalf("CurrentLevel: %v", err)
	}
	if got != social.WatchParticipating {
		t.Errorf("after UnsetWatch: level=%s, want participating", got)
	}
}

// TestAutoWatch_NonDestructive is the regression guard for the
// auto-watch contract: collaborator-add inserts level='all', but a
// later involvement event must NOT downgrade to 'participating'.
func TestAutoWatch_NonDestructive(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.AutoWatchOnCollab(ctx, deps, uid, repoID); err != nil {
		t.Fatalf("AutoWatchOnCollab: %v", err)
	}
	if got, _ := social.CurrentLevel(ctx, deps, uid, repoID); got != social.WatchAll {
		t.Fatalf("step 1: level=%s, want all", got)
	}
	if err := social.AutoWatchOnInvolvement(ctx, deps, uid, repoID); err != nil {
		t.Fatalf("AutoWatchOnInvolvement: %v", err)
	}
	if got, _ := social.CurrentLevel(ctx, deps, uid, repoID); got != social.WatchAll {
		t.Errorf("involvement should not overwrite collab default: got %s", got)
	}
}

// TestAutoWatch_PreservesUserChoice mirrors the spec pitfall about
// permission-revocation cascades: a user who explicitly chose `ignore`
// must keep that choice even when an involvement event fires.
func TestAutoWatch_PreservesUserChoice(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	uid := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	_ = social.SetWatch(ctx, deps, uid, repoID, social.WatchIgnore)
	_ = social.AutoWatchOnInvolvement(ctx, deps, uid, repoID)
	if got, _ := social.CurrentLevel(ctx, deps, uid, repoID); got != social.WatchIgnore {
		t.Errorf("user-chosen ignore should win: got %s", got)
	}
}

// TestStargazerList_ExcludesSuspended guards the spec's "suspended
// users don't taint public lists" pitfall.
func TestStargazerList_ExcludesSuspended(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	ctx := context.Background()
	good := mustCreateUser(t, pool, "good")
	bad := mustCreateUser(t, pool, "bad")
	_ = social.Star(ctx, deps, good, repoID, true)
	_ = social.Star(ctx, deps, bad, repoID, true)

	// Suspend bad.
	if _, err := pool.Exec(ctx,
		"UPDATE users SET suspended_at = now() WHERE id = $1", bad); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	rows, err := socialdb.New().ListStargazersForRepo(ctx, pool, socialdb.ListStargazersForRepoParams{
		RepoID: repoID, Limit: 10, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListStargazersForRepo: %v", err)
	}
	if len(rows) != 1 || rows[0].UserID != good {
		t.Errorf("expected only good user, got %d rows = %+v", len(rows), rows)
	}
}

func TestCaptureTrendingSnapshots_FeedsCachedTrending(t *testing.T) {
	pool, deps, _, repoID := setup(t)
	actorID := mustCreateUser(t, pool, "bob")
	ctx := context.Background()

	if err := social.Star(ctx, deps, actorID, repoID, true); err != nil {
		t.Fatalf("Star: %v", err)
	}
	if err := social.CaptureTrendingSnapshots(ctx, deps); err != nil {
		t.Fatalf("CaptureTrendingSnapshots: %v", err)
	}
	repos, err := social.CachedTrendingRepos(ctx, deps, social.TrendingScopeWeek, 7, 5)
	if err != nil {
		t.Fatalf("CachedTrendingRepos: %v", err)
	}
	if len(repos) == 0 || repos[0].RepoID != repoID {
		t.Fatalf("cached trending repos = %+v, want repo %d first", repos, repoID)
	}
	users, err := social.CachedTrendingUsers(ctx, deps, social.TrendingScopeWeek, 7, 5)
	if err != nil {
		t.Fatalf("CachedTrendingUsers: %v", err)
	}
	if len(users) == 0 || users[0].UserID != actorID {
		t.Fatalf("cached trending users = %+v, want actor %d first", users, actorID)
	}
}
