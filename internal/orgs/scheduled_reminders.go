// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

const (
	// ScheduledReminderSweepBatch caps one worker tick. The handler
	// self-enqueues when the batch fills so a backlog drains without a
	// long transaction or an in-memory scheduler.
	ScheduledReminderSweepBatch = 50

	ScheduledReminderNotificationKind   = "scheduled_reminder"
	ScheduledReminderNotificationReason = "scheduled_reminder"
)

var (
	ErrScheduledReminderNotFound     = errors.New("orgs: scheduled reminder not found")
	ErrScheduledReminderInvalid      = errors.New("orgs: invalid scheduled reminder")
	ErrScheduledReminderRequiresTeam = errors.New("orgs: scheduled reminders for private repositories require Team")
	ErrScheduledReminderTarget       = errors.New("orgs: scheduled reminder target does not belong to org")
)

type ScheduledReminderInput struct {
	Name                         string
	Target                       orgsdb.OrgScheduledReminderTarget
	RepoID                       int64
	TeamID                       int64
	CronExpr                     string
	Timezone                     string
	ConditionReviewRequested     bool
	ConditionTeamReviewRequested bool
	MinAgeMinutes                int32
	Paused                       bool
}

type ScheduledReminderView struct {
	ID                           int64
	OrgID                        int64
	Name                         string
	Target                       orgsdb.OrgScheduledReminderTarget
	RepoID                       int64
	TeamID                       int64
	RepoName                     string
	TeamSlug                     string
	TeamDisplayName              string
	CronExpr                     string
	Timezone                     string
	NextRunAt                    time.Time
	LastRunAt                    time.Time
	LastRunStatus                orgsdb.OrgScheduledReminderStatus
	LastRunError                 string
	ConditionReviewRequested     bool
	ConditionTeamReviewRequested bool
	MinAgeMinutes                int32
	Paused                       bool
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

func ListScheduledReminders(ctx context.Context, deps Deps, orgID int64) ([]ScheduledReminderView, error) {
	if deps.Pool == nil {
		return nil, errors.New("orgs: ListScheduledReminders needs Pool")
	}
	rows, err := orgsdb.New().ListOrgScheduledReminders(ctx, deps.Pool, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduledReminderView, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduledReminderListView(row))
	}
	return out, nil
}

func CreateScheduledReminder(ctx context.Context, deps Deps, orgID, actorUserID int64, in ScheduledReminderInput) (orgsdb.OrgScheduledReminder, error) {
	if deps.Pool == nil {
		return orgsdb.OrgScheduledReminder{}, errors.New("orgs: CreateScheduledReminder needs Pool")
	}
	norm, next, err := normalizeScheduledReminder(ctx, deps, orgID, in, time.Now().UTC())
	if err != nil {
		return orgsdb.OrgScheduledReminder{}, err
	}
	return orgsdb.New().CreateOrgScheduledReminder(ctx, deps.Pool, orgsdb.CreateOrgScheduledReminderParams{
		OrgID:                        orgID,
		Name:                         norm.Name,
		Target:                       norm.Target,
		CronExpr:                     norm.CronExpr,
		Timezone:                     norm.Timezone,
		NextRunAt:                    timestamptz(next),
		ConditionReviewRequested:     norm.ConditionReviewRequested,
		ConditionTeamReviewRequested: norm.ConditionTeamReviewRequested,
		MinAgeMinutes:                norm.MinAgeMinutes,
		RepoID:                       nullableID(norm.RepoID),
		TeamID:                       nullableID(norm.TeamID),
		CreatedByUserID:              nullableID(actorUserID),
	})
}

