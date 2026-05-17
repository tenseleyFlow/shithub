// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const scheduledReminderFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestScheduledReminderPrivateTargetRequiresTeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newScheduledReminderEnv(t)
	privateRepo := env.createRepo(t, "private-repo", reposdb.RepoVisibilityPrivate)

	_, err := CreateScheduledReminder(ctx, env.deps, env.org.ID, env.owner.ID, ScheduledReminderInput{
		Name:                     "Private review follow-up",
		Target:                   orgsdb.OrgScheduledReminderTargetRepository,
		RepoID:                   privateRepo.ID,
		CronExpr:                 "0 9 * * 1",
		Timezone:                 "UTC",
		ConditionReviewRequested: true,
		MinAgeMinutes:            60,
	})
	if !errors.Is(err, ErrScheduledReminderRequiresTeam) {
		t.Fatalf("CreateScheduledReminder err=%v, want ErrScheduledReminderRequiresTeam", err)
	}

	if err := env.setTeamPlan(t, "team-private-target"); err != nil {
		t.Fatalf("set team plan: %v", err)
	}
	row, err := CreateScheduledReminder(ctx, env.deps, env.org.ID, env.owner.ID, ScheduledReminderInput{
		Name:                     "Private review follow-up",
		Target:                   orgsdb.OrgScheduledReminderTargetRepository,
		RepoID:                   privateRepo.ID,
		CronExpr:                 "0 9 * * 1",
		Timezone:                 "UTC",
		ConditionReviewRequested: true,
		MinAgeMinutes:            60,
	})
	if err != nil {
		t.Fatalf("CreateScheduledReminder with Team: %v", err)
	}
	if row.RepoID.Int64 != privateRepo.ID {
		t.Fatalf("row repo_id=%v, want %d", row.RepoID, privateRepo.ID)
	}
}

func TestScheduledReminderLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newScheduledReminderEnv(t)
	repo := env.createRepo(t, "public-repo", reposdb.RepoVisibilityPublic)

	row, err := CreateScheduledReminder(ctx, env.deps, env.org.ID, env.owner.ID, ScheduledReminderInput{
		Name:                         "Weekly reminders",
		Target:                       orgsdb.OrgScheduledReminderTargetAllRepositories,
		CronExpr:                     "0 9 * * 1",
		Timezone:                     "UTC",
		ConditionReviewRequested:     true,
		ConditionTeamReviewRequested: true,
		MinAgeMinutes:                30,
	})
	if err != nil {
		t.Fatalf("CreateScheduledReminder: %v", err)
	}
	updated, err := UpdateScheduledReminder(ctx, env.deps, env.org.ID, row.ID, ScheduledReminderInput{
		Name:                     "Repository reminders",
		Target:                   orgsdb.OrgScheduledReminderTargetRepository,
		RepoID:                   repo.ID,
		CronExpr:                 "30 10 * * 2",
		Timezone:                 "America/New_York",
		ConditionReviewRequested: true,
		MinAgeMinutes:            120,
	})
	if err != nil {
		t.Fatalf("UpdateScheduledReminder: %v", err)
	}
	if updated.Name != "Repository reminders" || updated.RepoID.Int64 != repo.ID || updated.Timezone != "America/New_York" {
		t.Fatalf("updated row = %+v", updated)
	}
	if err := PauseScheduledReminder(ctx, env.deps, env.org.ID, row.ID); err != nil {
		t.Fatalf("PauseScheduledReminder: %v", err)
	}
	resumed, err := ResumeScheduledReminder(ctx, env.deps, env.org.ID, row.ID)
	if err != nil {
		t.Fatalf("ResumeScheduledReminder: %v", err)
	}
	if resumed.PausedAt.Valid {
		t.Fatalf("resumed PausedAt=%v, want invalid", resumed.PausedAt)
	}
	if err := DeleteScheduledReminder(ctx, env.deps, env.org.ID, row.ID); err != nil {
		t.Fatalf("DeleteScheduledReminder: %v", err)
	}
	views, err := ListScheduledReminders(ctx, env.deps, env.org.ID)
	if err != nil {
		t.Fatalf("ListScheduledReminders: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("reminders after delete=%d, want 0", len(views))
	}
}

