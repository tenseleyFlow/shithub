// SPDX-License-Identifier: AGPL-3.0-or-later

package dependencyupdates

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const defaultScheduleLocation = "UTC"

var (
	ErrInvalidSchedule = errors.New("dependency update schedule invalid")

	dependencyUpdateCronParser = cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
	)
)

// NextRunAfter returns the next update check time after from. It follows the
// GitHub-compatible interval meanings shithub supports: daily means weekdays,
// weekly defaults to Monday, longer intervals run on the first day of their
// cadence month, and cron uses standard 5-field crontab syntax.
func NextRunAfter(schedule Schedule, from time.Time, seed string) (time.Time, error) {
	interval := strings.TrimSpace(strings.ToLower(schedule.Interval))
	if interval == "cron" {
		return nextCronRun(schedule.Cronjob, from)
	}

	loc, err := scheduleLocation(schedule.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	hour, minute, err := parseScheduleClock(schedule.Time, seed)
	if err != nil {
		return time.Time{}, err
	}

	localFrom := from.In(loc)
	switch interval {
	case "daily":
		return nextDailyRun(localFrom, hour, minute, loc).UTC(), nil
	case "weekly":
		weekday, err := parseScheduleWeekday(defaultString(schedule.Day, "monday"))
		if err != nil {
			return time.Time{}, err
		}
		return nextWeeklyRun(localFrom, weekday, hour, minute, loc).UTC(), nil
	case "monthly":
		return nextMonthSetRun(localFrom, []time.Month{
			time.January, time.February, time.March, time.April,
			time.May, time.June, time.July, time.August,
			time.September, time.October, time.November, time.December,
		}, hour, minute, loc).UTC(), nil
	case "quarterly":
		return nextMonthSetRun(localFrom, []time.Month{
			time.January, time.April, time.July, time.October,
		}, hour, minute, loc).UTC(), nil
	case "semiannually":
		return nextMonthSetRun(localFrom, []time.Month{
			time.January, time.July,
		}, hour, minute, loc).UTC(), nil
	case "yearly":
		return nextMonthSetRun(localFrom, []time.Month{time.January}, hour, minute, loc).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("%w: unsupported interval %q", ErrInvalidSchedule, schedule.Interval)
	}
}

func nextCronRun(expr string, from time.Time) (time.Time, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return time.Time{}, fmt.Errorf("%w: cronjob is required", ErrInvalidSchedule)
	}
	sched, err := dependencyUpdateCronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid cronjob: %v", ErrInvalidSchedule, err)
	}
	return sched.Next(from.UTC()).UTC(), nil
}

func scheduleLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultScheduleLocation
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q", ErrInvalidSchedule, name)
	}
	return loc, nil
}

func parseScheduleClock(value string, seed string) (int, int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		minuteOfDay := deterministicMinuteOfDay(seed)
		return minuteOfDay / 60, minuteOfDay % 60, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: time must use HH:MM", ErrInvalidSchedule)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: time hour must be numeric", ErrInvalidSchedule)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: time minute must be numeric", ErrInvalidSchedule)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%w: time must be between 00:00 and 23:59", ErrInvalidSchedule)
	}
	return hour, minute, nil
}

func deterministicMinuteOfDay(seed string) int {
	if seed == "" {
		seed = "shithub-dependency-updates"
	}
	sum := sha256.Sum256([]byte(seed))
	return int(binary.BigEndian.Uint16(sum[:2]) % (24 * 60))
}

func parseScheduleWeekday(value string) (time.Weekday, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sunday":
		return time.Sunday, nil
	case "monday":
		return time.Monday, nil
	case "tuesday":
		return time.Tuesday, nil
	case "wednesday":
		return time.Wednesday, nil
	case "thursday":
		return time.Thursday, nil
	case "friday":
		return time.Friday, nil
	case "saturday":
		return time.Saturday, nil
	default:
		return time.Sunday, fmt.Errorf("%w: unsupported day %q", ErrInvalidSchedule, value)
	}
}

func nextDailyRun(from time.Time, hour int, minute int, loc *time.Location) time.Time {
	candidate := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, loc)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for candidate.Weekday() == time.Saturday || candidate.Weekday() == time.Sunday {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func nextWeeklyRun(from time.Time, weekday time.Weekday, hour int, minute int, loc *time.Location) time.Time {
	days := (int(weekday) - int(from.Weekday()) + 7) % 7
	candidate := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, loc).AddDate(0, 0, days)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate
}

func nextMonthSetRun(from time.Time, months []time.Month, hour int, minute int, loc *time.Location) time.Time {
	for year := from.Year(); year <= from.Year()+2; year++ {
		for _, month := range months {
			candidate := time.Date(year, month, 1, hour, minute, 0, 0, loc)
			if candidate.After(from) {
				return candidate
			}
		}
	}
	return time.Date(from.Year()+3, months[0], 1, hour, minute, 0, 0, loc)
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
