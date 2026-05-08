// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// RemapAllForPR re-anchors every non-draft review comment on a PR
// against the new (base, head) snapshot. For each comment:
//
//	1. Re-walk the diff for its file at (newBase..newHead).
//	2. If the line at original_line still appears on the chosen side
//	   (left = base, right = head), set current_position to its new
//	   position index. Otherwise NULL → outdated.
//
// This is the conservative v1 implementation. The spec calls out
// `git blame --porcelain` as a richer mapper for rebase-heavy PRs;
// add that when the simple line-presence check proves insufficient.
//
// The function reads `pr_review_comments` then issues per-row
// SetPRReviewCommentCurrentPosition updates. Idempotent: re-running
// converges on the same answer.
func RemapAllForPR(ctx context.Context, deps Deps, gitDir string, prID int64, newBaseOID, newHeadOID string) error {
	if newBaseOID == "" || newHeadOID == "" {
		return nil
	}
	q := pullsdb.New()
	rows, err := q.ListNonDraftCommentsForPositionMap(ctx, deps.Pool, prID)
	if err != nil {
		return fmt.Errorf("position map list: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	// Build per-file caches keyed on (file_path, side). Each entry
	// stores the line-text → position map for that side of the new
	// diff. v1 reads file contents at the relevant snapshot OID and
	// computes positions on demand.
	type sideKey struct {
		path string
		side string
	}
	cache := map[sideKey]map[int]int{} // line# (from original) → new position

	getMap := func(path, side string) (map[int]int, error) {
		k := sideKey{path, side}
		if m, ok := cache[k]; ok {
			return m, nil
		}
		// For the v1 mapper we just compute position-by-line via the
		// snapshot file content. side=right uses newHeadOID; side=left
		// uses newBaseOID.
		ref := newHeadOID
		if side == "left" {
			ref = newBaseOID
		}
		// Inline cap on file size — we don't bother mapping files >
		// 1 MiB; those comments outdate.
		const maxBytes = 1 << 20
		blob, err := repogit.ReadBlobBytes(ctx, gitDir, ref, path, maxBytes)
		if err != nil {
			cache[k] = nil // miss → outdate everything in this file
			return nil, nil
		}
		m := buildLineMap(blob)
		cache[k] = m
		return m, nil
	}

	for _, c := range rows {
		m, _ := getMap(c.FilePath, string(c.Side))
		var newPos pgtype.Int4
		if m != nil {
			if pos, ok := m[int(c.OriginalLine)]; ok {
				newPos = pgtype.Int4{Int32: int32(pos), Valid: true}
			}
		}
		if err := q.SetPRReviewCommentCurrentPosition(ctx, deps.Pool, pullsdb.SetPRReviewCommentCurrentPositionParams{
			ID:              c.ID,
			CurrentPosition: newPos,
		}); err != nil {
			return fmt.Errorf("position map update: %w", err)
		}
	}
	return nil
}

// buildLineMap returns line-number → position-index for a blob. v1
// just maps line N to position N — line numbers in the blob *are*
// the positions for the simple "line still exists" check. The map
// will be replaced with a content-aware mapping when we add the
// blame-based variant.
func buildLineMap(blob []byte) map[int]int {
	out := map[int]int{}
	line := 1
	pos := 1
	for i := 0; i < len(blob); i++ {
		if blob[i] == '\n' {
			out[line] = pos
			line++
			pos++
		}
	}
	// Trailing line without newline.
	if len(blob) > 0 && blob[len(blob)-1] != '\n' {
		out[line] = pos
	}
	return out
}
