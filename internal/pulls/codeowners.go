// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/codeowners"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func requestCodeOwners(ctx context.Context, deps Deps, gitDir string, repoID, prIssueID, requestedByUserID int64, baseOID string) error {
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return err
	}
	allowed, err := codeOwnersAllowed(ctx, deps, repo)
	if err != nil || !allowed {
		return err
	}
	ownersFile, ok, err := codeowners.Load(ctx, gitDir, baseOID)
	if err != nil || !ok {
		return err
	}
	files, err := pullsdb.New().ListPullRequestFiles(ctx, deps.Pool, prIssueID)
	if err != nil {
		return err
	}
	userTargets := map[int64]struct{}{}
	teamTargets := map[int64]struct{}{}
	for _, file := range files {
		entry, matched := ownersFile.OwnersFor(file.Path)
		if !matched || len(entry.Owners) == 0 {
			continue
		}
		targets, err := review.ResolveCodeOwnerTargets(ctx, deps.Pool, repo, entry.Owners)
		if err != nil {
			return err
		}
		for id := range targets.UserIDs {
			userTargets[id] = struct{}{}
		}
		for id := range targets.TeamIDs {
			teamTargets[id] = struct{}{}
		}
	}
	rdeps := review.Deps{Pool: deps.Pool, Logger: deps.Logger}
	for userID := range userTargets {
		if userID == requestedByUserID {
			continue
		}
		if err := requestReviewerTarget(ctx, rdeps, prIssueID, requestedByUserID, userID, 0); err != nil {
			return err
		}
	}
	for teamID := range teamTargets {
		if err := requestReviewerTarget(ctx, rdeps, prIssueID, requestedByUserID, 0, teamID); err != nil {
			return err
		}
	}
	return nil
}

func requestReviewerTarget(ctx context.Context, deps review.Deps, prIssueID, requestedByUserID, userID, teamID int64) error {
	_, err := review.Request(ctx, deps, review.RequestParams{
		PRIssueID:         prIssueID,
		RequestedUserID:   userID,
		RequestedTeamID:   teamID,
		RequestedByUserID: requestedByUserID,
	})
	if err == nil || errors.Is(err, review.ErrReviewerAlreadyPending) {
		return nil
	}
	if errors.Is(err, review.ErrReviewerLimitReached) {
		return nil
	}
	return err
}

func codeOwnersAllowed(ctx context.Context, deps Deps, repo reposdb.Repo) (bool, error) {
	if repo.Visibility != reposdb.RepoVisibilityPrivate {
		return true, nil
	}
	principal, ok := repoBillingPrincipal(repo)
	if !ok {
		return false, nil
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx, entitlements.Deps{Pool: deps.Pool}, principal, entitlements.FeatureCodeOwnersReview)
	if err != nil {
		return false, err
	}
	return decision.Allowed, nil
}

func repoBillingPrincipal(repo reposdb.Repo) (billing.Principal, bool) {
	if repo.OwnerOrgID.Valid {
		return billing.PrincipalForOrg(repo.OwnerOrgID.Int64), true
	}
	if repo.OwnerUserID.Valid {
		return billing.PrincipalForUser(repo.OwnerUserID.Int64), true
	}
	return billing.Principal{}, false
}

func maybeRequestCodeOwners(ctx context.Context, deps Deps, gitDir string, repoID, prIssueID, requestedByUserID int64, baseOID string) {
	if err := requestCodeOwners(ctx, deps, gitDir, repoID, prIssueID, requestedByUserID, baseOID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "pulls: codeowners review requests", "error", fmt.Sprintf("%v", err), "pr_id", prIssueID)
		}
	}
}
