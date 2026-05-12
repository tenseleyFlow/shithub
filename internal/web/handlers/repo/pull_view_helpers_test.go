// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
)

func TestDefaultMergeSubjectUsesPullNumberAndHeadOwner(t *testing.T) {
	t.Parallel()
	got := defaultMergeSubject(pullsdb.GetPullRequestByRepoAndNumberRow{
		INumber: 138,
		HeadRef: "s41h/dogfood-ga-audit",
	}, "tenseleyFlow")
	want := "Merge pull request #138 from tenseleyFlow/s41h/dogfood-ga-audit"
	if got != want {
		t.Fatalf("defaultMergeSubject = %q, want %q", got, want)
	}
}

func TestIssueTimelineRowsDecoratesMergedEvent(t *testing.T) {
	t.Parallel()
	meta, err := json.Marshal(map[string]string{
		"method": "merge",
		"commit": "5eb70568f00dbabe111111111111111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := (*Handlers)(nil).issueTimelineRows(nil, []issuesdb.IssueEvent{{
		Kind:        "merged",
		Meta:        meta,
		ActorUserID: pgtype.Int8{Int64: 7, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: time.Unix(10, 0), Valid: true},
	}}, nil, nil, func(id int64) string {
		if id == 7 {
			return "mfwolffe"
		}
		return ""
	})
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Message != "merged commit" || row.CommitSHA == "" || row.ShortCommit != "5eb7056" || row.ActorName != "mfwolffe" {
		t.Fatalf("merged row = %#v", row)
	}
}
