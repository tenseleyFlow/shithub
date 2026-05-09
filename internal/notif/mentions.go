// SPDX-License-Identifier: AGPL-3.0-or-later

package notif

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/markdown"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// ResolveMentionUserIDs maps a slice of (possibly duplicate)
// `markdown.Mention` records to deduplicated user IDs. Unknown,
// suspended, and soft-deleted accounts are silently dropped — the
// fan-out worker should never produce notifications for accounts
// that wouldn't have read access. Result is stable-ordered by
// first-occurrence in the input.
//
// The lookup is one-by-one rather than batched: typical mention
// counts are in the single digits and the per-username GetUserByUsername
// is index-backed. Batching becomes worth it only above ~50 distinct
// mentions per body.
func ResolveMentionUserIDs(ctx context.Context, pool *pgxpool.Pool, ms []markdown.Mention) []int64 {
	if len(ms) == 0 {
		return nil
	}
	q := usersdb.New()
	seen := map[int64]struct{}{}
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		u, err := q.GetUserByUsername(ctx, pool, m.Username)
		if err != nil {
			continue
		}
		if u.SuspendedAt.Valid || u.DeletedAt.Valid {
			continue
		}
		if _, dup := seen[u.ID]; dup {
			continue
		}
		seen[u.ID] = struct{}{}
		out = append(out, u.ID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
