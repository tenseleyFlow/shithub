// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// CacheWriter is the minimal surface WriteResult needs from the
// reposdb queries layer. Sub-PR 3 (backfill) and Sub-PR 4 (settings
// add handler) both consume WriteResult; this interface lets them
// pass any concrete DBTX-bound queries set without taking the full
// reposdb.Queries struct as a dependency.
type CacheWriter interface {
	UpsertCommitVerification(ctx context.Context, db reposdb.DBTX, arg reposdb.UpsertCommitVerificationParams) error
}

// WriteResult persists a verification Result into the cache table.
// Wraps the sqlc-generated UpsertCommitVerification so the Result
// → params translation lives in one place.
//
// Concurrency: UpsertCommitVerification's ON CONFLICT clause makes
// this safe to call from the orchestrator and the backfill worker
// against the same (repo_id, commit_oid) without losing data — the
// most-recent invocation wins.
func WriteResult(
	ctx context.Context,
	q CacheWriter,
	db reposdb.DBTX,
	repoID int64,
	commitOID string,
	kind Kind,
	r Result,
) error {
	return q.UpsertCommitVerification(ctx, db, reposdb.UpsertCommitVerificationParams{
		RepoID:           repoID,
		CommitOid:        commitOID,
		Reason:           string(r.Reason),
		Verified:         r.Verified,
		SignerUserID:     nullableInt64(r.SignerUserID),
		SignerSubkeyID:   nullableInt64(r.SignerSubkeyID),
		Kind:             string(kind),
		SignatureArmored: nullableText(r.Signature),
		Payload:          r.Payload,
	})
}

func nullableInt64(v int64) pgtype.Int8 {
	if v == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: v, Valid: true}
}

func nullableText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
