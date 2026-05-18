// SPDX-License-Identifier: AGPL-3.0-or-later

package insights

import (
	"testing"
	"time"
)

func TestAggregateBuildsContributorAndWeeklyStats(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	commits := []CommitStat{
		{
			OID:          "111",
			ShortOID:     "111",
			AuthorName:   "Alice",
			AuthorEmail:  "alice@example.com",
			AuthorWhen:   time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
			Additions:    10,
			Deletions:    2,
			FilesChanged: 2,
		},
		{
			OID:          "222",
			ShortOID:     "222",
			AuthorName:   "Bob",
			AuthorEmail:  "bob@example.com",
			AuthorWhen:   time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC),
			Additions:    5,
			Deletions:    1,
			FilesChanged: 1,
		},
		{
			OID:          "333",
			ShortOID:     "333",
			AuthorName:   "alice",
			AuthorEmail:  "ALICE@example.com",
			AuthorWhen:   time.Date(2026, 4, 7, 9, 0, 0, 0, time.UTC),
			Additions:    7,
			Deletions:    3,
			FilesChanged: 1,
		},
	}

	snap := Aggregate(commits, now)
	if snap.CommitCount != 3 {
		t.Fatalf("CommitCount=%d, want 3", snap.CommitCount)
	}
	if snap.ContributorCount != 2 {
		t.Fatalf("ContributorCount=%d, want 2", snap.ContributorCount)
	}
	if snap.Additions != 22 || snap.Deletions != 6 {
		t.Fatalf("totals additions=%d deletions=%d, want 22/6", snap.Additions, snap.Deletions)
	}
	if snap.Pulse.Commits != 2 || snap.Pulse.Contributors != 2 || snap.Pulse.FilesChanged != 3 {
		t.Fatalf("pulse=%+v, want 2 commits, 2 contributors, 3 files", snap.Pulse)
	}
	if len(snap.Contributors) != 2 {
		t.Fatalf("contributors len=%d, want 2", len(snap.Contributors))
	}
	if snap.Contributors[0].Name != "Alice" || snap.Contributors[0].Commits != 2 || snap.Contributors[0].BarWidth != 100 {
		t.Fatalf("top contributor=%+v, want Alice with 2 commits and full bar", snap.Contributors[0])
	}
	if len(snap.CommitActivity) != 3 {
		t.Fatalf("CommitActivity len=%d, want 3", len(snap.CommitActivity))
	}
	if snap.CommitActivity[0].WeekStart != "2026-04-06" {
		t.Fatalf("first week=%q, want 2026-04-06", snap.CommitActivity[0].WeekStart)
	}
	if snap.CommitActivity[2].WeekStart != "2026-05-18" {
		t.Fatalf("last week=%q, want 2026-05-18", snap.CommitActivity[2].WeekStart)
	}
}

func TestParseLogNumstat(t *testing.T) {
	out := []byte("\x1eabc\x1fab\x1fAlice\x1falice@example.com\x1f1779127200\x1fInitial commit\n" +
		"12\t2\tREADME.md\n" +
		"-\t-\timage.png\n" +
		"\x1edef\x1fde\x1fBob\x1fbob@example.com\x1f1779040800\x1fRemove code\n" +
		"0\t5\told.go\n")

	commits, err := ParseLogNumstat(out)
	if err != nil {
		t.Fatalf("ParseLogNumstat: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("len=%d, want 2", len(commits))
	}
	if commits[0].Additions != 12 || commits[0].Deletions != 2 || commits[0].FilesChanged != 2 {
		t.Fatalf("first commit stats=%+v, want additions/deletions/files 12/2/2", commits[0])
	}
	if commits[1].Subject != "Remove code" || commits[1].Deletions != 5 {
		t.Fatalf("second commit=%+v, want subject and deletions", commits[1])
	}
}
