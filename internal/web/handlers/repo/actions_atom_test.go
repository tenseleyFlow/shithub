// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

func TestWriteActionsAtomEscapesRunsAndUsesLatestUpdate(t *testing.T) {
	ts1 := pgtype.Timestamptz{Time: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC), Valid: true}
	ts2 := pgtype.Timestamptz{Time: time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC), Valid: true}
	var buf bytes.Buffer
	writeActionsAtom(&buf, "alice", "demo", []actionsdb.ListWorkflowRunsForRepoRow{
		{
			ID:            12,
			RunIndex:      7,
			WorkflowFile:  ".shithub/workflows/ci.yml",
			WorkflowName:  "CI <release>",
			HeadSha:       strings.Repeat("a", 40),
			HeadRef:       "refs/heads/trunk",
			Event:         actionsdb.WorkflowRunEventPush,
			Status:        actionsdb.WorkflowRunStatusCompleted,
			Conclusion:    actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
			ActorUsername: "dev<one>",
			CreatedAt:     ts2,
			UpdatedAt:     ts1,
		},
	}, time.Date(2026, 5, 12, 8, 0, 0, 0, time.UTC))

	type feed struct {
		XMLName xml.Name `xml:"feed"`
		Title   string   `xml:"title"`
		Updated string   `xml:"updated"`
		Entries []struct {
			Title   string `xml:"title"`
			Updated string `xml:"updated"`
			Author  struct {
				Name string `xml:"name"`
			} `xml:"author"`
			Summary string `xml:"summary"`
			Link    struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	var got feed
	if err := xml.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal atom: %v\n%s", err, buf.String())
	}
	if got.Title != "alice/demo Actions runs" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Updated != "2026-05-12T10:00:00Z" {
		t.Fatalf("updated = %q", got.Updated)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d", len(got.Entries))
	}
	if got.Entries[0].Title != "CI <release> #7 success" {
		t.Fatalf("entry title = %q", got.Entries[0].Title)
	}
	if got.Entries[0].Author.Name != "dev<one>" {
		t.Fatalf("author = %q", got.Entries[0].Author.Name)
	}
	if got.Entries[0].Link.Href != "/alice/demo/actions/runs/7" {
		t.Fatalf("link = %q", got.Entries[0].Link.Href)
	}
	if !strings.Contains(got.Entries[0].Summary, "Conclusion: success") {
		t.Fatalf("summary = %q", got.Entries[0].Summary)
	}
}
