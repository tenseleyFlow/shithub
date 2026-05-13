// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
)

// Errors surfaced by the team orchestrator.
var (
	ErrTeamNotFound        = errors.New("orgs: team not found")
	ErrTeamSlugInvalid     = errors.New("orgs: team slug must be lowercase letters, digits, and hyphens (1-50)")
	ErrTeamSlugTaken       = errors.New("orgs: team slug already in use in this org")
	ErrTeamNestingTooDeep  = errors.New("orgs: team nesting limited to one level (parent already has a parent)")
	ErrTeamSelfParent      = errors.New("orgs: a team cannot be its own parent")
	ErrTeamCrossOrgParent  = errors.New("orgs: parent team must belong to the same org")
	ErrTeamMissingActor    = errors.New("orgs: actor required for team mutation")
	ErrTeamRoleInvalid     = errors.New("orgs: invalid team role")
	ErrTeamRepoRoleInvalid = errors.New("orgs: invalid team repo-access role")
)

// teamSlugRE matches the team slug shape (lowercase letters, digits,
// hyphens, dots; can't start/end with hyphen). 50 chars matches the
// migration's CHECK.
var teamSlugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,48}[a-z0-9])?$`)

// CreateTeamParams describes a new-team request.
type CreateTeamParams struct {
	OrgID           int64
	Slug            string
	DisplayName     string
	Description     string
	ParentTeamID    int64  // 0 → top-level
	Privacy         string // "visible" | "secret"
	CreatedByUserID int64
}

// CreateTeam inserts a team row. Validates slug format, checks
// parent-team org match, and translates the trigger's nesting-violation
// SQLSTATE into a friendly error.
func CreateTeam(ctx context.Context, deps Deps, p CreateTeamParams) (orgsdb.Team, error) {
	slug := strings.ToLower(strings.TrimSpace(p.Slug))
	if !teamSlugRE.MatchString(slug) {
		return orgsdb.Team{}, ErrTeamSlugInvalid
	}
	if p.CreatedByUserID == 0 {
		return orgsdb.Team{}, ErrTeamMissingActor
	}
	priv, err := parsePrivacy(p.Privacy)
	if err != nil {
		return orgsdb.Team{}, err
	}
	q := orgsdb.New()
	if p.ParentTeamID != 0 {
		parent, perr := q.GetTeamByID(ctx, deps.Pool, p.ParentTeamID)
		if perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return orgsdb.Team{}, ErrTeamNotFound
			}
			return orgsdb.Team{}, perr
		}
		if parent.OrgID != p.OrgID {
			return orgsdb.Team{}, ErrTeamCrossOrgParent
		}
	}
	row, err := q.CreateTeam(ctx, deps.Pool, orgsdb.CreateTeamParams{
		OrgID:           p.OrgID,
		Slug:            slug,
		DisplayName:     strings.TrimSpace(p.DisplayName),
		Description:     strings.TrimSpace(p.Description),
		ParentTeamID:    pgtype.Int8{Int64: p.ParentTeamID, Valid: p.ParentTeamID != 0},
		Privacy:         priv,
		CreatedByUserID: pgtype.Int8{Int64: p.CreatedByUserID, Valid: true},
	})
	if err != nil {
		return orgsdb.Team{}, translateTeamError(err)
	}
	return row, nil
}

// SetParent changes a team's parent. Both the no-self-parent CHECK
// and the one-level-nesting trigger fire here; we translate to the
// orchestrator-level errors.
func SetTeamParent(ctx context.Context, deps Deps, teamID, parentTeamID int64) error {
	if parentTeamID == teamID {
		return ErrTeamSelfParent
	}
	err := orgsdb.New().SetTeamParent(ctx, deps.Pool, orgsdb.SetTeamParentParams{
		ID:           teamID,
		ParentTeamID: pgtype.Int8{Int64: parentTeamID, Valid: parentTeamID != 0},
	})
	return translateTeamError(err)
}

// AddTeamMember inserts (team, user) at the given role. Idempotent on
// the pair (the sqlc query uses ON CONFLICT DO NOTHING). Caller
// resolves the policy gate; this orchestrator just shapes the row.
func AddTeamMember(ctx context.Context, deps Deps, teamID, userID, addedByUserID int64, role string) error {
	r, err := parseTeamRole(role)
	if err != nil {
		return err
	}
	check, err := entitlements.CheckTeamMemberPrivateCollaboration(ctx, entitlements.Deps{Pool: deps.Pool}, teamID, userID)
	if err != nil {
		return err
	}
	if err := check.Err(); err != nil {
		return err
	}
	return orgsdb.New().AddTeamMember(ctx, deps.Pool, orgsdb.AddTeamMemberParams{
		TeamID:        teamID,
		UserID:        userID,
		Role:          r,
		AddedByUserID: pgtype.Int8{Int64: addedByUserID, Valid: addedByUserID != 0},
	})
}

// RemoveTeamMember drops the (team, user) pair. No last-maintainer
// protection here — teams without maintainers are fine; org owners
// can always still mutate the team via the org-owner policy bypass.
func RemoveTeamMember(ctx context.Context, deps Deps, teamID, userID int64) error {
	return orgsdb.New().RemoveTeamMember(ctx, deps.Pool, orgsdb.RemoveTeamMemberParams{
		TeamID: teamID, UserID: userID,
	})
}

// GrantRepoAccess upserts the team's role on a repo. ON CONFLICT
// DO UPDATE so re-granting at a new role is one call.
func GrantTeamRepoAccess(ctx context.Context, deps Deps, teamID, repoID, addedByUserID int64, role string) error {
	r, err := parseTeamRepoRole(role)
	if err != nil {
		return err
	}
	check, err := entitlements.CheckTeamPrivateRepoGrant(ctx, entitlements.Deps{Pool: deps.Pool}, teamID, repoID)
	if err != nil {
		return err
	}
	if err := check.Err(); err != nil {
		return err
	}
	return orgsdb.New().GrantTeamRepoAccess(ctx, deps.Pool, orgsdb.GrantTeamRepoAccessParams{
		TeamID:        teamID,
		RepoID:        repoID,
		Role:          r,
		AddedByUserID: pgtype.Int8{Int64: addedByUserID, Valid: addedByUserID != 0},
	})
}

// RevokeTeamRepoAccess drops the team's grant. Effective immediately
// (next request denies; per-request policy cache means in-flight
// requests can still pass — same staleness as collaborator changes).
func RevokeTeamRepoAccess(ctx context.Context, deps Deps, teamID, repoID int64) error {
	return orgsdb.New().RevokeTeamRepoAccess(ctx, deps.Pool, orgsdb.RevokeTeamRepoAccessParams{
		TeamID: teamID, RepoID: repoID,
	})
}

// ─── helpers ───────────────────────────────────────────────────────

func parsePrivacy(s string) (orgsdb.TeamPrivacy, error) {
	switch s {
	case "", "visible":
		return orgsdb.TeamPrivacyVisible, nil
	case "secret":
		return orgsdb.TeamPrivacySecret, nil
	}
	return "", fmt.Errorf("orgs: invalid team privacy %q", s)
}

func parseTeamRole(s string) (orgsdb.TeamRole, error) {
	switch s {
	case "", "member":
		return orgsdb.TeamRoleMember, nil
	case "maintainer":
		return orgsdb.TeamRoleMaintainer, nil
	}
	return "", ErrTeamRoleInvalid
}

func parseTeamRepoRole(s string) (orgsdb.TeamRepoRole, error) {
	switch s {
	case "read":
		return orgsdb.TeamRepoRoleRead, nil
	case "triage":
		return orgsdb.TeamRepoRoleTriage, nil
	case "write":
		return orgsdb.TeamRepoRoleWrite, nil
	case "maintain":
		return orgsdb.TeamRepoRoleMaintain, nil
	case "admin":
		return orgsdb.TeamRepoRoleAdmin, nil
	}
	return "", ErrTeamRepoRoleInvalid
}

// translateTeamError maps Postgres errors back to the orchestrator's
// typed errors. The migration's nesting trigger raises CHECK
// (SQLSTATE 23514); the unique index on (org_id, slug) raises 23505.
func translateTeamError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrTeamSlugTaken
		case "23514":
			if strings.Contains(pgErr.Message, "no_self_parent") {
				return ErrTeamSelfParent
			}
			return ErrTeamNestingTooDeep
		}
	}
	return err
}
