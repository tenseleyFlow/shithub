// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/secretscan"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// secretScanMaxFileBytes mirrors the code-index size cap. Files
// larger than this are skipped from secret scanning — a "secret"
// committed in a 256 KiB+ blob is rare enough that the cost of
// scanning every such file isn't worth it.
const secretScanMaxFileBytes = 256 * 1024

// SecretScanHistoryDeps wires the job.
type SecretScanHistoryDeps struct {
	Pool    *pgxpool.Pool
	RepoFS  *storage.RepoFS
	Logger  *slog.Logger
	Enforce config.EnforceConfig
}

// SecretScanHistoryPayload — the only input is the repo id.
type SecretScanHistoryPayload struct {
	RepoID int64 `json:"repo_id"`
}

// SecretScanHistory walks the repo's default-branch tree, scans each
// text blob with the curated pattern engine, and writes findings.
// Re-runs are idempotent on (repo, pattern, path, line) — the unique
// constraint absorbs duplicate matches and only refreshes the
// last_seen_oid + last_seen_at.
//
// Owner gate: the worker checks FeatureSecretScanHistory on the repo
// owner before doing any work. Free-owned repos short-circuit with
// either a report-only log line (default) or a no-op (enforce mode).
//
// 10c adds the allowlist + the email/webhook alerts on insert; this
// PR is *just* the storage + worker.
func SecretScanHistory(deps SecretScanHistoryDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p SecretScanHistoryPayload
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
			return err
		}
		if repo.DeletedAt.Valid {
			return nil
		}

		// Owner gate. Org-owned repos fall through (sprint 10 is
		// user-tier only); user-owned repos consult the owner's plan.
		if repo.OwnerUserID.Valid {
			principal := billing.PrincipalForUser(repo.OwnerUserID.Int64)
			decision, derr := entitlements.CheckPrincipalFeature(ctx,
				entitlements.Deps{Pool: deps.Pool},
				principal,
				entitlements.FeatureSecretScanHistory)
			if derr == nil && !decision.Allowed {
				if deps.Enforce.UserSecretScanHistory {
					deps.Logger.InfoContext(ctx, "secret-scan: owner gate denied",
						"repo_id", repo.ID, "owner_user_id", repo.OwnerUserID.Int64,
						"mode", "enforce")
					return nil
				}
				deps.Logger.InfoContext(ctx, "entitlements.report_only_deny",
					"principal", principal.String(),
					"principal_kind", string(billing.SubjectKindUser),
					"principal_id", repo.OwnerUserID.Int64,
					"feature", string(entitlements.FeatureSecretScanHistory),
					"reason", string(decision.Reason),
					"required_plan", string(decision.RequiredPlan),
					"mode", "report_only",
					"surface", "secret_scan_history_worker")
				// Fall through: scan still runs in report-only mode.
			}
		}

		owner, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, repo.ID)
		if err != nil {
			return err
		}
		ownerSlug, err := ownerSlugString(owner.OwnerUsername)
		if err != nil {
			return err
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerSlug, repo.Name)
		if err != nil {
			return err
		}

		ref := repo.DefaultBranch
		oid, err := repogit.ResolveRefOID(ctx, gitDir, ref)
		if err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				// Empty repo or default branch unset — nothing to scan.
				return nil
			}
			return fmt.Errorf("resolve %s: %w", ref, err)
		}

		paths, err := repogit.ListAllPaths(ctx, gitDir, ref)
		if err != nil {
			return fmt.Errorf("ls-tree: %w", err)
		}

		sq := secretscandb.New()

		// Pre-load the allowlist as a (pattern, path) set so the per-
		// finding skip check is an in-memory map lookup rather than a
		// round-trip. Allowlist sizes are bounded by user intent;
		// loading the whole repo's list at scan start is fine.
		allowSet := loadAllowlistSet(ctx, sq, deps.Pool, repo.ID, deps.Logger)

		totalFindings := 0
		for _, path := range paths {
			if shouldSkipPath(path) {
				continue
			}
			blob, err := repogit.ReadBlobBytes(ctx, gitDir, ref, path, secretScanMaxFileBytes+1)
			if err != nil || len(blob) > secretScanMaxFileBytes || !isText(blob) {
				continue
			}
			findings := secretscan.Scan(blob, secretscan.ScanOptions{
				MaxBytes: secretScanMaxFileBytes,
			})
			for _, f := range findings {
				if _, skip := allowSet[allowlistKey{Pattern: f.Pattern, Path: path}]; skip {
					continue
				}
				excerpt := f.Excerpt
				if len(excerpt) > 400 {
					excerpt = excerpt[:400]
				}
				if _, ierr := sq.UpsertSecretScanFinding(ctx, deps.Pool, secretscandb.UpsertSecretScanFindingParams{
					RepoID:       repo.ID,
					Pattern:      f.Pattern,
					Path:         path,
					LineNo:       int32(f.Line),
					Excerpt:      excerpt,
					FirstSeenOid: oid,
				}); ierr != nil {
					deps.Logger.WarnContext(ctx, "secret-scan: upsert finding",
						"repo_id", repo.ID, "pattern", f.Pattern, "path", path, "error", ierr)
					continue
				}
				totalFindings++
			}
		}

		// Mark prior open findings whose last_seen_oid != current oid
		// as stale. This makes the findings list accurate across
		// re-scans even though we don't delete old rows.
		if err := sq.MarkSecretScanFindingsStaleForRepo(ctx, deps.Pool, secretscandb.MarkSecretScanFindingsStaleForRepoParams{
			RepoID:      repo.ID,
			LastSeenOid: oid,
		}); err != nil {
			deps.Logger.WarnContext(ctx, "secret-scan: mark stale", "repo_id", repo.ID, "error", err)
		}

		deps.Logger.InfoContext(ctx, "secret-scan: scan complete",
			"repo_id", repo.ID, "findings", totalFindings, "oid", oid)
		return nil
	}
}

// allowlistKey is the (pattern, path) tuple that the per-repo
// allowlist set is keyed on inside the worker.
type allowlistKey struct {
	Pattern string
	Path    string
}

// loadAllowlistSet returns the repo's allowlist as a map for cheap
// per-finding skip checks. Errors return an empty set + a warn log;
// failing closed (scanning everything) is the safer default than
// failing open (skipping everything).
func loadAllowlistSet(ctx context.Context, sq *secretscandb.Queries, pool *pgxpool.Pool, repoID int64, logger *slog.Logger) map[allowlistKey]struct{} {
	rows, err := sq.ListSecretScanAllowlistForRepo(ctx, pool, repoID)
	if err != nil {
		logger.WarnContext(ctx, "secret-scan: load allowlist", "repo_id", repoID, "error", err)
		return map[allowlistKey]struct{}{}
	}
	out := make(map[allowlistKey]struct{}, len(rows))
	for _, r := range rows {
		out[allowlistKey{Pattern: r.Pattern, Path: r.Path}] = struct{}{}
	}
	return out
}
