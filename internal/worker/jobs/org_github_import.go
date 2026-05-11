// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type OrgGitHubImportDeps struct {
	Pool         *pgxpool.Pool
	RepoFS       *storage.RepoFS
	Box          *secretbox.Box
	Audit        *audit.Recorder
	Limiter      *throttle.Limiter
	Logger       *slog.Logger
	ShithubdPath string
	GitHubClient orgs.GitHubClient
}

type OrgGitHubImportDiscoverPayload struct {
	ImportID int64 `json:"import_id"`
}

type OrgGitHubImportRepoPayload struct {
	ImportRepoID int64 `json:"import_repo_id"`
}

func OrgGitHubImportDiscover(deps OrgGitHubImportDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p OrgGitHubImportDiscoverPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.ImportID == 0 {
			return worker.PoisonError(errors.New("missing import_id"))
		}

		q := orgsdb.New()
		imp, err := q.GetOrgGithubImport(ctx, deps.Pool, p.ImportID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("import %d not found", p.ImportID))
			}
			return err
		}
		if orgs.IsTerminalImportStatus(imp.Status) {
			return nil
		}
		token, err := orgs.DecryptGitHubImportToken(imp, deps.Box)
		if err != nil {
			_ = markImportFailed(ctx, deps, imp.ID, err)
			return worker.PoisonError(err)
		}
		if err := q.MarkOrgGithubImportDiscovering(ctx, deps.Pool, imp.ID); err != nil {
			return err
		}
		ghRepos, err := deps.GitHubClient.ListOrgRepos(ctx, imp.SourceOrg, token)
		if err != nil {
			_ = markImportFailed(ctx, deps, imp.ID, err)
			return nil
		}

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

		for _, gh := range ghRepos {
			targetName := repos.NormalizeName(gh.Name)
			visibility := orgsdb.RepoVisibilityPublic
			if gh.Private {
				visibility = orgsdb.RepoVisibilityPrivate
			}
			row, err := q.InsertOrgGithubImportRepo(ctx, tx, orgsdb.InsertOrgGithubImportRepoParams{
				ImportID:         imp.ID,
				GithubID:         pgtype.Int8{Int64: gh.ID, Valid: gh.ID != 0},
				SourceFullName:   fallbackFullName(imp.SourceOrg, gh),
				SourceName:       strings.TrimSpace(gh.Name),
				TargetName:       targetName,
				CloneUrl:         strings.TrimSpace(gh.CloneURL),
				Description:      truncateRunes(gh.Description, repos.MaxDescriptionLen),
				DefaultBranch:    strings.TrimSpace(gh.DefaultBranch),
				TargetVisibility: visibility,
				IsPrivate:        gh.Private,
				IsFork:           gh.Fork,
			})
			if err != nil {
				return err
			}
			if _, err := worker.Enqueue(ctx, tx, worker.KindOrgGitHubImportRepo, OrgGitHubImportRepoPayload{
				ImportRepoID: row.ID,
			}, worker.EnqueueOptions{}); err != nil {
				return err
			}
		}
		if len(ghRepos) == 0 {
			if err := q.MarkOrgGithubImportCompleted(ctx, tx, imp.ID); err != nil {
				return err
			}
		} else if err := q.MarkOrgGithubImportImporting(ctx, tx, orgsdb.MarkOrgGithubImportImportingParams{
			ID:         imp.ID,
			TotalCount: int32(len(ghRepos)),
		}); err != nil {
			return err
		}
		if err := worker.Notify(ctx, tx); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "github import: notify children", "error", err, "import_id", imp.ID)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	}
}