func UpdateScheduledReminder(ctx context.Context, deps Deps, orgID, reminderID int64, in ScheduledReminderInput) (orgsdb.OrgScheduledReminder, error) {
	if deps.Pool == nil {
		return orgsdb.OrgScheduledReminder{}, errors.New("orgs: UpdateScheduledReminder needs Pool")
	}
	norm, next, err := normalizeScheduledReminder(ctx, deps, orgID, in, time.Now().UTC())
	if err != nil {
		return orgsdb.OrgScheduledReminder{}, err
	}
	var pausedAt pgtype.Timestamptz
	if norm.Paused {
		pausedAt = timestamptz(time.Now().UTC())
	}
	row, err := orgsdb.New().UpdateOrgScheduledReminder(ctx, deps.Pool, orgsdb.UpdateOrgScheduledReminderParams{
		ID:                           reminderID,
		OrgID:                        orgID,
		Name:                         norm.Name,
		Target:                       norm.Target,
		CronExpr:                     norm.CronExpr,
		Timezone:                     norm.Timezone,
		NextRunAt:                    timestamptz(next),
		ConditionReviewRequested:     norm.ConditionReviewRequested,
		ConditionTeamReviewRequested: norm.ConditionTeamReviewRequested,
		MinAgeMinutes:                norm.MinAgeMinutes,
		RepoID:                       nullableID(norm.RepoID),
		TeamID:                       nullableID(norm.TeamID),
		PausedAt:                     pausedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return orgsdb.OrgScheduledReminder{}, ErrScheduledReminderNotFound
	}
	return row, err
}

func PauseScheduledReminder(ctx context.Context, deps Deps, orgID, reminderID int64) error {
	if deps.Pool == nil {
		return errors.New("orgs: PauseScheduledReminder needs Pool")
	}
	return orgsdb.New().PauseOrgScheduledReminder(ctx, deps.Pool, orgsdb.PauseOrgScheduledReminderParams{
		ID:    reminderID,
		OrgID: orgID,
	})
}

func ResumeScheduledReminder(ctx context.Context, deps Deps, orgID, reminderID int64) (orgsdb.OrgScheduledReminder, error) {
	if deps.Pool == nil {
		return orgsdb.OrgScheduledReminder{}, errors.New("orgs: ResumeScheduledReminder needs Pool")
	}
	row, err := orgsdb.New().GetOrgScheduledReminder(ctx, deps.Pool, reminderID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OrgID != orgID) {
		return orgsdb.OrgScheduledReminder{}, ErrScheduledReminderNotFound
	}
	if err != nil {
		return orgsdb.OrgScheduledReminder{}, err
	}
	if err := ensureScheduledReminderPrivateTargetEntitled(ctx, deps, orgID, ScheduledReminderInput{
		Target: row.Target,
		RepoID: int64FromPg(row.RepoID),
		TeamID: int64FromPg(row.TeamID),
	}); err != nil {
		return orgsdb.OrgScheduledReminder{}, err
	}
	next, err := nextScheduledReminderTick(row.CronExpr, row.Timezone, time.Now().UTC())
	if err != nil {
		return orgsdb.OrgScheduledReminder{}, fmt.Errorf("%w: %v", ErrScheduledReminderInvalid, err)
	}
	resumed, err := orgsdb.New().ResumeOrgScheduledReminder(ctx, deps.Pool, orgsdb.ResumeOrgScheduledReminderParams{
		ID:        reminderID,
		OrgID:     orgID,
		NextRunAt: timestamptz(next),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return orgsdb.OrgScheduledReminder{}, ErrScheduledReminderNotFound
	}
	return resumed, err
}

func DeleteScheduledReminder(ctx context.Context, deps Deps, orgID, reminderID int64) error {
	if deps.Pool == nil {
		return errors.New("orgs: DeleteScheduledReminder needs Pool")
	}
	return orgsdb.New().DeleteOrgScheduledReminder(ctx, deps.Pool, orgsdb.DeleteOrgScheduledReminderParams{
		ID:    reminderID,
		OrgID: orgID,
	})
}

type ScheduledReminderSweepDeps struct {
	Deps
	Now func() time.Time
}

func SweepScheduledReminders(ctx context.Context, deps ScheduledReminderSweepDeps) (int, error) {
	if deps.Pool == nil {
		return 0, errors.New("orgs: SweepScheduledReminders needs Pool")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	tx, err := deps.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("scheduled reminders: claim begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := orgsdb.New()
	rows, err := q.ClaimDueOrgScheduledReminders(ctx, tx, ScheduledReminderSweepBatch)
	if err != nil {
		return 0, fmt.Errorf("scheduled reminders: claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("scheduled reminders: claim commit: %w", err)
	}

	processed := 0
	for _, row := range rows {
		sweepOneScheduledReminder(ctx, deps, row, now().UTC())
		processed++
	}
	return processed, nil
}

func normalizeScheduledReminder(ctx context.Context, deps Deps, orgID int64, in ScheduledReminderInput, from time.Time) (ScheduledReminderInput, time.Time, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.CronExpr = strings.TrimSpace(in.CronExpr)
	in.Timezone = strings.TrimSpace(in.Timezone)
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if in.Target == "" {
		in.Target = orgsdb.OrgScheduledReminderTargetAllRepositories
	}
	if len(in.Name) == 0 || len(in.Name) > 80 {
		return ScheduledReminderInput{}, time.Time{}, ErrScheduledReminderInvalid
	}
	if in.MinAgeMinutes < 0 || in.MinAgeMinutes > 43200 {
		return ScheduledReminderInput{}, time.Time{}, ErrScheduledReminderInvalid
	}
	if !in.ConditionReviewRequested && !in.ConditionTeamReviewRequested {
		return ScheduledReminderInput{}, time.Time{}, ErrScheduledReminderInvalid
	}
	if err := validateScheduledReminderTarget(ctx, deps, orgID, in); err != nil {
		return ScheduledReminderInput{}, time.Time{}, err
	}
	if err := ensureScheduledReminderPrivateTargetEntitled(ctx, deps, orgID, in); err != nil {
		return ScheduledReminderInput{}, time.Time{}, err
	}
	next, err := nextScheduledReminderTick(in.CronExpr, in.Timezone, from)
	if err != nil {
		return ScheduledReminderInput{}, time.Time{}, fmt.Errorf("%w: %v", ErrScheduledReminderInvalid, err)
	}
	return in, next, nil
}

func validateScheduledReminderTarget(ctx context.Context, deps Deps, orgID int64, in ScheduledReminderInput) error {
	q := orgsdb.New()
	switch in.Target {
	case orgsdb.OrgScheduledReminderTargetAllRepositories:
		if in.RepoID != 0 || in.TeamID != 0 {
			return ErrScheduledReminderInvalid
		}
		return nil
	case orgsdb.OrgScheduledReminderTargetRepository:
		if in.RepoID == 0 || in.TeamID != 0 {
			return ErrScheduledReminderInvalid
		}
		repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, in.RepoID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduledReminderTarget
		}
		if err != nil {
			return err
		}
		if !repo.OwnerOrgID.Valid || repo.OwnerOrgID.Int64 != orgID || repo.DeletedAt.Valid {
			return ErrScheduledReminderTarget
		}
		return nil
	case orgsdb.OrgScheduledReminderTargetTeam:
		if in.TeamID == 0 || in.RepoID != 0 {
			return ErrScheduledReminderInvalid
		}
		team, err := q.GetTeamByID(ctx, deps.Pool, in.TeamID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduledReminderTarget
		}
		if err != nil {
			return err
		}
		if team.OrgID != orgID {
			return ErrScheduledReminderTarget
		}
		return nil
	default:
		return ErrScheduledReminderInvalid
	}
}

func ensureScheduledReminderPrivateTargetEntitled(ctx context.Context, deps Deps, orgID int64, in ScheduledReminderInput) error {
	count, err := orgsdb.New().CountPrivateReposForScheduledReminderTarget(ctx, deps.Pool, orgsdb.CountPrivateReposForScheduledReminderTargetParams{
		OwnerOrgID: pgtype.Int8{Int64: orgID, Valid: true},
		Target:     in.Target,
		RepoID:     nullableID(in.RepoID),
		TeamID:     nullableID(in.TeamID),
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: deps.Pool}, orgID, entitlements.FeatureScheduledReminders)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return ErrScheduledReminderRequiresTeam
	}
	return nil
}

func nextScheduledReminderTick(expr, timezone string, from time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	sched, err := cronworkflow.ParseExpr(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from.In(loc)).UTC(), nil
}

func sweepOneScheduledReminder(ctx context.Context, deps ScheduledReminderSweepDeps, row orgsdb.OrgScheduledReminder, now time.Time) {
	status := orgsdb.OrgScheduledReminderStatusSkipped
	var errText pgtype.Text

	delivered, err := deliverScheduledReminder(ctx, deps, row)
	if err != nil {
		status = orgsdb.OrgScheduledReminderStatusError
		errText = pgtype.Text{String: err.Error(), Valid: true}
	} else if delivered > 0 {
		status = orgsdb.OrgScheduledReminderStatusSent
	}

	next, nextErr := nextScheduledReminderTick(row.CronExpr, row.Timezone, now)
	if nextErr != nil {
		status = orgsdb.OrgScheduledReminderStatusError
		errText = pgtype.Text{String: nextErr.Error(), Valid: true}
		if err := orgsdb.New().AdvanceOrgScheduledReminder(ctx, deps.Pool, orgsdb.AdvanceOrgScheduledReminderParams{
			ID:            row.ID,
			NextRunAt:     row.NextRunAt,
			LastRunStatus: status,
			LastRunError:  errText,
		}); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "scheduled reminders: record invalid stored schedule failed",
				"schedule_id", row.ID, "error", err)
		}
		if err := orgsdb.New().PauseOrgScheduledReminder(ctx, deps.Pool, orgsdb.PauseOrgScheduledReminderParams{
			ID:    row.ID,
			OrgID: row.OrgID,
		}); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "scheduled reminders: pause invalid stored schedule failed",
				"schedule_id", row.ID, "error", err)
		}
		return
	}
	if err := orgsdb.New().AdvanceOrgScheduledReminder(ctx, deps.Pool, orgsdb.AdvanceOrgScheduledReminderParams{
		ID:            row.ID,
		NextRunAt:     timestamptz(next),
		LastRunStatus: status,
		LastRunError:  errText,
	}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "scheduled reminders: advance failed",
			"schedule_id", row.ID, "error", err)
	}
}

