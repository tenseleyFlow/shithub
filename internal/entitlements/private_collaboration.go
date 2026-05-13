// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

var ErrPrivateCollaborationLimitExceeded = errors.New("entitlements: private collaboration limit exceeded")

type PrivateCollaborationExpansion struct {
	CandidateUserIDs    []int64
	AnonymousCandidates int64
}

type PrivateCollaborationUsage struct {
	OrgID        int64
	Count        int64
	Limit        int64
	Unlimited    bool
	RequiredPlan billing.Plan
	Reason       Reason
}

type PrivateCollaborationCheck struct {
	Allowed      bool
	Usage        PrivateCollaborationUsage
	Added        int64
	WouldUse     int64
	RequiredPlan billing.Plan
	Reason       Reason
}

func PrivateCollaborationUsageForOrg(ctx context.Context, deps Deps, orgID int64) (PrivateCollaborationUsage, error) {
	usage, _, err := privateCollaborationUsageWithIDs(ctx, deps, orgID)
	return usage, err
}

func CheckPrivateCollaborationExpansion(ctx context.Context, deps Deps, orgID int64, expansion PrivateCollaborationExpansion) (PrivateCollaborationCheck, error) {
	usage, current, err := privateCollaborationUsageWithIDs(ctx, deps, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	check := PrivateCollaborationCheck{
		Allowed:      true,
		Usage:        usage,
		WouldUse:     usage.Count,
		RequiredPlan: usage.RequiredPlan,
		Reason:       usage.Reason,
	}
	if usage.Unlimited {
		return check, nil
	}
	added := expansion.AnonymousCandidates
	if added < 0 {
		added = 0
	}
	for _, userID := range expansion.CandidateUserIDs {
		if userID == 0 {
			continue
		}
		if _, ok := current[userID]; ok {
			continue
		}
		current[userID] = struct{}{}
		added++
	}
	check.Added = added
	check.WouldUse = usage.Count + added
	if added == 0 {
		return check, nil
	}
	if check.WouldUse > usage.Limit {
		check.Allowed = false
	}
	return check, nil
}

func CheckPrivateRepositoryCreation(ctx context.Context, deps Deps, orgID int64) (PrivateCollaborationCheck, error) {
	usage, _, err := privateCollaborationUsageWithIDs(ctx, deps, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	check := PrivateCollaborationCheck{
		Allowed:      true,
		Usage:        usage,
		WouldUse:     usage.Count,
		RequiredPlan: usage.RequiredPlan,
		Reason:       usage.Reason,
	}
	if usage.Unlimited {
		return check, nil
	}
	if usage.Count > usage.Limit {
		check.Allowed = false
		return check, nil
	}
	if usage.Count > 0 {
		return check, nil
	}
	owners, err := orgOwnerIDs(ctx, deps.Pool, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: owners})
}

func CheckRepoPrivateVisibility(ctx context.Context, deps Deps, orgID, repoID int64) (PrivateCollaborationCheck, error) {
	usage, _, err := privateCollaborationUsageWithIDs(ctx, deps, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if !usage.Unlimited && usage.Count > usage.Limit {
		return PrivateCollaborationCheck{
			Allowed:      false,
			Usage:        usage,
			WouldUse:     usage.Count,
			RequiredPlan: usage.RequiredPlan,
			Reason:       usage.Reason,
		}, nil
	}
	candidates, err := repoPrivateCollaboratorCandidateIDs(ctx, deps.Pool, orgID, repoID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: candidates})
}

func CheckOrgOwnerPrivateCollaboration(ctx context.Context, deps Deps, orgID, userID int64) (PrivateCollaborationCheck, error) {
	hasPrivate, err := orgHasPrivateRepos(ctx, deps.Pool, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if !hasPrivate {
		return allowedPrivateCollaborationCheck(ctx, deps, orgID)
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: []int64{userID}})
}

func CheckPrivateInvitationSlot(ctx context.Context, deps Deps, orgID int64) (PrivateCollaborationCheck, error) {
	hasPrivate, err := orgHasPrivateRepos(ctx, deps.Pool, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if !hasPrivate {
		return allowedPrivateCollaborationCheck(ctx, deps, orgID)
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{AnonymousCandidates: 1})
}

func CheckDirectPrivateCollaborator(ctx context.Context, deps Deps, repoID, userID int64) (PrivateCollaborationCheck, error) {
	orgID, private, err := orgRepoPrivateState(ctx, deps.Pool, repoID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if orgID == 0 || !private {
		return allowedPrivateCollaborationCheck(ctx, deps, orgID)
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: []int64{userID}})
}

func CheckTeamMemberPrivateCollaboration(ctx context.Context, deps Deps, teamID, userID int64) (PrivateCollaborationCheck, error) {
	orgID, hasPrivateAccess, err := teamPrivateRepoAccessState(ctx, deps.Pool, teamID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if !hasPrivateAccess {
		return allowedPrivateCollaborationCheck(ctx, deps, orgID)
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: []int64{userID}})
}

func CheckTeamPrivateRepoGrant(ctx context.Context, deps Deps, teamID, repoID int64) (PrivateCollaborationCheck, error) {
	orgID, private, err := orgRepoPrivateState(ctx, deps.Pool, repoID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	if orgID == 0 || !private {
		return allowedPrivateCollaborationCheck(ctx, deps, orgID)
	}
	candidates, err := teamGrantCandidateIDs(ctx, deps.Pool, teamID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	return CheckPrivateCollaborationExpansion(ctx, deps, orgID, PrivateCollaborationExpansion{CandidateUserIDs: candidates})
}

func (c PrivateCollaborationCheck) Err() error {
	if c.Allowed {
		return nil
	}
	return ErrPrivateCollaborationLimitExceeded
}

func (c PrivateCollaborationCheck) Message() string {
	if c.Allowed || c.Usage.Unlimited {
		return ""
	}
	return fmt.Sprintf("Free organizations can have up to %d private collaborators. This change would use %d. Upgrade to Team to add more.", c.Usage.Limit, c.WouldUse)
}

func (c PrivateCollaborationCheck) BillingPath(orgSlug string) string {
	return "/organizations/" + url.PathEscape(orgSlug) + "/settings/billing"
}

func (c PrivateCollaborationCheck) UpgradeBanner(orgSlug string) UpgradeBanner {
	return UpgradeBanner{
		Message:    c.Message(),
		ActionText: "Manage billing and plans",
		ActionHref: c.BillingPath(orgSlug),
		StatusCode: c.HTTPStatus(),
	}
}

func (c PrivateCollaborationCheck) HTTPStatus() int {
	if c.Allowed {
		return 200
	}
	return 402
}

func allowedPrivateCollaborationCheck(ctx context.Context, deps Deps, orgID int64) (PrivateCollaborationCheck, error) {
	if orgID == 0 {
		return PrivateCollaborationCheck{Allowed: true}, nil
	}
	usage, err := PrivateCollaborationUsageForOrg(ctx, deps, orgID)
	if err != nil {
		return PrivateCollaborationCheck{}, err
	}
	return PrivateCollaborationCheck{
		Allowed:      true,
		Usage:        usage,
		WouldUse:     usage.Count,
		RequiredPlan: usage.RequiredPlan,
		Reason:       usage.Reason,
	}, nil
}

func privateCollaborationUsageWithIDs(ctx context.Context, deps Deps, orgID int64) (PrivateCollaborationUsage, map[int64]struct{}, error) {
	if deps.Pool == nil {
		return PrivateCollaborationUsage{}, nil, ErrPoolRequired
	}
	if orgID == 0 {
		return PrivateCollaborationUsage{}, nil, ErrOrgIDRequired
	}
	set, err := ForOrg(ctx, deps, orgID)
	if err != nil {
		return PrivateCollaborationUsage{}, nil, err
	}
	limit, err := set.Limit(LimitOrgPrivateCollaboration)
	if err != nil {
		return PrivateCollaborationUsage{}, nil, err
	}
	ids, err := currentPrivateCollaboratorIDs(ctx, deps.Pool, orgID)
	if err != nil {
		return PrivateCollaborationUsage{}, nil, err
	}
	return PrivateCollaborationUsage{
		OrgID:        orgID,
		Count:        int64(len(ids)),
		Limit:        limit.Value,
		Unlimited:    limit.Unlimited,
		RequiredPlan: limit.RequiredPlan,
		Reason:       limit.Reason,
	}, ids, nil
}

func currentPrivateCollaboratorIDs(ctx context.Context, pool *pgxpool.Pool, orgID int64) (map[int64]struct{}, error) {
	return queryIDSet(ctx, pool, `
WITH private_repos AS (
    SELECT id
      FROM repos
     WHERE owner_org_id = $1
       AND visibility = 'private'
       AND deleted_at IS NULL
),
granting_teams AS (
    SELECT DISTINCT tra.team_id
      FROM team_repo_access tra
      JOIN teams t ON t.id = tra.team_id AND t.org_id = $1
      JOIN private_repos pr ON pr.id = tra.repo_id
)
SELECT DISTINCT user_id
  FROM (
        SELECT om.user_id
          FROM org_members om
         WHERE om.org_id = $1
           AND om.role = 'owner'
           AND EXISTS (SELECT 1 FROM private_repos)
        UNION
        SELECT rc.user_id
          FROM repo_collaborators rc
          JOIN private_repos pr ON pr.id = rc.repo_id
        UNION
        SELECT tm.user_id
          FROM team_members tm
          JOIN teams member_team ON member_team.id = tm.team_id AND member_team.org_id = $1
          JOIN granting_teams gt ON gt.team_id = member_team.id OR gt.team_id = member_team.parent_team_id
       ) effective
 WHERE user_id IS NOT NULL`, orgID)
}

func repoPrivateCollaboratorCandidateIDs(ctx context.Context, pool *pgxpool.Pool, orgID, repoID int64) ([]int64, error) {
	ids, err := queryIDSet(ctx, pool, `
WITH granting_teams AS (
    SELECT DISTINCT tra.team_id
      FROM team_repo_access tra
      JOIN teams t ON t.id = tra.team_id AND t.org_id = $1
     WHERE tra.repo_id = $2
)
SELECT DISTINCT user_id
  FROM (
        SELECT om.user_id
          FROM org_members om
         WHERE om.org_id = $1
           AND om.role = 'owner'
        UNION
        SELECT rc.user_id
          FROM repo_collaborators rc
         WHERE rc.repo_id = $2
        UNION
        SELECT tm.user_id
          FROM team_members tm
          JOIN teams member_team ON member_team.id = tm.team_id AND member_team.org_id = $1
          JOIN granting_teams gt ON gt.team_id = member_team.id OR gt.team_id = member_team.parent_team_id
       ) effective
 WHERE user_id IS NOT NULL`, orgID, repoID)
	if err != nil {
		return nil, err
	}
	return idSetToSlice(ids), nil
}

func teamGrantCandidateIDs(ctx context.Context, pool *pgxpool.Pool, teamID int64) ([]int64, error) {
	ids, err := queryIDSet(ctx, pool, `
SELECT DISTINCT tm.user_id
  FROM teams grant_team
  JOIN teams member_team ON member_team.id = grant_team.id OR member_team.parent_team_id = grant_team.id
  JOIN team_members tm ON tm.team_id = member_team.id
 WHERE grant_team.id = $1`, teamID)
	if err != nil {
		return nil, err
	}
	return idSetToSlice(ids), nil
}

func orgOwnerIDs(ctx context.Context, pool *pgxpool.Pool, orgID int64) ([]int64, error) {
	ids, err := queryIDSet(ctx, pool, `SELECT user_id FROM org_members WHERE org_id = $1 AND role = 'owner'`, orgID)
	if err != nil {
		return nil, err
	}
	return idSetToSlice(ids), nil
}

func orgHasPrivateRepos(ctx context.Context, pool *pgxpool.Pool, orgID int64) (bool, error) {
	if pool == nil {
		return false, ErrPoolRequired
	}
	var exists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repos WHERE owner_org_id = $1 AND visibility = 'private' AND deleted_at IS NULL)`, orgID).Scan(&exists)
	return exists, err
}

func orgRepoPrivateState(ctx context.Context, pool *pgxpool.Pool, repoID int64) (int64, bool, error) {
	if pool == nil {
		return 0, false, ErrPoolRequired
	}
	var ownerOrgID pgtype.Int8
	var visibility string
	err := pool.QueryRow(ctx, `SELECT owner_org_id, visibility::text FROM repos WHERE id = $1 AND deleted_at IS NULL`, repoID).Scan(&ownerOrgID, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !ownerOrgID.Valid {
		return 0, visibility == "private", nil
	}
	return ownerOrgID.Int64, visibility == "private", nil
}

func teamPrivateRepoAccessState(ctx context.Context, pool *pgxpool.Pool, teamID int64) (int64, bool, error) {
	if pool == nil {
		return 0, false, ErrPoolRequired
	}
	var orgID int64
	var hasPrivateAccess bool
	err := pool.QueryRow(ctx, `
SELECT t.org_id,
       EXISTS(
           SELECT 1
             FROM team_repo_access tra
             JOIN repos r ON r.id = tra.repo_id
            WHERE r.owner_org_id = t.org_id
              AND r.visibility = 'private'
              AND r.deleted_at IS NULL
              AND (tra.team_id = t.id OR tra.team_id = t.parent_team_id)
       )
  FROM teams t
 WHERE t.id = $1`, teamID).Scan(&orgID, &hasPrivateAccess)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return orgID, hasPrivateAccess, err
}

func queryIDSet(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (map[int64]struct{}, error) {
	if pool == nil {
		return nil, ErrPoolRequired
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func idSetToSlice(ids map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out
}
