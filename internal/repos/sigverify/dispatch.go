// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// BackfillPayload is the on-the-wire schema for the gpg:backfill
// worker job. One job per repo; the handler walks every signed
// commit and tag on the repo's default branch and writes
// commit_verification_cache rows.
//
// The handler is intentionally NOT key-scoped — it verifies every
// signed object regardless of which user key signed it. The
// orchestrator returns ReasonUnknownKey for signatures it can't
// resolve; the cache row carries that state forward and the
// rendering path treats it as "Unverified". When new keys arrive
// later, those previously-unknown-key cache rows will get
// re-stamped by the next DispatchForKey or DispatchAll run.
type BackfillPayload struct {
	RepoID int64 `json:"repo_id"`
}

// DispatchForKey enqueues backfill jobs for every repo owned by the
// user who just added a GPG key. This is the eager-backfill path:
// "user uploads key → existing signed commits in their own repos
// retroactively get Verified badges within minutes" — the gh
// behavior we're matching.
//
// We deliberately scope to OWNED repos rather than every repo the
// user has ever committed to. Owned repos are the common case
// (~90%); cross-repo authorship (PR contributions to others'
// repos) gets picked up by the next DispatchAll run. Without a
// commits-author index, walking every repo on every key upload
// would be O(repos × commits) per upload and is the wrong cost
// shape for the eager path.
//
// The caller passes (db) so this can run inside an existing
// transaction (typical pattern in the add-key handler: insert key
// row → insert subkey rows → dispatch backfill → commit). Notify
// is called inside the same tx so workers wake up on commit, not
// before.
func DispatchForKey(ctx context.Context, db reposdb.DBTX, userID int64) error {
	rq := reposdb.New()
	rows, err := rq.ListReposForOwnerUser(ctx, db, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return fmt.Errorf("sigverify: list user repos: %w", err)
	}
	for _, row := range rows {
		if _, err := worker.Enqueue(ctx, db, worker.KindGPGBackfill, BackfillPayload{
			RepoID: row.ID,
		}, worker.EnqueueOptions{}); err != nil {
			return fmt.Errorf("sigverify: enqueue backfill for repo %d: %w", row.ID, err)
		}
	}
	if err := worker.Notify(ctx, db); err != nil {
		return fmt.Errorf("sigverify: notify workers: %w", err)
	}
	return nil
}

// DispatchAll enqueues backfill jobs for every active repo in the
// system. Called by `shithubd gpg-backfill-all`. Returns the number
// of jobs enqueued so the admin command can log progress.
//
// Unlike DispatchForKey this is NOT bounded to a single user; it
// walks every active repo. Use sparingly — typically once on
// initial S51 deploy, or after a known mass-key-upload event.
func DispatchAll(ctx context.Context, db reposdb.DBTX) (int, error) {
	rq := reposdb.New()
	rows, err := rq.ListAllActiveReposWithOwner(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("sigverify: list all repos: %w", err)
	}
	for _, row := range rows {
		if _, err := worker.Enqueue(ctx, db, worker.KindGPGBackfill, BackfillPayload{
			RepoID: row.ID,
		}, worker.EnqueueOptions{}); err != nil {
			return 0, fmt.Errorf("sigverify: enqueue backfill for repo %d: %w", row.ID, err)
		}
	}
	if err := worker.Notify(ctx, db); err != nil {
		return 0, fmt.Errorf("sigverify: notify workers: %w", err)
	}
	return len(rows), nil
}