func deliverScheduledReminder(ctx context.Context, deps ScheduledReminderSweepDeps, row orgsdb.OrgScheduledReminder) (int, error) {
	candidates, err := scheduledReminderCandidates(ctx, deps, row)
	if err != nil {
		return 0, err
	}
	delivered := 0
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		key := fmt.Sprintf("%d/%d", c.prIssueID, c.recipientUserID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		if c.recipientSuspended {
			continue
		}
		actor := policy.UserActor(c.recipientUserID, c.recipientUsername, c.recipientSuspended, c.recipientSiteAdmin)
		if dec := policy.Can(ctx, policy.Deps{Pool: deps.Pool}, actor, policy.ActionRepoRead, c.repoRef()); !dec.Allow {
			continue
		}
		ok, err := deliverScheduledReminderNotification(ctx, deps, row, c)
		if err != nil {
			return delivered, err
		}
		if ok {
			delivered++
		}
	}
	return delivered, nil
}

func deliverScheduledReminderNotification(ctx context.Context, deps ScheduledReminderSweepDeps, row orgsdb.OrgScheduledReminder, c scheduledReminderCandidate) (bool, error) {
	tx, err := deps.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := orgsdb.New()
	if _, err := q.ClaimOrgScheduledReminderDelivery(ctx, tx, orgsdb.ClaimOrgScheduledReminderDeliveryParams{
		ScheduleID:      row.ID,
		RunKey:          row.NextRunAt,
		PrIssueID:       c.prIssueID,
		RecipientUserID: c.recipientUserID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	notification, err := notifdb.New().UpsertNotificationByThread(ctx, tx, notifdb.UpsertNotificationByThreadParams{
		RecipientUserID: c.recipientUserID,
		Kind:            ScheduledReminderNotificationKind,
		Reason:          ScheduledReminderNotificationReason,
		RepoID:          pgtype.Int8{Int64: c.repoID, Valid: true},
		ThreadKind: notifdb.NullNotificationThreadKind{
			NotificationThreadKind: notifdb.NotificationThreadKindPr,
			Valid:                  true,
		},
		ThreadID: pgtype.Int8{Int64: c.prIssueID, Valid: true},
	})
	if err != nil {
		return false, err
	}
	if err := q.MarkOrgScheduledReminderDeliverySent(ctx, tx, orgsdb.MarkOrgScheduledReminderDeliverySentParams{
		ScheduleID:      row.ID,
		RunKey:          row.NextRunAt,
		PrIssueID:       c.prIssueID,
		RecipientUserID: c.recipientUserID,
		NotificationID:  pgtype.Int8{Int64: notification.ID, Valid: true},
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type scheduledReminderCandidate struct {
	prIssueID          int64
	prNumber           int64
	prTitle            string
	authorUserID       int64
	repoID             int64
	ownerUserID        int64
	ownerOrgID         int64
	repoVisibility     string
	isArchived         bool
	isDeleted          bool
	isPaused           bool
	recipientUserID    int64
	recipientUsername  string
	recipientSuspended bool
	recipientSiteAdmin bool
}

func (c scheduledReminderCandidate) repoRef() policy.RepoRef {
	return policy.RepoRef{
		ID:           c.repoID,
		OwnerUserID:  c.ownerUserID,
		OwnerOrgID:   c.ownerOrgID,
		Visibility:   c.repoVisibility,
		IsArchived:   c.isArchived,
		IsDeleted:    c.isDeleted,
		IsPaused:     c.isPaused,
		AuthorUserID: c.authorUserID,
	}
}

func scheduledReminderCandidates(ctx context.Context, deps ScheduledReminderSweepDeps, row orgsdb.OrgScheduledReminder) ([]scheduledReminderCandidate, error) {
	q := orgsdb.New()
	var out []scheduledReminderCandidate
	if row.ConditionReviewRequested {
		rows, err := q.ListDirectReviewReminderCandidates(ctx, deps.Pool, row.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, scheduledReminderCandidate{
				prIssueID:          r.PrIssueID,
				prNumber:           r.PrNumber,
				prTitle:            r.PrTitle,
				authorUserID:       int64FromPg(r.AuthorUserID),
				repoID:             r.RepoID,
				ownerUserID:        int64FromPg(r.OwnerUserID),
				ownerOrgID:         int64FromPg(r.OwnerOrgID),
				repoVisibility:     r.RepoVisibility,
				isArchived:         r.IsArchived,
				isDeleted:          r.DeletedAt.Valid,
				isPaused:           r.IsPaused,
				recipientUserID:    r.RecipientUserID,
				recipientUsername:  r.RecipientUsername,
				recipientSuspended: r.RecipientSuspended,
				recipientSiteAdmin: r.RecipientSiteAdmin,
			})
		}
	}
	if row.ConditionTeamReviewRequested {
		rows, err := q.ListTeamReviewReminderCandidates(ctx, deps.Pool, row.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, scheduledReminderCandidate{
				prIssueID:          r.PrIssueID,
				prNumber:           r.PrNumber,
				prTitle:            r.PrTitle,
				authorUserID:       int64FromPg(r.AuthorUserID),
				repoID:             r.RepoID,
				ownerUserID:        int64FromPg(r.OwnerUserID),
				ownerOrgID:         int64FromPg(r.OwnerOrgID),
				repoVisibility:     r.RepoVisibility,
				isArchived:         r.IsArchived,
				isDeleted:          r.DeletedAt.Valid,
				isPaused:           r.IsPaused,
				recipientUserID:    r.RecipientUserID,
				recipientUsername:  r.RecipientUsername,
				recipientSuspended: r.RecipientSuspended,
				recipientSiteAdmin: r.RecipientSiteAdmin,
			})
		}
	}
	return out, nil
}

func scheduledReminderListView(row orgsdb.ListOrgScheduledRemindersRow) ScheduledReminderView {
	return ScheduledReminderView{
		ID:                           row.ID,
		OrgID:                        row.OrgID,
		Name:                         row.Name,
		Target:                       row.Target,
		RepoID:                       int64FromPg(row.RepoID),
		TeamID:                       int64FromPg(row.TeamID),
		RepoName:                     row.RepoName,
		TeamSlug:                     row.TeamSlug,
		TeamDisplayName:              row.TeamDisplayName,
		CronExpr:                     row.CronExpr,
		Timezone:                     row.Timezone,
		NextRunAt:                    timeFromPg(row.NextRunAt),
		LastRunAt:                    timeFromPg(row.LastRunAt),
		LastRunStatus:                row.LastRunStatus,
		LastRunError:                 textFromPg(row.LastRunError),
		ConditionReviewRequested:     row.ConditionReviewRequested,
		ConditionTeamReviewRequested: row.ConditionTeamReviewRequested,
		MinAgeMinutes:                row.MinAgeMinutes,
		Paused:                       row.PausedAt.Valid,
		CreatedAt:                    timeFromPg(row.CreatedAt),
		UpdatedAt:                    timeFromPg(row.UpdatedAt),
	}
}

func nullableID(id int64) pgtype.Int8 {
	if id == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: id, Valid: true}
}

func int64FromPg(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func textFromPg(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func timeFromPg(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time
}
