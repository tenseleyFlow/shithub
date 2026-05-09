// SPDX-License-Identifier: AGPL-3.0-or-later

package notif_test

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notif"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// captureSender records every Send call so tests can count them.
type captureSender struct {
	mu       sync.Mutex
	messages []email.Message
}

func (c *captureSender) Send(_ context.Context, m email.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	return nil
}
func (c *captureSender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

type fixture struct {
	pool   *pgxpool.Pool
	deps   notif.Deps
	sender *captureSender
	repoID int64
	author int64 // creator + actor in most tests
}

func setupFanout(t *testing.T) fixture {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	uq := usersdb.New()
	author, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: author.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sender := &captureSender{}
	deps := notif.Deps{
		Pool:           pool,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailSender:    sender,
		EmailFrom:      "noreply@example.com",
		SiteName:       "shithub",
		BaseURL:        "https://shithub.example",
		UnsubscribeKey: []byte("test-key-32-bytes-aaaaaaaaaaaaaa"),
	}
	return fixture{pool: pool, deps: deps, sender: sender, repoID: repo.ID, author: author.ID}
}

func mustUser(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: name, DisplayName: name, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	// Give every user a verified primary email so the email-side
	// gate doesn't trivially short-circuit the storm-dampener tests.
	em, err := usersdb.New().CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: u.ID,
		Email:  name + "@example.com",
	})
	if err != nil {
		t.Fatalf("create email %s: %v", name, err)
	}
	if err := usersdb.New().MarkUserEmailVerified(context.Background(), pool, em.ID); err != nil {
		t.Fatalf("verify email %s: %v", name, err)
	}
	if err := usersdb.New().LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: u.ID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("set primary email %s: %v", name, err)
	}
	return u.ID
}

