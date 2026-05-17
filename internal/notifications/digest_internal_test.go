// SPDX-License-Identifier: AGPL-3.0-or-later

package notifications

import (
	"testing"
	"time"

	notifdb "github.com/tenseleyFlow/shithub/internal/notif/sqlc"
)

// TestNextSendTime_Daily codifies the daily-cadence math.
// Subtleties: a base time of exactly 09:00 should advance to the
// next day (09:00 is not strictly after 09:00). 09:01 → next 09:00
// of the same day is in the past, so wrap.
func TestNextSendTime_Daily(t *testing.T) {
	t.Parallel()
	freq := notifdb.UserNotificationDigestFrequencyDaily

	cases := []struct {
		name     string
		base     time.Time
		hour     int
		wantHour int
		wantDay  int
	}{
		{
			name:     "before today's hour",
			base:     time.Date(2026, 5, 17, 6, 0, 0, 0, time.UTC),
			hour:     9,
			wantHour: 9,
			wantDay:  17,
		},
		{
			name:     "at today's hour exactly — must roll forward",
			base:     time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC),
			hour:     9,
			wantHour: 9,
			wantDay:  18,
		},
		{
			name:     "past today's hour",
			base:     time.Date(2026, 5, 17, 14, 30, 0, 0, time.UTC),
			hour:     9,
			wantHour: 9,
			wantDay:  18,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := NextSendTime(c.base, freq, c.hour, 0)
			if got.Hour() != c.wantHour {
				t.Errorf("Hour = %d, want %d", got.Hour(), c.wantHour)
			}
			if got.Day() != c.wantDay {
				t.Errorf("Day = %d, want %d", got.Day(), c.wantDay)
			}
			if !got.After(c.base) {
				t.Errorf("result %v not after base %v", got, c.base)
			}
		})
	}
}

// TestNextSendTime_Weekly: walks forward to the chosen DOW + hour.
// 2026-05-17 is a Sunday (weekday 0).
func TestNextSendTime_Weekly(t *testing.T) {
	t.Parallel()
	freq := notifdb.UserNotificationDigestFrequencyWeekly
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC) // Sunday 10:00

	// Wanting "Tuesday at 09:00 UTC" — should land on 2026-05-19.
	got := NextSendTime(base, freq, 9, 2 /* Tue */)
	if got.Weekday() != time.Tuesday {
		t.Errorf("Weekday = %s, want Tuesday", got.Weekday())
	}
	if got.Day() != 19 {
		t.Errorf("Day = %d, want 19", got.Day())
	}
	if got.Hour() != 9 {
		t.Errorf("Hour = %d, want 9", got.Hour())
	}

	// Same DOW as base (Sunday) but later in the day → next week.
	got = NextSendTime(base, freq, 9, 0 /* Sun */)
	if got.Weekday() != time.Sunday {
		t.Errorf("Weekday = %s, want Sunday", got.Weekday())
	}
	if got.Day() != 24 {
		t.Errorf("Day = %d, want 24 (next Sun)", got.Day())
	}
}

// TestPlural is trivial but a small regression here changes every
// subject line — worth a guard.
func TestPlural(t *testing.T) {
	t.Parallel()
	if plural(1) != "" {
		t.Errorf("plural(1) = %q, want empty", plural(1))
	}
	if plural(0) != "s" || plural(2) != "s" {
		t.Errorf("plural(0)/(2) should be 's'")
	}
}
