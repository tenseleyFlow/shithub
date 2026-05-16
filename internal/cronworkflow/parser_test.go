// SPDX-License-Identifier: AGPL-3.0-or-later

package cronworkflow_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
)

func TestParseExpr_AcceptsValid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"* * * * *",    // every minute
		"0 * * * *",    // every hour on the hour
		"*/15 * * * *", // every 15 min
		"0 0 * * 0",    // weekly Sunday midnight
		"30 9 1 * *",   // 09:30 on the 1st of every month
		"0 12 * * 1-5", // weekday noons
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if _, err := cronworkflow.ParseExpr(c); err != nil {
				t.Errorf("ParseExpr(%q): %v", c, err)
			}
		})
	}
}

func TestParseExpr_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",            // empty
		"not a cron",  // garbage
		"* * *",       // too few fields
		"* * * * * *", // too many (seconds-resolution disabled by design)
		"60 * * * *",  // minute out of range
		"@hourly",     // descriptors disabled — matches the GitHub Actions semantic
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			_, err := cronworkflow.ParseExpr(c)
			if err == nil {
				t.Errorf("ParseExpr(%q): want error, got nil", c)
				return
			}
			if !errors.Is(err, cronworkflow.ErrInvalidCronExpr) {
				t.Errorf("ParseExpr(%q): want ErrInvalidCronExpr, got %v", c, err)
			}
		})
	}
}

func TestNextTick_AdvancesFromBase(t *testing.T) {
	t.Parallel()
	// At 09:00:30 UTC, the next "* * * * *" tick is 09:01:00.
	base := time.Date(2026, 5, 16, 9, 0, 30, 0, time.UTC)
	got, err := cronworkflow.NextTick("* * * * *", base)
	if err != nil {
		t.Fatalf("NextTick: %v", err)
	}
	want := time.Date(2026, 5, 16, 9, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextTick: got %s, want %s", got, want)
	}
}

func TestNextTick_HonorsHourlyCadence(t *testing.T) {
	t.Parallel()
	// "0 * * * *" from 09:00:00 should jump to 10:00:00 (not stay).
	base := time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	got, err := cronworkflow.NextTick("0 * * * *", base)
	if err != nil {
		t.Fatalf("NextTick: %v", err)
	}
	want := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextTick: got %s, want %s", got, want)
	}
}