func mustIssue(t *testing.T, pool *pgxpool.Pool, repoID, authorID int64) issuesdb.Issue {
	t.Helper()
	ctx := context.Background()
	q := issuesdb.New()
	if err := q.EnsureRepoIssueCounter(ctx, pool, repoID); err != nil {
		t.Fatalf("ensure counter: %v", err)
	}
	num, err := q.AllocateIssueNumber(ctx, pool, repoID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	row, err := q.CreateIssue(ctx, pool, issuesdb.CreateIssueParams{
		RepoID:       repoID,
		Number:       num,
		Kind:         issuesdb.IssueKindIssue,
		Title:        "test issue " + strconv.FormatInt(num, 10),
		Body:         "body",
		AuthorUserID: pgtype.Int8{Int64: authorID, Valid: true},
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	return row
}

func emitComment(t *testing.T, fx fixture, issue issuesdb.Issue, actorID int64, mentions []int64) {
	t.Helper()
	if err := notif.Emit(context.Background(), fx.pool, notif.Event{
		ActorUserID: actorID,
		Kind:        "issue_comment_created",
		RepoID:      fx.repoID,
		SourceKind:  "issue",
		SourceID:    issue.ID,
		Public:      true,
		Mentions:    mentions,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

// ─── tests ─────────────────────────────────────────────────────────

// Five comments on one issue produce ONE inbox row for a watching
// recipient, with last_event_at advanced.
func TestFanout_ThreadCoalescing(t *testing.T) {
	fx := setupFanout(t)
	ctx := context.Background()

	bob := mustUser(t, fx.pool, "bob")
	// Bob watches the repo at level=all so he gets every comment.
	if err := socialdb.New().UpsertWatch(ctx, fx.pool, socialdb.UpsertWatchParams{
		UserID: bob, RepoID: fx.repoID, Level: socialdb.WatchLevelAll,
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	issue := mustIssue(t, fx.pool, fx.repoID, fx.author)

	for i := 0; i < 5; i++ {
		emitComment(t, fx, issue, fx.author, nil)
	}
	if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
		t.Fatalf("fanout: %v", err)
	}

	// Single inbox row coalesced for bob.
	count, err := notifdb.New().CountNotificationsForRecipient(ctx, fx.pool,
		notifdb.CountNotificationsForRecipientParams{RecipientUserID: bob})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 coalesced row, got %d", count)
	}
}

// Same scenario as coalesce: 5 events should produce ONE email
// thanks to the per-thread storm dampener (cap=1 per 10 minutes).
func TestFanout_StormDampener_SingleEmailPerThread(t *testing.T) {
	fx := setupFanout(t)
	ctx := context.Background()

	bob := mustUser(t, fx.pool, "bob")
	if err := socialdb.New().UpsertWatch(ctx, fx.pool, socialdb.UpsertWatchParams{
		UserID: bob, RepoID: fx.repoID, Level: socialdb.WatchLevelAll,
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	issue := mustIssue(t, fx.pool, fx.repoID, fx.author)
	for i := 0; i < 5; i++ {
		emitComment(t, fx, issue, fx.author, nil)
		if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
			t.Fatalf("fanout: %v", err)
		}
	}
	if got := fx.sender.count(); got != 1 {
		t.Fatalf("want 1 email after dampener, got %d", got)
	}
}

// The actor never notifies themselves — even if they are a
// watcher / author / etc.
func TestFanout_SelfSuppression(t *testing.T) {
	fx := setupFanout(t)
	ctx := context.Background()

	// Make the author a level=all watcher of their own repo so they
	// would otherwise match the watcher slot.
	if err := socialdb.New().UpsertWatch(ctx, fx.pool, socialdb.UpsertWatchParams{
		UserID: fx.author, RepoID: fx.repoID, Level: socialdb.WatchLevelAll,
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	issue := mustIssue(t, fx.pool, fx.repoID, fx.author)
	emitComment(t, fx, issue, fx.author, nil)
	if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	count, _ := notifdb.New().CountNotificationsForRecipient(ctx, fx.pool,
		notifdb.CountNotificationsForRecipientParams{RecipientUserID: fx.author})
	if count != 0 {
		t.Fatalf("self-notification leaked: %d rows", count)
	}
}

// A user with watches.level=ignore on the repo gets nothing for
// regular events — but @mentions OVERRIDE ignore (matches GitHub
// semantics; the spec's day-1 lean).
func TestFanout_MentionOverridesIgnore(t *testing.T) {
	fx := setupFanout(t)
	ctx := context.Background()

	bob := mustUser(t, fx.pool, "bob")
	if err := socialdb.New().UpsertWatch(ctx, fx.pool, socialdb.UpsertWatchParams{
		UserID: bob, RepoID: fx.repoID, Level: socialdb.WatchLevelIgnore,
	}); err != nil {
		t.Fatalf("watch ignore: %v", err)
	}

	issue := mustIssue(t, fx.pool, fx.repoID, fx.author)
	// Plain comment: bob should get nothing (ignore level + no
	// other relation).
	emitComment(t, fx, issue, fx.author, nil)
	if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	count, _ := notifdb.New().CountNotificationsForRecipient(ctx, fx.pool,
		notifdb.CountNotificationsForRecipientParams{RecipientUserID: bob})
	if count != 0 {
		t.Fatalf("ignored user got plain comment: %d rows", count)
	}
	// Mention should bypass ignore.
	emitComment(t, fx, issue, fx.author, []int64{bob})
	if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	count, _ = notifdb.New().CountNotificationsForRecipient(ctx, fx.pool,
		notifdb.CountNotificationsForRecipientParams{RecipientUserID: bob})
	if count != 1 {
		t.Fatalf("mention should override ignore; got %d rows", count)
	}
}

// Visibility re-check: bob is not a collab on a private repo. An
// event emitted with Public=true (e.g. emitter mis-stamped or repo
// flipped to private mid-flight) must not produce a notification
// for bob.
func TestFanout_VisibilityRecheck_PrivateRepo(t *testing.T) {
	fx := setupFanout(t)
	ctx := context.Background()
	// Flip the repo to private.
	if _, err := fx.pool.Exec(ctx,
		`UPDATE repos SET visibility='private' WHERE id=$1`, fx.repoID); err != nil {
		t.Fatalf("flip private: %v", err)
	}
	bob := mustUser(t, fx.pool, "bob")
	if err := socialdb.New().UpsertWatch(ctx, fx.pool, socialdb.UpsertWatchParams{
		UserID: bob, RepoID: fx.repoID, Level: socialdb.WatchLevelAll,
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	issue := mustIssue(t, fx.pool, fx.repoID, fx.author)
	emitComment(t, fx, issue, fx.author, nil)
	if _, err := notif.FanoutOnce(ctx, fx.deps); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	count, _ := notifdb.New().CountNotificationsForRecipient(ctx, fx.pool,
		notifdb.CountNotificationsForRecipientParams{RecipientUserID: bob})
	if count != 0 {
		t.Fatalf("non-collab on private got notification: %d rows", count)
	}
}

// Verify that the unsubscribe HMAC roundtrips: a recipient + thread
// gets a verifying signature; tampering with any field invalidates it.
func TestUnsubscribe_HMACRoundtrip(t *testing.T) {
	key := []byte("test-key-32-bytes-aaaaaaaaaaaaaa")
	const (
		uid  = int64(123)
		tk   = "issue"
		tid  = int64(45)
		bad  = "issues"
		other = int64(46)
	)
	// Build by parsing the URL the same path the email-side uses.
	// The exposed verifier takes the parsed pieces.
	good := makeSig(key, uid, tk, tid)
	if !notif.VerifyUnsubscribe(key, uid, tk, tid, good) {
		t.Fatal("good sig should verify")
	}
	if notif.VerifyUnsubscribe(key, uid, bad, tid, good) {
		t.Fatal("changed kind should NOT verify")
	}
	if notif.VerifyUnsubscribe(key, uid, tk, other, good) {
		t.Fatal("changed thread id should NOT verify")
	}
}

// makeSig duplicates the package's URL-builder math so the roundtrip
// test doesn't have to import internals or pull a half-built URL apart.
func makeSig(key []byte, uid int64, tk string, tid int64) string {
	// Mirror unsubscribeURL's payload format.
	rec := strconv.FormatInt(uid, 10)
	tids := strconv.FormatInt(tid, 10)
	payload := rec + ":" + tk + ":" + tids
	return notif.HMACSig(key, payload)
}
