// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/codeowners"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

type CodeOwnerTargets struct {
	UserIDs map[int64]struct{}
	TeamIDs map[int64]struct{}
}

func (t CodeOwnerTargets) Empty() bool {
	return len(t.UserIDs) == 0 && len(t.TeamIDs) == 0
}

func ResolveCodeOwnerTargets(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo, owners []codeowners.Owner) (CodeOwnerTargets, error) {
	targets := CodeOwnerTargets{
		UserIDs: map[int64]struct{}{},
		TeamIDs: map[int64]struct{}{},
	}
	var org orgsdb.Org
	var hasOrg bool
	if repo.OwnerOrgID.Valid {
		o, err := orgsdb.New().GetOrgByID(ctx, pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return targets, err
		}
		org = o
		hasOrg = true
	}
	for _, owner := range owners {
		switch owner.Kind {
		case codeowners.OwnerUser:
			u, err := usersdb.New().GetUserByUsername(ctx, pool, strings.ToLower(owner.Username))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return targets, err
			}
			if userCanReview(ctx, pool, repo, u) {
				targets.UserIDs[u.ID] = struct{}{}
			}
		case codeowners.OwnerTeam:
			if !hasOrg || !strings.EqualFold(owner.Org, org.Slug) {
				continue
			}
			team, err := orgsdb.New().GetTeamByOrgAndSlug(ctx, pool, orgsdb.GetTeamByOrgAndSlugParams{
				OrgID: org.ID,
				Slug:  strings.ToLower(owner.Team),
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return targets, err
			}
			grant, err := orgsdb.New().GetTeamRepoAccess(ctx, pool, orgsdb.GetTeamRepoAccessParams{
				TeamID: team.ID,
				RepoID: repo.ID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return targets, err
			}
			if teamRepoRoleCanReview(grant.Role) {
				targets.TeamIDs[team.ID] = struct{}{}
			}
		}
	}
	return targets, nil
}

func userCanReview(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo, u usersdb.User) bool {
	has2FA, err := usersdb.New().HasConfirmedUserTOTP(ctx, pool, u.ID)
	if err != nil {
		return false
	}
	actor := policy.UserActorWithTwoFactor(u.ID, u.Username, u.SuspendedAt.Valid, u.IsSiteAdmin, has2FA)
	return policy.Can(ctx, policy.Deps{Pool: pool}, actor, policy.ActionPullReview, policy.NewRepoRefFromRepo(repo)).Allow
}

func teamRepoRoleCanReview(role orgsdb.TeamRepoRole) bool {
	switch role {
	case orgsdb.TeamRepoRoleWrite, orgsdb.TeamRepoRoleMaintain, orgsdb.TeamRepoRoleAdmin:
		return true
	default:
		return false
	}
}

func codeOwnerReviewSatisfied(ctx context.Context, pool *pgxpool.Pool, in GateInputs, repo reposdb.Repo, approved map[int64]struct{}) (bool, []string, error) {
	if in.GitDir == "" || in.BaseOID == "" {
		return false, []string{"CODEOWNERS"}, nil
	}
	ownersFile, ok, err := codeowners.Load(ctx, in.GitDir, in.BaseOID)
	if err != nil {
		return false, nil, err
	}
	if !ok {
		return true, nil, nil
	}
	files, err := pullsdb.New().ListPullRequestFiles(ctx, pool, in.PRIssueID)
	if err != nil {
		return false, nil, err
	}
	missing := []string{}
	for _, file := range files {
		entry, matched := ownersFile.OwnersFor(file.Path)
		if !matched || len(entry.Owners) == 0 {
			continue
		}
		targets, err := ResolveCodeOwnerTargets(ctx, pool, repo, entry.Owners)
		if err != nil {
			return false, nil, err
		}
		if targets.Empty() {
			missing = append(missing, file.Path)
			continue
		}
		ok, err := targetsHaveApproval(ctx, pool, targets, approved)
		if err != nil {
			return false, nil, err
		}
		if !ok {
			missing = append(missing, file.Path)
		}
	}
	return len(missing) == 0, missing, nil
}

func targetsHaveApproval(ctx context.Context, pool *pgxpool.Pool, targets CodeOwnerTargets, approved map[int64]struct{}) (bool, error) {
	for id := range targets.UserIDs {
		if _, ok := approved[id]; ok {
			return true, nil
		}
	}
	if len(targets.TeamIDs) == 0 || len(approved) == 0 {
		return false, nil
	}
	q := orgsdb.New()
	for teamID := range targets.TeamIDs {
		members, err := q.ListTeamMembers(ctx, pool, teamID)
		if err != nil {
			return false, err
		}
		for _, member := range members {
			if _, ok := approved[member.UserID]; ok {
				return true, nil
			}
		}
	}
	return false, nil
}
