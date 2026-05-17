// SPDX-License-Identifier: AGPL-3.0-or-later

package issues

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// MilestoneCreateParams is input for CreateMilestone.
type MilestoneCreateParams struct {
	RepoID      int64
	Title       string
	Description string
	DueOn       *time.Time
}

func CreateMilestone(ctx context.Context, deps Deps, p MilestoneCreateParams) (issuesdb.Milestone, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" || len(title) > 200 {
		return issuesdb.Milestone{}, errors.New("issues: milestone title length 1–200")
	}
	due := pgtype.Timestamptz{}
	if p.DueOn != nil {
		due = pgtype.Timestamptz{Time: *p.DueOn, Valid: true}
	}
	q := issuesdb.New()
	row, err := q.CreateMilestone(ctx, deps.Pool, issuesdb.CreateMilestoneParams{
		RepoID: p.RepoID, Title: title, Description: p.Description, DueOn: due,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return issuesdb.Milestone{}, ErrMilestoneExists
		}
		return issuesdb.Milestone{}, err
	}
	return row, nil
}

type MilestoneUpdateParams struct {
	ID          int64
	Title       string
	Description string
	DueOn       *time.Time
}

func UpdateMilestone(ctx context.Context, deps Deps, p MilestoneUpdateParams) error {
	title := strings.TrimSpace(p.Title)
	if title == "" || len(title) > 200 {
		return errors.New("issues: milestone title length 1–200")
	}
	due := pgtype.Timestamptz{}
	if p.DueOn != nil {
		due = pgtype.Timestamptz{Time: *p.DueOn, Valid: true}
	}
	q := issuesdb.New()
	if err := q.UpdateMilestone(ctx, deps.Pool, issuesdb.UpdateMilestoneParams{
		ID: p.ID, Title: title, Description: p.Description, DueOn: due,
	}); err != nil {
		if isUniqueViolation(err) {
			return ErrMilestoneExists
		}
		return err
	}
	return nil
}

func SetMilestoneState(ctx context.Context, deps Deps, id int64, state string) error {
	if state != "open" && state != "closed" {
		return errors.New("issues: milestone state must be open or closed")
	}
	q := issuesdb.New()
	return q.SetMilestoneState(ctx, deps.Pool, issuesdb.SetMilestoneStateParams{
		ID: id, State: issuesdb.MilestoneState(state),
	})
}

func DeleteMilestone(ctx context.Context, deps Deps, id int64) error {
	q := issuesdb.New()
	return q.DeleteMilestone(ctx, deps.Pool, id)
}

// AssignMilestone sets an issue's milestone (or clears with milestoneID==0)
// and emits a `milestoned`/`demilestoned` event.
func AssignMilestone(ctx context.Context, deps Deps, actorUserID, issueID, milestoneID int64) error {
	q := issuesdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	mid := pgtype.Int8{Int64: milestoneID, Valid: milestoneID != 0}
	if err := q.SetIssueMilestone(ctx, tx, issuesdb.SetIssueMilestoneParams{
		ID: issueID, MilestoneID: mid,
	}); err != nil {
		return err
	}
	kind := "milestoned"
	mval := strconv.FormatInt(milestoneID, 10)
	if milestoneID == 0 {
		kind = "demilestoned"
		mval = "null"
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        kind,
		Meta:        []byte(`{"milestone_id":` + mval + `}`),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// AssignUser adds an assignee + emits an `assigned` event.
func AssignUser(ctx context.Context, deps Deps, actorUserID, issueID, userID int64) error {
	q := issuesdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	exists, err := q.IssueAssigneeExists(ctx, tx, issuesdb.IssueAssigneeExistsParams{
		IssueID: issueID,
		UserID:  userID,
	})
	if err != nil {
		return err
	}
	if !exists {
		count, err := q.CountIssueAssignees(ctx, tx, issueID)
		if err != nil {
			return err
		}
		if count >= 1 {
			issue, err := q.GetIssueByID(ctx, tx, issueID)
			if err != nil {
				return err
			}
			repo, err := reposdb.New().GetRepoByID(ctx, tx, issue.RepoID)
			if err != nil {
				return err
			}
			if repo.OwnerOrgID.Valid && repo.Visibility == reposdb.RepoVisibilityPrivate {
				decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: deps.Pool}, repo.OwnerOrgID.Int64, entitlements.FeatureMultipleAssignees)
				if err != nil {
					return err
				}
				if !decision.Allowed {
					return ErrMultipleAssigneesRequireTeam
				}
			}
		}
	}
	if err := q.AssignUserToIssue(ctx, tx, issuesdb.AssignUserToIssueParams{
		IssueID: issueID, UserID: userID,
		AssignedByUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
	}); err != nil {
		return err
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        "assigned",
		Meta:        []byte(`{"user_id":` + strconv.FormatInt(userID, 10) + `}`),
	}); err != nil {
		return err
	}
	// S29: domain event so the fan-out worker can route an
	// `assignment` notification to the assignee.
	issue, _ := q.GetIssueByID(ctx, tx, issueID)
	repoVis, _ := repoVisibilityPublic(ctx, tx, issue.RepoID)
	if err := emitAssignmentEventTx(ctx, tx, issue, actorUserID, userID, repoVis); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// UnassignUser removes an assignee + emits an `unassigned` event.
func UnassignUser(ctx context.Context, deps Deps, actorUserID, issueID, userID int64) error {
	q := issuesdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := q.UnassignUserFromIssue(ctx, tx, issuesdb.UnassignUserFromIssueParams{
		IssueID: issueID, UserID: userID,
	}); err != nil {
		return err
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        "unassigned",
		Meta:        []byte(`{"user_id":` + strconv.FormatInt(userID, 10) + `}`),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
