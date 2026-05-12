// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"testing"
	"time"
)

func TestContributionGridExpandsToCompleteWeeks(t *testing.T) {
	t.Parallel()
	start, weeks := contributionGrid(
		time.Date(2028, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2028, time.December, 31, 0, 0, 0, 0, time.UTC),
	)
	if want := time.Date(2027, time.December, 26, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("grid start = %s, want %s", start.Format(time.DateOnly), want.Format(time.DateOnly))
	}
	if weeks != 54 {
		t.Fatalf("weeks = %d, want 54", weeks)
	}
}

func TestContributionWeekMonthLabelUsesFirstOfMonthColumn(t *testing.T) {
	t.Parallel()
	weekStart := time.Date(2026, time.April, 26, 0, 0, 0, 0, time.UTC)
	if got := contributionWeekMonthLabel(weekStart, false); got != "May" {
		t.Fatalf("label = %q, want May", got)
	}
	if got := contributionWeekMonthLabel(weekStart.AddDate(0, 0, 7), false); got != "" {
		t.Fatalf("next label = %q, want empty", got)
	}
}
