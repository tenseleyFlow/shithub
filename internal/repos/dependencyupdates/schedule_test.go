// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencyupdates

import (
	"errors"
	"testing"
	"time"
)

func TestNextRunAfterDailySkipsWeekend(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC) // Friday
	got, err := NextRunAfter(Schedule{Interval: "daily", Time: "09:00"}, from, "repo")
	if err != nil {
		t.Fatalf("NextRunAfter: %v", err)
	}
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC) // Monday
	if !got.Equal(want) {
		t.Fatalf("next daily run = %s, want %s", got, want)
	}
}

func TestNextRunAfterWeeklyUsesTimezoneAndDefaultMonday(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 5, 18, 12, 59, 0, 0, time.UTC) // Monday, 08:59 in New York
	got, err := NextRunAfter(Schedule{
		Interval: "weekly",
		Time:     "09:00",
		Timezone: "America/New_York",
	}, from, "repo")
	if err != nil {
		t.Fatalf("NextRunAfter: %v", err)
	}
	want := time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next weekly run = %s, want %s", got, want)
	}
}

func TestNextRunAfterQuarterlyFirstCadenceMonth(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)
	got, err := NextRunAfter(Schedule{Interval: "quarterly", Time: "04:30"}, from, "repo")
	if err != nil {
		t.Fatalf("NextRunAfter: %v", err)
	}
	want := time.Date(2026, 4, 1, 4, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next quarterly run = %s, want %s", got, want)
	}
}

func TestNextRunAfterCron(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	got, err := NextRunAfter(Schedule{Interval: "cron", Cronjob: "15 6 * * 1"}, from, "repo")
	if err != nil {
		t.Fatalf("NextRunAfter: %v", err)
	}
	want := time.Date(2026, 5, 25, 6, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next cron run = %s, want %s", got, want)
	}
}

func TestNextRunAfterDeterministicDefaultTime(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	first, err := NextRunAfter(Schedule{Interval: "daily"}, from, "same-seed")
	if err != nil {
		t.Fatalf("first NextRunAfter: %v", err)
	}
	second, err := NextRunAfter(Schedule{Interval: "daily"}, from, "same-seed")
	if err != nil {
		t.Fatalf("second NextRunAfter: %v", err)
	}
	if !first.Equal(second) {
		t.Fatalf("default scheduled time is not deterministic: %s vs %s", first, second)
	}
	if !first.After(from) {
		t.Fatalf("default scheduled time = %s, want after %s", first, from)
	}
}

func TestNextRunAfterRejectsInvalidSchedule(t *testing.T) {
	t.Parallel()
	tests := []Schedule{
		{Interval: "weekly", Day: "funday"},
		{Interval: "daily", Time: "24:00"},
		{Interval: "daily", Timezone: "Mars/Olympus"},
		{Interval: "cron", Cronjob: "bad"},
	}
	from := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	for _, schedule := range tests {
		if _, err := NextRunAfter(schedule, from, "repo"); !errors.Is(err, ErrInvalidSchedule) {
			t.Fatalf("NextRunAfter(%+v) error = %v, want ErrInvalidSchedule", schedule, err)
		}
	}
}