func TestScheduledReminderDeliveryIsIdempotentAndSkipsSuspendedRecipients(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newScheduledReminderEnv(t)
	if err := env.setTeamPlan(t, "team-delivery"); err != nil {
		t.Fatalf("set team plan: %v", err)
	}
	repo := env.createRepo(t, "reviews", reposdb.RepoVisibilityPrivate)
	reviewer := env.createUser(t, "reviewer")
	suspended := env.createUser(t, "suspended")
	env.addMember(t, reviewer.ID)
	env.addMember(t, suspended.ID)
	env.suspendUser(t, suspended.ID)
	prID := env.createPullRequest(t, repo.ID, env.owner.ID, "Needs review")
	env.requestUserReview(t, prID, reviewer.ID)
	env.requestUserReview(t, prID, suspended.ID)

	row, err := CreateScheduledReminder(ctx, env.deps, env.org.ID, env.owner.ID, ScheduledReminderInput{
		Name:                     "Due reviews",
		Target:                   orgsdb.OrgScheduledReminderTargetAllRepositories,
		CronExpr:                 "0 9 * * 1",
		Timezone:                 "UTC",
		ConditionReviewRequested: true,
		MinAgeMinutes:            0,
	})
	if err != nil {
		t.Fatalf("CreateScheduledReminder: %v", err)
	}
	delivered, err := deliverScheduledReminder(ctx, ScheduledReminderSweepDeps{Deps: env.deps}, row)
	if err != nil {
		t.Fatalf("deliverScheduledReminder: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered=%d, want 1", delivered)
	}
	deliveredAgain, err := deliverScheduledReminder(ctx, ScheduledReminderSweepDeps{Deps: env.deps}, row)
	if err != nil {
		t.Fatalf("deliverScheduledReminder second: %v", err)
	}
	if deliveredAgain != 0 {
		t.Fatalf("second delivered=%d, want 0", deliveredAgain)
	}
	q := notifdb.New()
	reviewerCount, err := q.CountNotificationsForRecipient(ctx, env.pool, notifdb.CountNotificationsForRecipientParams{
		RecipientUserID: reviewer.ID,
		Column2:         false,
	})
	if err != nil {
		t.Fatalf("reviewer notification count: %v", err)
	}
	if reviewerCount != 1 {
		t.Fatalf("reviewer notification count=%d, want 1", reviewerCount)
	}
	suspendedCount, err := q.CountNotificationsForRecipient(ctx, env.pool, notifdb.CountNotificationsForRecipientParams{
		RecipientUserID: suspended.ID,
		Column2:         false,
	})
	if err != nil {
		t.Fatalf("suspended notification count: %v", err)
	}
	if suspendedCount != 0 {
		t.Fatalf("suspended notification count=%d, want 0", suspendedCount)
	}
}

type scheduledReminderEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	deps  Deps
	owner usersdb.User
	org   orgsdb.Org
}

func newScheduledReminderEnv(t *testing.T) *scheduledReminderEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	owner := createScheduledReminderUser(t, pool, "owner")
	deps := Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	org, err := Create(ctx, deps, CreateParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		CreatedByUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("Create org: %v", err)
	}
	return &scheduledReminderEnv{t: t, pool: pool, deps: deps, owner: owner, org: org}
}

func (e *scheduledReminderEnv) createUser(t *testing.T, username string) usersdb.User {
	t.Helper()
	return createScheduledReminderUser(t, e.pool, username)
}

func createScheduledReminderUser(t *testing.T, db usersdb.DBTX, username string) usersdb.User {
	t.Helper()
	user, err := usersdb.New().CreateUser(context.Background(), db, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  username,
		PasswordHash: scheduledReminderFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	return user
}

func (e *scheduledReminderEnv) createRepo(t *testing.T, name string, visibility reposdb.RepoVisibility) reposdb.Repo {
	t.Helper()
	repo, err := reposdb.New().CreateRepo(context.Background(), e.pool, reposdb.CreateRepoParams{
		OwnerOrgID:      pgtype.Int8{Int64: e.org.ID, Valid: true},
		Name:            name,
		Description:     name,
		Visibility:      visibility,
		DefaultBranch:   "trunk",
		LicenseKey:      pgtype.Text{},
		PrimaryLanguage: pgtype.Text{},
	})
	if err != nil {
		t.Fatalf("CreateRepo %s: %v", name, err)
	}
	return repo
}

func (e *scheduledReminderEnv) addMember(t *testing.T, userID int64) {
	t.Helper()
	if err := orgsdb.New().AddOrgMember(context.Background(), e.pool, orgsdb.AddOrgMemberParams{
		OrgID:           e.org.ID,
		UserID:          userID,
		Role:            orgsdb.OrgRoleMember,
		InvitedByUserID: pgtype.Int8{Int64: e.owner.ID, Valid: true},
	}); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
}

func (e *scheduledReminderEnv) suspendUser(t *testing.T, userID int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET suspended_at = now(), suspended_reason = 'test' WHERE id = $1`, userID); err != nil {
		t.Fatalf("suspend user: %v", err)
	}
}

func (e *scheduledReminderEnv) createPullRequest(t *testing.T, repoID, authorID int64, title string) int64 {
	t.Helper()
	ctx := context.Background()
	var issueID int64
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO issues (repo_id, number, kind, title, body, author_user_id, updated_at)
		VALUES ($1, 1, 'pr', $2, '', $3, now() - interval '2 days')
		RETURNING id
	`, repoID, title, authorID).Scan(&issueID); err != nil {
		t.Fatalf("insert pr issue: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO pull_requests (issue_id, base_ref, head_ref, head_repo_id, base_oid, head_oid)
		VALUES ($1, 'trunk', 'feature', $2, 'base', 'head')
	`, issueID, repoID); err != nil {
		t.Fatalf("insert pull_request: %v", err)
	}
	return issueID
}

func (e *scheduledReminderEnv) requestUserReview(t *testing.T, prIssueID, userID int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO pr_review_requests (pr_issue_id, requested_user_id, requested_by_user_id)
		VALUES ($1, $2, $3)
	`, prIssueID, userID, e.owner.ID); err != nil {
		t.Fatalf("insert review request: %v", err)
	}
}

func (e *scheduledReminderEnv) setTeamPlan(t *testing.T, suffix string) error {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: e.pool}, billing.SubscriptionSnapshot{
		OrgID:                    e.org.ID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_" + suffix,
		StripeSubscriptionItemID: "si_" + suffix,
		CurrentPeriodStart:       now,
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_" + suffix,
	})
	return err
}
