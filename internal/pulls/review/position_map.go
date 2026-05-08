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
// against the new (base, head) snapshot. For each comment we do a
// content-aware check:
//
//  1. Read the line text at (original_commit_sha, file_path,
//     original_line). This is what the comment was written against.
//  2. Read the same line number in the new blob (newHeadOID for
//     side=right; newBaseOID for side=left).
//  3. If the bytes are identical → keep current_position = original_line.
//     If not → current_position = NULL (outdated). The comment still
//     renders in the conversation timeline; the Files tab hides it
//     until the user clicks "Show outdated."
//
// This is the conservative v1 mapper. Lines that have been re-indented,
// shifted by an insertion above, or merely had a comma added all
// outdate — that's the right default. The spec calls out
// `git blame --porcelain` as a richer mapper for rebase-heavy PRs;
// add that when the simple presence check proves too aggressive.
//
// Idempotent: re-running converges on the same answer.
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

	// Per-(ref, path) blob cache so we read each blob at most once per
	// PR, regardless of how many comments anchor into it.
	type blobKey struct {
		ref  string
		path string
	}
	cache := map[blobKey][]byte{}
	loadBlob := func(ref, path string) []byte {
		k := blobKey{ref, path}
		if b, ok := cache[k]; ok {
			return b
		}
		// We don't bother mapping files > 1 MiB; those comments outdate.
		const maxBytes = 1 << 20
		blob, err := repogit.ReadBlobBytes(ctx, gitDir, ref, path, maxBytes)
		if err != nil {
			cache[k] = nil
			return nil
		}
		cache[k] = blob
		return blob
	}

	for _, c := range rows {
		newRef := newHeadOID
		if c.Side == pullsdb.PrReviewSideLeft {
			newRef = newBaseOID
		}
		original := loadBlob(c.OriginalCommitSha, c.FilePath)
		current := loadBlob(newRef, c.FilePath)

		var newPos pgtype.Int4
		if line, ok := lineAt(original, int(c.OriginalLine)); ok {
			if cur, ok := lineAt(current, int(c.OriginalLine)); ok && bytesEqual(line, cur) {
				newPos = pgtype.Int4{Int32: c.OriginalLine, Valid: true}
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

// lineAt returns the bytes of line N (1-indexed) in blob, excluding
// the trailing newline. Returns (nil, false) when the blob is nil/empty
// or N is out of range.
func lineAt(blob []byte, n int) ([]byte, bool) {
	if blob == nil || n < 1 {
		return nil, false
	}
	start, line := 0, 1
	for i := 0; i < len(blob); i++ {
		if blob[i] == '\n' {
			if line == n {
				return blob[start:i], true
			}
			line++
			start = i + 1
		}
	}
	// Trailing line without terminator.
	if line == n && start < len(blob) {
		return blob[start:], true
	}
	return nil, false
}

// bytesEqual is a tiny escape hatch so the test fixtures stay readable.
// `bytes.Equal` would do, but pulling the whole package in for one call
// adds noise to the position-map dependency graph.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
