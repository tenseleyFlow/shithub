// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos/sigverify"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// GPGBackfillDeps wires the gpg:backfill handler.
type GPGBackfillDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Logger *slog.Logger
}

// GPGBackfillPayload mirrors sigverify.BackfillPayload — duplicated
// here so the jobs package doesn't pull sigverify in just for the
// type definition. JSON wire shape MUST stay identical to
// sigverify.BackfillPayload; both forms unmarshal the same bytes.
type GPGBackfillPayload struct {
	RepoID int64 `json:"repo_id"`
}

// perCommitTimeout bounds a single object's verification. A
// pathological commit object (huge gpgsig with deep continuation
// lines) shouldn't stall the whole queue.
const perCommitTimeout = 5 * time.Second

// GPGBackfill is the worker.Handler for KindGPGBackfill. One job per
// repo; the handler enumerates every commit on the default branch
// and every annotated tag, then runs sigverify.Verify / VerifyTag
// and writes the result to commit_verification_cache.
//
// The handler is idempotent thanks to UpsertCommitVerification's
// ON CONFLICT clause — re-running this job is safe and is in fact
// the documented recovery path for a partially-completed backfill.
//
// Failure semantics: any per-commit cat-file failure is logged and
// SKIPPED (not retried at the job level) so one corrupted object
// doesn't poison the whole repo's backfill. The job itself returns
// nil unless the repo lookup or git env is unreachable; those
// surface as retryable errors so the worker pool's backoff kicks
// in.
func GPGBackfill(deps GPGBackfillDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p GPGBackfillPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.RepoID == 0 {
			return worker.PoisonError(errors.New("missing repo_id"))
		}

		rq := reposdb.New()
		row, err := rq.GetRepoForBackfill(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Repo was deleted between enqueue and dispatch.
				// Poison so we don't retry a deleted target.
				return worker.PoisonError(fmt.Errorf("repo %d not found", p.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}

		gitDir, err := deps.RepoFS.RepoPath(row.Owner, row.Name)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo path: %w", err))
		}

		lookups := sigverify.NewSQLCLookups(deps.Pool)

		commitsProcessed, err := backfillCommits(ctx, deps, gitDir, p.RepoID, row.DefaultBranch, lookups)
		if err != nil {
			return fmt.Errorf("backfill commits: %w", err)
		}
		tagsProcessed, err := backfillTags(ctx, deps, gitDir, p.RepoID, lookups)
		if err != nil {
			return fmt.Errorf("backfill tags: %w", err)
		}

		deps.Logger.InfoContext(ctx, "gpg backfill completed",
			"repo_id", p.RepoID,
			"commits", commitsProcessed,
			"tags", tagsProcessed,
		)
		return nil
	}
}

// backfillCommits walks every commit on the default branch and
// verifies each. Returns the number processed (signed + unsigned;
// the cache stamps both so future "is this verified" reads don't
// re-walk).
func backfillCommits(
	ctx context.Context,
	deps GPGBackfillDeps,
	gitDir string,
	repoID int64,
	defaultBranch string,
	lookups sigverify.Lookups,
) (int, error) {
	// Empty default branch (uninitialized repo) → nothing to walk.
	if defaultBranch == "" {
		return 0, nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-list", defaultBranch)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("rev-list pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("rev-list start: %w", err)
	}

	rq := reposdb.New()
	scanner := bufio.NewScanner(stdout)
	count := 0
	for scanner.Scan() {
		oid := strings.TrimSpace(scanner.Text())
		if len(oid) != 40 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return count, err
		}
		verifyCtx, cancel := context.WithTimeout(ctx, perCommitTimeout)
		result, vErr := sigverify.Verify(verifyCtx, gitDir, oid, lookups)
		cancel()
		if vErr != nil {
			deps.Logger.WarnContext(ctx, "verify commit failed; skipping",
				"oid", oid, "err", vErr)
			continue
		}
		if wErr := sigverify.WriteResult(ctx, rq, deps.Pool, repoID, oid, sigverify.KindCommit, result); wErr != nil {
			deps.Logger.WarnContext(ctx, "cache write failed; skipping",
				"oid", oid, "err", wErr)
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("scan rev-list: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return count, fmt.Errorf("rev-list: %w: %s", err, stderr.String())
	}
	return count, nil
}

// backfillTags walks every annotated tag in the repo and verifies
// each. Lightweight tags (which carry no signature) are skipped.
// Returns the number processed.
func backfillTags(
	ctx context.Context,
	deps GPGBackfillDeps,
	gitDir string,
	repoID int64,
	lookups sigverify.Lookups,
) (int, error) {
	// for-each-ref filters to refs/tags and emits 'oid type'; we
	// only want annotated tags (type=tag).
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"for-each-ref", "--format=%(objectname) %(objecttype)", "refs/tags",
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("for-each-ref tags: %w", err)
	}

	rq := reposdb.New()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "tag" {
			continue
		}
		oid := fields[0]
		if err := ctx.Err(); err != nil {
			return count, err
		}
		verifyCtx, cancel := context.WithTimeout(ctx, perCommitTimeout)
		result, vErr := sigverify.VerifyTag(verifyCtx, gitDir, oid, lookups)
		cancel()
		if vErr != nil {
			deps.Logger.WarnContext(ctx, "verify tag failed; skipping",
				"oid", oid, "err", vErr)
			continue
		}
		if wErr := sigverify.WriteResult(ctx, rq, deps.Pool, repoID, oid, sigverify.KindTag, result); wErr != nil {
			deps.Logger.WarnContext(ctx, "cache write failed; skipping",
				"oid", oid, "err", wErr)
			continue
		}
		count++
	}
	return count, nil
}