func OrgGitHubImportRepo(deps OrgGitHubImportDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p OrgGitHubImportRepoPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.ImportRepoID == 0 {
			return worker.PoisonError(errors.New("missing import_repo_id"))
		}

		q := orgsdb.New()
		rq := reposdb.New()
		item, err := q.GetOrgGithubImportRepo(ctx, deps.Pool, p.ImportRepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("import repo %d not found", p.ImportRepoID))
			}
			return err
		}
		if orgs.IsTerminalImportRepoStatus(item.Status) {
			return nil
		}
		imp, err := q.GetOrgGithubImport(ctx, deps.Pool, item.ImportID)
		if err != nil {
			return err
		}
		if orgs.IsTerminalImportStatus(imp.Status) {
			return nil
		}
		org, err := q.GetOrgByID(ctx, deps.Pool, imp.OrgID)
		if err != nil {
			return err
		}
		if org.DeletedAt.Valid {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, 0, "Organization was deleted during import."); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}
		if err := q.MarkOrgGithubImportRepoImporting(ctx, deps.Pool, item.ID); err != nil {
			return err
		}

		if err := repos.ValidateName(item.TargetName); err != nil {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, 0, friendlyRepoImportError(err)); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}
		exists, err := rq.ExistsRepoForOwnerOrg(ctx, deps.Pool, reposdb.ExistsRepoForOwnerOrgParams{
			OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
			Name:       item.TargetName,
		})
		if err != nil {
			return err
		}
		if exists {
			if err := q.MarkOrgGithubImportRepoSkipped(ctx, deps.Pool, orgsdb.MarkOrgGithubImportRepoSkippedParams{
				ID:        item.ID,
				LastError: pgtype.Text{String: "Repository already exists in this organization.", Valid: true},
			}); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}

		token, err := orgs.DecryptGitHubImportToken(imp, deps.Box)
		if err != nil {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, 0, err.Error()); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}
		if item.IsPrivate && token == "" {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, 0, "GitHub token unavailable for private repository."); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}

		result, err := repos.Create(ctx, repos.Deps{
			Pool:         deps.Pool,
			RepoFS:       deps.RepoFS,
			Audit:        deps.Audit,
			Limiter:      deps.Limiter,
			Logger:       deps.Logger,
			ShithubdPath: deps.ShithubdPath,
		}, repos.Params{
			OwnerOrgID:            org.ID,
			OwnerSlug:             string(org.Slug),
			ActorUserID:           int64Value(imp.RequestedByUserID),
			BypassCreateRateLimit: true,
			Name:                  item.TargetName,
			Description:           item.Description,
			Visibility:            string(item.TargetVisibility),
		})
		if err != nil {
			if errors.Is(err, repos.ErrTaken) {
				if err := q.MarkOrgGithubImportRepoSkipped(ctx, deps.Pool, orgsdb.MarkOrgGithubImportRepoSkippedParams{
					ID:        item.ID,
					LastError: pgtype.Text{String: "Repository already exists in this organization.", Valid: true},
				}); err != nil {
					return err
				}
				return completeImportIfDone(ctx, deps, imp.ID)
			}
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, 0, friendlyRepoImportError(err)); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}

		remoteURL, err := repos.SaveSourceRemote(ctx, sourceRemoteDeps(deps, token), result.Repo.ID, item.CloneUrl)
		if err != nil {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, result.Repo.ID, friendlyRepoImportError(err)); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}
		if err := repos.FetchSourceRemote(ctx, sourceRemoteDeps(deps, token), result.Repo, string(org.Slug), remoteURL); err != nil {
			if err := markImportRepoFailed(ctx, q, deps.Pool, item.ID, result.Repo.ID, friendlyRepoImportError(err)); err != nil {
				return err
			}
			return completeImportIfDone(ctx, deps, imp.ID)
		}
		if err := q.MarkOrgGithubImportRepoImported(ctx, deps.Pool, orgsdb.MarkOrgGithubImportRepoImportedParams{
			ID:     item.ID,
			RepoID: pgtype.Int8{Int64: result.Repo.ID, Valid: true},
		}); err != nil {
			return err
		}
		return completeImportIfDone(ctx, deps, imp.ID)
	}
}

func sourceRemoteDeps(deps OrgGitHubImportDeps, token string) repos.SourceRemoteDeps {
	return repos.SourceRemoteDeps{
		Pool:       deps.Pool,
		RepoFS:     deps.RepoFS,
		Logger:     deps.Logger,
		FetchToken: token,
	}
}

func markImportFailed(ctx context.Context, deps OrgGitHubImportDeps, importID int64, err error) error {
	msg := friendlyRepoImportError(err)
	return orgsdb.New().MarkOrgGithubImportFailed(ctx, deps.Pool, orgsdb.MarkOrgGithubImportFailedParams{
		ID:        importID,
		LastError: pgtype.Text{String: msg, Valid: true},
	})
}

func markImportRepoFailed(ctx context.Context, q *orgsdb.Queries, db orgsdb.DBTX, itemID, repoID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		msg = "Import failed."
	}
	return q.MarkOrgGithubImportRepoFailed(ctx, db, orgsdb.MarkOrgGithubImportRepoFailedParams{
		ID:        itemID,
		LastError: pgtype.Text{String: truncateRunes(msg, 500), Valid: true},
		RepoID:    pgtype.Int8{Int64: repoID, Valid: repoID != 0},
	})
}

func completeImportIfDone(ctx context.Context, deps OrgGitHubImportDeps, importID int64) error {
	_, err := orgsdb.New().MarkOrgGithubImportCompletedIfDone(ctx, deps.Pool, importID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func fallbackFullName(sourceOrg string, repo orgs.GitHubRepo) string {
	if strings.TrimSpace(repo.FullName) != "" {
		return strings.TrimSpace(repo.FullName)
	}
	return sourceOrg + "/" + strings.TrimSpace(repo.Name)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max])
}

func friendlyRepoImportError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "Import failed."
	}
	return truncateRunes(msg, 500)
}

func int64Value(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}
