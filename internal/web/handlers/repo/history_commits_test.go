// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/url"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/identity"
)

func TestGroupCommitRowsByDay(t *testing.T) {
	rows := []commitRow{
		testCommitRow("a1", "Ada", "ada@example.com", time.Date(2026, 5, 10, 16, 0, 0, 0, time.UTC)),
		testCommitRow("b2", "Ada", "ada@example.com", time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
		testCommitRow("c3", "Ben", "ben@example.com", time.Date(2026, 4, 24, 18, 0, 0, 0, time.UTC)),
	}

	groups := groupCommitRows(rows)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].Title != "May 10, 2026" {
		t.Fatalf("first title = %q, want May 10, 2026", groups[0].Title)
	}
	if len(groups[0].Rows) != 2 {
		t.Fatalf("first group rows = %d, want 2", len(groups[0].Rows))
	}
	if groups[1].Title != "April 24, 2026" {
		t.Fatalf("second title = %q, want April 24, 2026", groups[1].Title)
	}
}

func TestCommitAuthorFiltersDeduplicateAndPreserveQuery(t *testing.T) {
	base := url.Values{
		"path":  {"cmd/shithubd/main.go"},
		"until": {"2026-05-10"},
	}
	rows := []commitRow{
		newCommitRow(git.Commit{
			OID:         "a1",
			ShortOID:    "a1",
			AuthorName:  "Matthew Forrester Wolffe",
			AuthorEmail: "mfwolffe@example.com",
			AuthorWhen:  time.Date(2026, 5, 10, 16, 0, 0, 0, time.UTC),
			Subject:     "one",
		}, identity.Resolved{User: true, Username: "mfwolffe", AvatarURL: "/avatars/mfwolffe"}),
		newCommitRow(git.Commit{
			OID:         "b2",
			ShortOID:    "b2",
			AuthorName:  "Matthew Forrester Wolffe",
			AuthorEmail: "mfwolffe@example.com",
			AuthorWhen:  time.Date(2026, 5, 10, 15, 0, 0, 0, time.UTC),
			Subject:     "two",
		}, identity.Resolved{User: true, Username: "mfwolffe", AvatarURL: "/avatars/mfwolffe"}),
		newCommitRow(git.Commit{
			OID:         "c3",
			ShortOID:    "c3",
			AuthorName:  "espandonne",
			AuthorEmail: "espandonne@example.com",
			AuthorWhen:  time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC),
			Subject:     "three",
		}, identity.Resolved{}),
	}

	filters := commitAuthorFilters("tenseleyFlow", "shithub", "feature/x", base, rows, "mfwolffe")
	if len(filters) != 2 {
		t.Fatalf("got %d filters, want 2", len(filters))
	}
	if !filters[0].Active || filters[0].Label != "mfwolffe" {
		t.Fatalf("first filter = %+v, want active mfwolffe", filters[0])
	}
	u, err := url.Parse(filters[0].Href)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/tenseleyFlow/shithub/commits/feature/x" {
		t.Fatalf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("author") != "mfwolffe" || q.Get("path") != "cmd/shithubd/main.go" || q.Get("until") != "2026-05-10" {
		t.Fatalf("unexpected query: %s", u.RawQuery)
	}
}

func TestCommitCalendarBuildsMonthGridAndLinks(t *testing.T) {
	base := url.Values{"author": {"mfwolffe"}}
	selected := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 10, 16, 0, 0, 0, time.UTC)

	cal := commitCalendar("tenseleyFlow", "shithub", "trunk", base, selected, "", nil, now)
	if cal.MonthLabel != "May" || cal.YearLabel != "2026" {
		t.Fatalf("month = %s %s, want May 2026", cal.MonthLabel, cal.YearLabel)
	}
	if len(cal.Weeks) != 6 || len(cal.Weeks[0]) != 7 {
		t.Fatalf("grid dimensions = %dx%d, want 6x7", len(cal.Weeks), len(cal.Weeks[0]))
	}
	if cal.Weeks[0][0].Label != "26" || cal.Weeks[0][0].InMonth {
		t.Fatalf("first cell = %+v, want muted Apr 26", cal.Weeks[0][0])
	}
	if cal.Weeks[0][5].Label != "1" || !cal.Weeks[0][5].InMonth {
		t.Fatalf("May 1 cell = %+v", cal.Weeks[0][5])
	}
	day := cal.Weeks[2][0]
	if day.Label != "10" || !day.IsSelected || !day.IsToday {
		t.Fatalf("selected day = %+v, want selected today May 10", day)
	}
	u, err := url.Parse(day.Href)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("until"); got != "2026-05-10" {
		t.Fatalf("until = %q, want 2026-05-10", got)
	}
	if got := u.Query().Get("author"); got != "mfwolffe" {
		t.Fatalf("author = %q, want mfwolffe", got)
	}
	prev, err := url.Parse(cal.PrevMonthHref)
	if err != nil {
		t.Fatal(err)
	}
	if got := prev.Query().Get("calendar_month"); got != "2026-04" {
		t.Fatalf("prev calendar_month = %q", got)
	}
	clear, err := url.Parse(cal.ClearHref)
	if err != nil {
		t.Fatal(err)
	}
	if clear.Query().Get("until") != "" || clear.Query().Get("since") != "" {
		t.Fatalf("clear href kept date filters: %s", clear.RawQuery)
	}
	if got := clear.Query().Get("author"); got != "mfwolffe" {
		t.Fatalf("clear author = %q, want mfwolffe", got)
	}
}

func TestParseUntilDateParamIncludesWholeDay(t *testing.T) {
	got := parseUntilDateParam("2026-05-10")
	want := time.Date(2026, 5, 10, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parseUntilDateParam = %s, want %s", got, want)
	}
}

func testCommitRow(oid, name, email string, when time.Time) commitRow {
	return newCommitRow(git.Commit{
		OID:         oid,
		ShortOID:    oid,
		AuthorName:  name,
		AuthorEmail: email,
		AuthorWhen:  when,
		Subject:     oid,
	}, identity.Resolved{})
}
