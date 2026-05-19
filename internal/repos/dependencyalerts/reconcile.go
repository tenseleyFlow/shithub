// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dependencyalerts reconciles current repository dependencies against
// the local dependency advisory catalog.
package dependencyalerts

import (
	"context"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/repos/advisorymatch"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// RefreshForRepo opens/reopens dependency alerts for current dependency rows
// whose local advisory range matches, then resolves open alerts whose dependency
// row is stale, advisory is withdrawn, or range no longer matches.
func RefreshForRepo(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, repoID int64) error {
	candidates, err := q.ListDependencyAlertCandidatesForRepo(ctx, db, repoID)
	if err != nil {
		return fmt.Errorf("list dependency alert candidates for repo: %w", err)
	}
	for _, candidate := range candidates {
		if !advisorymatch.MatchVersion(candidate.Ecosystem, candidate.PackageVersion, candidate.AffectedRange) {
			continue
		}
		if err := q.UpsertDependencyAlertMatch(ctx, db, reposdb.UpsertDependencyAlertMatchParams{
			RepoID:       candidate.RepoID,
			DependencyID: candidate.DependencyID,
			AdvisoryID:   candidate.AdvisoryID,
		}); err != nil {
			return fmt.Errorf("upsert dependency alert match: %w", err)
		}
	}

	open, err := q.ListOpenDependencyAlertCandidatesForRepo(ctx, db, repoID)
	if err != nil {
		return fmt.Errorf("list open dependency alerts for repo: %w", err)
	}
	for _, alert := range open {
		if alert.DependencyCurrent &&
			alert.AdvisoryActive &&
			advisorymatch.MatchVersion(alert.Ecosystem, alert.PackageVersion, alert.AffectedRange) {
			continue
		}
		if err := q.ResolveDependencyAlertByID(ctx, db, alert.AlertID); err != nil {
			return fmt.Errorf("resolve dependency alert: %w", err)
		}
	}
	return nil
}

// RefreshForAdvisory reconciles alerts after one advisory changes. It has the
// same open/reopen/resolve behavior as RefreshForRepo but scopes candidates by
// advisory source and external ID so repository advisory publish/withdraw paths
// do not need to rescan the whole catalog.
func RefreshForAdvisory(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, source, externalID string) error {
	params := reposdb.ListDependencyAlertCandidatesForAdvisoryParams{
		Source:     source,
		ExternalID: externalID,
	}
	candidates, err := q.ListDependencyAlertCandidatesForAdvisory(ctx, db, params)
	if err != nil {
		return fmt.Errorf("list dependency alert candidates for advisory: %w", err)
	}
	for _, candidate := range candidates {
		if !advisorymatch.MatchVersion(candidate.Ecosystem, candidate.PackageVersion, candidate.AffectedRange) {
			continue
		}
		if err := q.UpsertDependencyAlertMatch(ctx, db, reposdb.UpsertDependencyAlertMatchParams{
			RepoID:       candidate.RepoID,
			DependencyID: candidate.DependencyID,
			AdvisoryID:   candidate.AdvisoryID,
		}); err != nil {
			return fmt.Errorf("upsert dependency alert match: %w", err)
		}
	}

	open, err := q.ListOpenDependencyAlertCandidatesForAdvisory(ctx, db, reposdb.ListOpenDependencyAlertCandidatesForAdvisoryParams{
		Source:     source,
		ExternalID: externalID,
	})
	if err != nil {
		return fmt.Errorf("list open dependency alerts for advisory: %w", err)
	}
	for _, alert := range open {
		if alert.DependencyCurrent &&
			alert.AdvisoryActive &&
			advisorymatch.MatchVersion(alert.Ecosystem, alert.PackageVersion, alert.AffectedRange) {
			continue
		}
		if err := q.ResolveDependencyAlertByID(ctx, db, alert.AlertID); err != nil {
			return fmt.Errorf("resolve dependency alert: %w", err)
		}
	}
	return nil
}
