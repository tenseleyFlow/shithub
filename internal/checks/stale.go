// SPDX-License-Identifier: AGPL-3.0-or-later

package checks

import (
	"context"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

// MarkStaleForPreviousHead flips suites on `prevHeadSHA` whose status
// is not yet completed to (completed, conclusion='stale'). Called from
// push:process when a head ref moves AND the matching protection rule
// has `dismiss_stale_status_checks_on_push = true`.
//
// "Status complete + conclusion stale" preserves the audit trail —
// the runs themselves stay readable, but the suite no longer counts
// as in-progress.
func MarkStaleForPreviousHead(ctx context.Context, deps Deps, repoID int64, prevHeadSHA string) (int, error) {
	if prevHeadSHA == "" {
		return 0, nil
	}
	q := checksdb.New()
	ids, err := q.ListCheckSuiteIDsForHead(ctx, deps.Pool, checksdb.ListCheckSuiteIDsForHeadParams{
		RepoID: repoID, HeadSha: prevHeadSHA,
	})
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := q.MarkCheckSuiteStale(ctx, deps.Pool, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
