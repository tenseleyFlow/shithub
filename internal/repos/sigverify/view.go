// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"context"
	"time"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// View is a render-friendly projection of a commit_verification_cache
// row. It carries everything the HTML badge partial and the REST
// `verification` object need; both consumers translate to their own
// surface shapes from here.
//
// "No cache row" maps to View{Verified: false, Reason: ReasonUnsigned}
// — gh's documented behavior for commits we haven't verified yet.
//
// "Cache row exists but invalidated_at != null" also maps to the
// unsigned-shaped default. Rationale: a stale row means the signing
// key was revoked between verification and now; pretending it's
// verified would be wrong. Treating it as "needs re-verify, render
// as unsigned for now" lets the backfill worker catch up
// asynchronously without the render path doing crypto work inline.
type View struct {
	Verified       bool
	Reason         Reason
	Signature      string
	Payload        []byte
	VerifiedAt     *time.Time
	SignerUserID   int64
	SignerSubkeyID int64
}

// UnsignedView is the canonical "no cache row" / "row invalidated"
// shape. Mirrors gh's "unsigned" rendering.
func UnsignedView() View {
	return View{Verified: false, Reason: ReasonUnsigned}
}

// ViewFromRow translates a sqlc cache row into the View shape.
// Honors the invalidated_at flag by returning UnsignedView for
// stale rows.
func ViewFromRow(row reposdb.CommitVerificationCache) View {
	if row.InvalidatedAt.Valid {
		return UnsignedView()
	}
	view := View{
		Verified: row.Verified,
		Reason:   Reason(row.Reason),
		Payload:  row.Payload,
	}
	if row.SignatureArmored.Valid {
		view.Signature = row.SignatureArmored.String
	}
	if row.VerifiedAt.Valid {
		t := row.VerifiedAt.Time
		view.VerifiedAt = &t
	}
	if row.SignerUserID.Valid {
		view.SignerUserID = row.SignerUserID.Int64
	}
	if row.SignerSubkeyID.Valid {
		view.SignerSubkeyID = row.SignerSubkeyID.Int64
	}
	return view
}

// LoadViewsForOIDs batch-reads verification rows for a set of commit
// OIDs and returns a map keyed by OID. OIDs without a cache row are
// absent from the map — the renderer treats absence as
// UnsignedView() via the LookupView helper.
//
// Used by the commit-list render path (both HTML and REST).
func LoadViewsForOIDs(ctx context.Context, db reposdb.DBTX, repoID int64, oids []string) (map[string]View, error) {
	if len(oids) == 0 {
		return map[string]View{}, nil
	}
	q := reposdb.New()
	rows, err := q.GetCommitVerificationsForOIDs(ctx, db, reposdb.GetCommitVerificationsForOIDsParams{
		RepoID: repoID,
		Oids:   oids,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]View, len(rows))
	for _, row := range rows {
		out[row.CommitOid] = ViewFromRow(row)
	}
	return out, nil
}

// LoadView reads a single commit's verification row. Returns
// UnsignedView() when no row exists or the row is invalidated.
func LoadView(ctx context.Context, db reposdb.DBTX, repoID int64, oid string) (View, error) {
	q := reposdb.New()
	row, err := q.GetCommitVerification(ctx, db, reposdb.GetCommitVerificationParams{
		RepoID:    repoID,
		CommitOid: oid,
	})
	if err != nil {
		// Distinguishing "not found" from "DB error" matters: not-
		// found is the unsigned-default fast path; DB error should
		// propagate so the caller can decide between fail-open
		// (render anyway) and fail-closed (5xx). Caller handles via
		// errors.Is(err, pgx.ErrNoRows).
		return UnsignedView(), err
	}
	return ViewFromRow(row), nil
}

// LookupView returns the View for the given OID from a map, falling
// back to UnsignedView() on miss. Template-friendly helper so the
// partial doesn't have to do nil-checks inline.
func LookupView(m map[string]View, oid string) View {
	v, ok := m[oid]
	if !ok {
		return UnsignedView()
	}
	return v
}
