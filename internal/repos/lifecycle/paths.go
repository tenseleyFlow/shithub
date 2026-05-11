// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

type repoDiskPaths struct {
	canonical string
	deleted   string
}

func lockRepoName(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, repo reposdb.Repo) error {
	key, err := repoNameLockKey(repo.OwnerUserID, repo.OwnerOrgID, repo.Name)
	if err != nil {
		return err
	}
	if err := q.LockRepoOwnerName(ctx, db, key); err != nil {
		return fmt.Errorf("lock repo owner/name: %w", err)
	}
	return nil
}

func repoNameLockKey(ownerUserID, ownerOrgID pgtype.Int8, name string) (string, error) {
	name = strings.ToLower(name)
	switch {
	case ownerUserID.Valid && !ownerOrgID.Valid:
		return fmt.Sprintf("repo-name:user:%d:%s", ownerUserID.Int64, name), nil
	case ownerOrgID.Valid && !ownerUserID.Valid:
		return fmt.Sprintf("repo-name:org:%d:%s", ownerOrgID.Int64, name), nil
	default:
		return "", errors.New("lifecycle: repo owner is not xor")
	}
}

func diskPathsForRepo(ctx context.Context, deps Deps, repo reposdb.Repo) (repoDiskPaths, error) {
	owner, err := ownerSlugForRepo(ctx, deps, repo)
	if err != nil {
		return repoDiskPaths{}, err
	}
	canonical, err := deps.RepoFS.RepoPath(owner, repo.Name)
	if err != nil {
		return repoDiskPaths{}, fmt.Errorf("canonical repo path: %w", err)
	}
	deleted, err := deps.RepoFS.DeletedRepoPath(owner, repo.Name, repo.ID)
	if err != nil {
		return repoDiskPaths{}, fmt.Errorf("deleted repo path: %w", err)
	}
	return repoDiskPaths{canonical: canonical, deleted: deleted}, nil
}

func ownerSlugForRepo(ctx context.Context, deps Deps, repo reposdb.Repo) (string, error) {
	switch {
	case repo.OwnerUserID.Valid && !repo.OwnerOrgID.Valid:
		user, err := usersdb.New().GetUserIncludingDeleted(ctx, deps.Pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", fmt.Errorf("load repo owner user: %w", err)
		}
		return user.Username, nil
	case repo.OwnerOrgID.Valid && !repo.OwnerUserID.Valid:
		org, err := orgsdb.New().GetOrgByID(ctx, deps.Pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", fmt.Errorf("load repo owner org: %w", err)
		}
		return string(org.Slug), nil
	default:
		return "", errors.New("lifecycle: repo owner is not xor")
	}
}

func activeRepoNameExists(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, repo reposdb.Repo) (bool, error) {
	switch {
	case repo.OwnerUserID.Valid && !repo.OwnerOrgID.Valid:
		return q.ExistsRepoForOwnerUser(ctx, db, reposdb.ExistsRepoForOwnerUserParams{
			OwnerUserID: pgtype.Int8{Int64: repo.OwnerUserID.Int64, Valid: true},
			Name:        repo.Name,
		})
	case repo.OwnerOrgID.Valid && !repo.OwnerUserID.Valid:
		return q.ExistsRepoForOwnerOrg(ctx, db, reposdb.ExistsRepoForOwnerOrgParams{
			OwnerOrgID: pgtype.Int8{Int64: repo.OwnerOrgID.Int64, Valid: true},
			Name:       repo.Name,
		})
	default:
		return false, errors.New("lifecycle: repo owner is not xor")
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
