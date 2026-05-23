// SPDX-License-Identifier: AGPL-3.0-or-later

// Package actionspolicy owns Actions-specific runtime policy: whether a
// matched workflow may be queued, whether it must pause for approval, and the
// abuse caps that keep a shared runner pool from being monopolized.
package actionspolicy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	authpolicy "github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

var (
	ErrActionsDisabled = errors.New("actions policy: actions disabled")
	ErrRepoQueuedCap   = errors.New("actions policy: repo queued-run cap reached")
	ErrActorRateLimit  = errors.New("actions policy: actor trigger rate limit reached")
	ErrActorRequired   = errors.New("actions policy: actor required")
	ErrActorNotFound   = errors.New("actions policy: actor not found")
	ErrUnauthorized    = errors.New("actions policy: actor cannot run actions")
)

// Deps wires policy evaluation to storage and auth policy role lookups.
type Deps struct {
	Pool *pgxpool.Pool
}

// TriggerRequest describes one matched workflow about to become a run.
type TriggerRequest struct {
	Repo        reposdb.Repo
	EventKind   string
	ActorUserID int64
	Now         time.Time
}

// TriggerDecision is the dispatch posture for one matched workflow.
type TriggerDecision struct {
	Allow          bool
	NeedApproval   bool
	ApprovalReason string
	Reason         string
	Policy         actionsdb.GetEffectiveActionsPolicyForRepoRow
}

// EvaluateTrigger applies site/org/repo Actions policy, auth policy, and
// enqueue-time abuse caps. It never grants repository privileges itself:
// push/dispatch/same-repo trusted PR execution flows through
// internal/auth/policy.ActionActionsRun. Untrusted PRs can only be queued in
// a paused approval-required state.
func EvaluateTrigger(ctx context.Context, deps Deps, req TriggerRequest) (TriggerDecision, error) {
	if deps.Pool == nil {
		return TriggerDecision{}, errors.New("actions policy: nil Pool")
	}
	q := actionsdb.New()
	pol, err := q.GetEffectiveActionsPolicyForRepo(ctx, deps.Pool, req.Repo.ID)
	if err != nil {
		return TriggerDecision{}, err
	}
	base := TriggerDecision{Policy: pol}
	if !pol.ActionsEnabled {
		base.Reason = "actions disabled by policy"
		return base, ErrActionsDisabled
	}
	if req.Repo.DeletedAt.Valid {
		base.Reason = "repo deleted"
		return base, ErrUnauthorized
	}
	if req.Repo.IsArchived {
		base.Reason = "repo archived"
		return base, ErrUnauthorized
	}
	if err := checkEnqueueCaps(ctx, q, deps.Pool, req, pol); err != nil {
		base.Reason = err.Error()
		return base, err
	}

	if req.EventKind == "schedule" {
		base.Allow = true
		base.Reason = "schedule trigger"
		return base, nil
	}
	if req.ActorUserID == 0 {
		base.Reason = "actor required"
		return base, ErrActorRequired
	}
	uq := usersdb.New()
	user, err := uq.GetUserByID(ctx, deps.Pool, req.ActorUserID)
	if err != nil {
		base.Reason = "actor not found"
		return base, fmt.Errorf("%w: %w", ErrActorNotFound, err)
	}
	has2FA, err := uq.HasConfirmedUserTOTP(ctx, deps.Pool, user.ID)
	if err != nil {
		base.Reason = "actor 2fa lookup failed"
		return base, err
	}
	actor := authpolicy.UserActorWithTwoFactor(user.ID, user.Username, user.SuspendedAt.Valid, user.IsSiteAdmin, has2FA)
	authz := authpolicy.Can(ctx, authpolicy.Deps{Pool: deps.Pool}, actor, authpolicy.ActionActionsRun, authpolicy.NewRepoRefFromRepo(req.Repo))
	if authz.Allow {
		base.Allow = true
		base.Reason = "actor can run actions"
		return base, nil
	}
	if user.SuspendedAt.Valid {
		base.Reason = "actor suspended"
		return base, ErrUnauthorized
	}
	if req.EventKind == "pull_request" {
		base.Allow = true
		if pol.RequirePrApproval {
			base.NeedApproval = true
			base.ApprovalReason = "Pull request workflow requires maintainer approval before runner dispatch."
			base.Reason = "pull request requires approval"
		} else {
			base.Reason = "pull request approval disabled by policy"
		}
		return base, nil
	}
	base.Reason = authz.Reason
	return base, ErrUnauthorized
}

func checkEnqueueCaps(
	ctx context.Context,
	q *actionsdb.Queries,
	db actionsdb.DBTX,
	req TriggerRequest,
	pol actionsdb.GetEffectiveActionsPolicyForRepoRow,
) error {
	queued, err := q.CountQueuedWorkflowRunsForRepo(ctx, db, req.Repo.ID)
	if err != nil {
		return err
	}
	if queued >= int64(pol.MaxRepoQueuedRuns) {
		return ErrRepoQueuedCap
	}
	if req.ActorUserID == 0 {
		return nil
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	recent, err := q.CountRecentWorkflowRunsForActor(ctx, db, actionsdb.CountRecentWorkflowRunsForActorParams{
		ActorUserID: pgtype.Int8{Int64: req.ActorUserID, Valid: true},
		Since:       pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	})
	if err != nil {
		return err
	}
	if recent >= int64(pol.ActorTriggerLimitPerHour) {
		return ErrActorRateLimit
	}
	return nil
}
