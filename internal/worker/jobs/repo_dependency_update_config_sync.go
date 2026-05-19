// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos/dependencyupdates"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type RepoDependencyUpdateConfigSyncDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

type RepoDependencyUpdateConfigSyncPayload struct {
	RepoID int64 `json:"repo_id"`
}

type dependencyUpdateConfigSyncSummary struct {
	Status      string                                 `json:"status"`
	Message     string                                 `json:"message,omitempty"`
	HeadSHA     string                                 `json:"head_sha,omitempty"`
	ConfigCount int                                    `json:"config_count,omitempty"`
	Diagnostics []dependencyUpdateConfigSyncDiagnostic `json:"diagnostics,omitempty"`
}

type dependencyUpdateConfigSyncDiagnostic struct {
	Path     string `json:"path"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// RepoDependencyUpdateConfigSync reads a repository's
// .github/dependabot.yml-compatible config from the default branch and stores
// the supported update entries. It is Team-gated for org-owned repositories and
// does not create dependency update PRs; later SP25b workers consume the stored
// configs.
func RepoDependencyUpdateConfigSync(deps RepoDependencyUpdateConfigSyncDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("dependency update config sync: missing pool")
		}
		if deps.RepoFS == nil {
			return errors.New("dependency update config sync: missing repo fs")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}

		var p RepoDependencyUpdateConfigSyncPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.RepoID == 0 {
			return worker.PoisonError(errors.New("missing repo_id"))
		}

		rq := reposdb.New()
		repo, err := rq.GetRepoByID(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("repo %d not found", p.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}
		if repo.DeletedAt.Valid {
			return nil
		}

		job, err := rq.CreateDependencyUpdateJob(ctx, deps.Pool, reposdb.CreateDependencyUpdateJobParams{
			RepoID:        repo.ID,
			JobKind:       "config_sync",
			Status:        "running",
			TriggerSource: "push",
			ResultSummary: []byte("{}"),
		})
		if err != nil {
			return fmt.Errorf("create config sync job: %w", err)
		}
		if _, err := rq.MarkDependencyUpdateJobRunning(ctx, deps.Pool, job.ID); err != nil {
			return fmt.Errorf("mark config sync running: %w", err)
		}

		if !repo.OwnerOrgID.Valid {
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "completed", "",
				dependencyUpdateConfigSyncSummary{Status: "skipped", Message: "repository is not org-owned"}, "")
		}
		decision, err := entitlements.CheckPrincipalFeature(ctx,
			entitlements.Deps{Pool: deps.Pool},
			billing.PrincipalForOrg(repo.OwnerOrgID.Int64),
			entitlements.FeatureDependabotVersionUpdates)
		if err != nil {
			return fmt.Errorf("dependency update entitlement: %w", err)
		}
		if !decision.Allowed {
			if err := disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil); err != nil {
				return fmt.Errorf("disable denied dependency update configs: %w", err)
			}
			logger.InfoContext(ctx, "dependency update config sync skipped by entitlement",
				"repo_id", repo.ID, "org_id", repo.OwnerOrgID.Int64, "reason", decision.Reason)
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "completed", "",
				dependencyUpdateConfigSyncSummary{Status: "skipped", Message: "Team dependency update entitlement denied"}, "")
		}

		owner, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, repo.ID)
		if err != nil {
			return fmt.Errorf("load repo owner: %w", err)
		}
		ownerSlug, err := ownerSlugString(owner.OwnerUsername)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo owner slug: %w", err))
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerSlug, repo.Name)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo path: %w", err))
		}

		head, err := repogit.ResolveRefOID(ctx, gitDir, repo.DefaultBranch)
		if err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				if derr := disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil); derr != nil {
					return fmt.Errorf("disable configs for missing ref: %w", derr)
				}
				return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "completed", "",
					dependencyUpdateConfigSyncSummary{Status: "skipped", Message: "default branch is missing"}, "")
			}
			return fmt.Errorf("resolve default branch: %w", err)
		}

		kind, _, size, err := repogit.StatPath(ctx, gitDir, repo.DefaultBranch, dependencyupdates.DefaultConfigPath)
		if err != nil {
			if errors.Is(err, repogit.ErrPathNotFound) {
				if derr := disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil); derr != nil {
					return fmt.Errorf("disable configs for missing config: %w", derr)
				}
				return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "completed", head,
					dependencyUpdateConfigSyncSummary{Status: "skipped", HeadSHA: head, Message: "no dependency update config"}, "")
			}
			return fmt.Errorf("stat dependency update config: %w", err)
		}
		if kind != repogit.EntryBlob {
			if derr := disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil); derr != nil {
				return fmt.Errorf("disable configs for non-blob config: %w", derr)
			}
			msg := "dependency update config path is not a file"
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "failed", head,
				dependencyUpdateConfigSyncSummary{Status: "failed", HeadSHA: head, Message: msg}, msg)
		}
		if size > dependencyupdates.MaxConfigFileBytes {
			if derr := disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil); derr != nil {
				return fmt.Errorf("disable configs for oversized config: %w", derr)
			}
			msg := fmt.Sprintf("dependency update config is %d bytes; limit is %d", size, dependencyupdates.MaxConfigFileBytes)
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "failed", head,
				dependencyUpdateConfigSyncSummary{Status: "failed", HeadSHA: head, Message: msg}, msg)
		}

		body, err := repogit.ReadBlobBytes(ctx, gitDir, repo.DefaultBranch, dependencyupdates.DefaultConfigPath, dependencyupdates.MaxConfigFileBytes)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, repogit.ErrBlobTooLarge) {
				_ = disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil)
				return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "failed", head,
					dependencyUpdateConfigSyncSummary{Status: "failed", HeadSHA: head, Message: msg}, msg)
			}
			return fmt.Errorf("read dependency update config: %w", err)
		}
		file, diags, err := dependencyupdates.Parse(body)
		if err != nil {
			msg := err.Error()
			_ = disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil)
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "failed", head,
				dependencyUpdateConfigSyncSummary{
					Status:      "failed",
					HeadSHA:     head,
					Message:     msg,
					Diagnostics: dependencyUpdateDiagnostics(diags),
				}, msg)
		}
		if file == nil {
			msg := "dependency update config has validation errors"
			_ = disableDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, nil)
			return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "failed", head,
				dependencyUpdateConfigSyncSummary{
					Status:      "failed",
					HeadSHA:     head,
					Message:     msg,
					Diagnostics: dependencyUpdateDiagnostics(diags),
				}, msg)
		}

		activeIDs, err := upsertDependencyUpdateConfigs(ctx, rq, deps.Pool, repo.ID, head, file)
		if err != nil {
			return fmt.Errorf("upsert dependency update configs: %w", err)
		}
		logger.InfoContext(ctx, "dependency update config sync complete",
			"repo_id", repo.ID, "head_sha", head, "configs", len(activeIDs), "diagnostics", len(diags))
		return completeDependencyUpdateConfigSync(ctx, rq, deps.Pool, job.ID, "completed", head,
			dependencyUpdateConfigSyncSummary{
				Status:      "synced",
				HeadSHA:     head,
				ConfigCount: len(activeIDs),
				Diagnostics: dependencyUpdateDiagnostics(diags),
			}, "")
	}
}

func upsertDependencyUpdateConfigs(ctx context.Context, q *reposdb.Queries, pool *pgxpool.Pool, repoID int64, head string, file *dependencyupdates.File) ([]int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	activeIDs := make([]int64, 0, len(file.Configs))
	for _, cfg := range file.Configs {
		row, err := q.UpsertDependencyUpdateConfig(ctx, tx, reposdb.UpsertDependencyUpdateConfigParams{
			RepoID:               repoID,
			Ecosystem:            cfg.Ecosystem,
			PackageManager:       cfg.PackageManager,
			Directory:            cfg.Directory,
			ScheduleInterval:     cfg.Schedule.Interval,
			ScheduleDay:          cfg.Schedule.Day,
			ScheduleTime:         cfg.Schedule.Time,
			ScheduleTimezone:     cfg.Schedule.Timezone,
			ScheduleCron:         cfg.Schedule.Cronjob,
			OpenPullRequestLimit: int32(cfg.OpenPullRequestMax),
			TargetBranch:         cfg.TargetBranch,
			Enabled:              true,
			RawConfigHash:        file.RawConfigHash,
			RawConfigPath:        cfg.RawConfigPath,
			LastSyncedSha:        head,
			AllowRules:           cfg.AllowRulesJSON,
			IgnoreRules:          cfg.IgnoreRulesJSON,
			Groups:               cfg.GroupsJSON,
			Registries:           cfg.RegistriesJSON,
			UnsupportedKeys:      cfg.UnsupportedKeys,
		})
		if err != nil {
			return nil, err
		}
		activeIDs = append(activeIDs, row.ID)
	}
	if err := q.DisableMissingDependencyUpdateConfigs(ctx, tx, reposdb.DisableMissingDependencyUpdateConfigsParams{
		RepoID:    repoID,
		ActiveIds: activeIDs,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return activeIDs, nil
}

func disableDependencyUpdateConfigs(ctx context.Context, q *reposdb.Queries, pool *pgxpool.Pool, repoID int64, keep []int64) error {
	if keep == nil {
		keep = []int64{}
	}
	return q.DisableMissingDependencyUpdateConfigs(ctx, pool, reposdb.DisableMissingDependencyUpdateConfigsParams{
		RepoID:    repoID,
		ActiveIds: keep,
	})
}

func completeDependencyUpdateConfigSync(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, jobID int64, status string, head string, summary dependencyUpdateConfigSyncSummary, lastErr string) error {
	body, err := json.Marshal(summary)
	if err != nil {
		body = []byte(`{"status":"failed","message":"could not marshal summary"}`)
	}
	_, err = q.CompleteDependencyUpdateJob(ctx, db, reposdb.CompleteDependencyUpdateJobParams{
		Status:        status,
		HeadSha:       head,
		ResultSummary: body,
		LastError:     lastErr,
		ID:            jobID,
	})
	return err
}

func dependencyUpdateDiagnostics(diags []dependencyupdates.Diagnostic) []dependencyUpdateConfigSyncDiagnostic {
	if len(diags) == 0 {
		return nil
	}
	out := make([]dependencyUpdateConfigSyncDiagnostic, 0, len(diags))
	for _, d := range diags {
		out = append(out, dependencyUpdateConfigSyncDiagnostic{
			Path:     d.Path,
			Message:  d.Message,
			Severity: string(d.Severity),
		})
	}
	return out
}
