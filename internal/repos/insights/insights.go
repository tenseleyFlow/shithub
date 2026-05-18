// SPDX-License-Identifier: AGPL-3.0-or-later

// Package insights computes cached repository insights from git history.
// The web layer renders snapshots produced by the worker so request paths
// do not walk large histories synchronously.
package insights

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

const (
	SchemaVersion     = 1
	DefaultMaxCommits = 5000
)

type BuildOptions struct {
	Ref        string
	MaxCommits int
	Now        time.Time
}

type Snapshot struct {
	Version          int              `json:"version"`
	GeneratedAt      time.Time        `json:"generated_at"`
	DefaultBranch    string           `json:"default_branch"`
	HeadSHA          string           `json:"head_sha"`
	CommitCount      int              `json:"commit_count"`
	ContributorCount int              `json:"contributor_count"`
	Additions        int64            `json:"additions"`
	Deletions        int64            `json:"deletions"`
	LatestCommitAt   time.Time        `json:"latest_commit_at,omitempty"`
	Pulse            Pulse            `json:"pulse"`
	Contributors     []Contributor    `json:"contributors"`
	CommitActivity   []WeeklyActivity `json:"commit_activity"`
	CodeFrequency    []WeeklyActivity `json:"code_frequency"`
}

type Pulse struct {
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	Commits      int       `json:"commits"`
	Contributors int       `json:"contributors"`
	Additions    int64     `json:"additions"`
	Deletions    int64     `json:"deletions"`
	FilesChanged int       `json:"files_changed"`
}

type Contributor struct {
	Name         string    `json:"name"`
	Email        string    `json:"email,omitempty"`
	Commits      int       `json:"commits"`
	Additions    int64     `json:"additions"`
	Deletions    int64     `json:"deletions"`
	LatestCommit time.Time `json:"latest_commit,omitempty"`
	BarWidth     int       `json:"bar_width"`
}

type WeeklyActivity struct {
	WeekStart string `json:"week_start"`
	Label     string `json:"label"`
	Commits   int    `json:"commits"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
	BarWidth  int    `json:"bar_width"`
}

type CommitStat struct {
	OID          string
	ShortOID     string
	AuthorName   string
	AuthorEmail  string
	AuthorWhen   time.Time
	Subject      string
	Additions    int64
	Deletions    int64
	FilesChanged int
}

// Build resolves ref, reads a bounded git log with numstat data, and
// aggregates the resulting commits into a render-ready snapshot.
func Build(ctx context.Context, gitDir string, opts BuildOptions) (Snapshot, error) {
	ref := strings.TrimSpace(opts.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	maxCommits := opts.MaxCommits
	if maxCommits <= 0 {
		maxCommits = DefaultMaxCommits
	}

	head, err := repogit.ResolveRefOID(ctx, gitDir, ref)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return EmptySnapshot(ref, now), nil
		}
		return Snapshot{}, err
	}
	commits, err := readCommitStats(ctx, gitDir, ref, maxCommits)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Aggregate(commits, now)
	snap.DefaultBranch = ref
	snap.HeadSHA = head
	return snap, nil
}

func EmptySnapshot(defaultBranch string, now time.Time) Snapshot {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	since := now.AddDate(0, 0, -30)
	return Snapshot{
		Version:       SchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: defaultBranch,
		Pulse: Pulse{
			Since: since,
			Until: now,
		},
	}
}

// Aggregate is pure so tests can exercise graph behavior without
// constructing git repositories.
func Aggregate(commits []CommitStat, now time.Time) Snapshot {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	snap := EmptySnapshot("", now)
	snap.CommitCount = len(commits)

	byContributor := map[string]*Contributor{}
	pulseContributors := map[string]struct{}{}
	byWeek := map[time.Time]*WeeklyActivity{}
	pulseSince := now.AddDate(0, 0, -30)

	var maxContributorCommits int
	var maxWeekCommits int
	for _, c := range commits {
		when := c.AuthorWhen.UTC()
		if snap.LatestCommitAt.IsZero() || when.After(snap.LatestCommitAt) {
			snap.LatestCommitAt = when
		}
		snap.Additions += c.Additions
		snap.Deletions += c.Deletions

		key := contributorKey(c)
		if key != "" {
			contributor := byContributor[key]
			if contributor == nil {
				contributor = &Contributor{
					Name:  strings.TrimSpace(c.AuthorName),
					Email: strings.TrimSpace(c.AuthorEmail),
				}
				if contributor.Name == "" {
					contributor.Name = contributor.Email
				}
				if contributor.Name == "" {
					contributor.Name = "Unknown author"
				}
				byContributor[key] = contributor
			}
			contributor.Commits++
			contributor.Additions += c.Additions
			contributor.Deletions += c.Deletions
			if contributor.LatestCommit.IsZero() || when.After(contributor.LatestCommit) {
				contributor.LatestCommit = when
			}
			if contributor.Commits > maxContributorCommits {
				maxContributorCommits = contributor.Commits
			}
			if !when.Before(pulseSince) {
				pulseContributors[key] = struct{}{}
			}
		}

		weekStart := startOfUTCWeek(when)
		week := byWeek[weekStart]
		if week == nil {
			week = &WeeklyActivity{
				WeekStart: weekStart.Format("2006-01-02"),
				Label:     weekStart.Format("Jan 2, 2006"),
			}
			byWeek[weekStart] = week
		}
		week.Commits++
		week.Additions += c.Additions
		week.Deletions += c.Deletions
		if week.Commits > maxWeekCommits {
			maxWeekCommits = week.Commits
		}

		if !when.Before(pulseSince) {
			snap.Pulse.Commits++
			snap.Pulse.Additions += c.Additions
			snap.Pulse.Deletions += c.Deletions
			snap.Pulse.FilesChanged += c.FilesChanged
		}
	}
	snap.Pulse.Contributors = len(pulseContributors)
	snap.ContributorCount = len(byContributor)

	contributors := make([]Contributor, 0, len(byContributor))
	for _, contributor := range byContributor {
		c := *contributor
		c.BarWidth = scaledWidth(c.Commits, maxContributorCommits)
		contributors = append(contributors, c)
	}
	sort.Slice(contributors, func(i, j int) bool {
		if contributors[i].Commits != contributors[j].Commits {
			return contributors[i].Commits > contributors[j].Commits
		}
		return strings.ToLower(contributors[i].Name) < strings.ToLower(contributors[j].Name)
	})
	if len(contributors) > 25 {
		contributors = contributors[:25]
	}
	snap.Contributors = contributors

	weekStarts := make([]time.Time, 0, len(byWeek))
	for weekStart := range byWeek {
		weekStarts = append(weekStarts, weekStart)
	}
	sort.Slice(weekStarts, func(i, j int) bool { return weekStarts[i].Before(weekStarts[j]) })
	for _, weekStart := range weekStarts {
		week := *byWeek[weekStart]
		week.BarWidth = scaledWidth(week.Commits, maxWeekCommits)
		snap.CommitActivity = append(snap.CommitActivity, week)
		snap.CodeFrequency = append(snap.CodeFrequency, week)
	}
	return snap
}

func readCommitStats(ctx context.Context, gitDir, ref string, maxCommits int) ([]CommitStat, error) {
	const sep = "\x1f"
	const rec = "\x1e"
	format := rec + strings.Join([]string{"%H", "%h", "%an", "%ae", "%at", "%s"}, sep)
	args := []string{
		"-C", gitDir, "log",
		"--max-count=" + strconv.Itoa(maxCommits),
		"--date-order",
		"--format=" + format,
		"--numstat",
		ref,
		"--",
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // gitDir/ref are validated repo path and argv values.
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log insights: %w", err)
	}
	return ParseLogNumstat(out)
}

func ParseLogNumstat(out []byte) ([]CommitStat, error) {
	const sep = "\x1f"
	records := bytes.Split(out, []byte("\x1e"))
	commits := make([]CommitStat, 0, len(records))
	for _, raw := range records {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		meta := strings.SplitN(lines[0], sep, 6)
		if len(meta) != 6 {
			return nil, fmt.Errorf("git log insights: malformed metadata %q", lines[0])
		}
		ts, err := strconv.ParseInt(meta[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git log insights: malformed timestamp %q: %w", meta[4], err)
		}
		commit := CommitStat{
			OID:         meta[0],
			ShortOID:    meta[1],
			AuthorName:  meta[2],
			AuthorEmail: meta[3],
			AuthorWhen:  time.Unix(ts, 0).UTC(),
			Subject:     meta[5],
		}
		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			additions, deletions, ok := parseNumstat(parts[0], parts[1])
			if !ok {
				commit.FilesChanged++
				continue
			}
			commit.Additions += additions
			commit.Deletions += deletions
			commit.FilesChanged++
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func parseNumstat(rawAdditions, rawDeletions string) (int64, int64, bool) {
	if rawAdditions == "-" || rawDeletions == "-" {
		return 0, 0, false
	}
	additions, err := strconv.ParseInt(rawAdditions, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	deletions, err := strconv.ParseInt(rawDeletions, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return additions, deletions, true
}

func contributorKey(c CommitStat) string {
	if email := strings.ToLower(strings.TrimSpace(c.AuthorEmail)); email != "" {
		return "email:" + email
	}
	if name := strings.ToLower(strings.TrimSpace(c.AuthorName)); name != "" {
		return "name:" + name
	}
	return ""
}

func scaledWidth(value, max int) int {
	if value <= 0 || max <= 0 {
		return 0
	}
	width := value * 100 / max
	if width < 3 {
		return 3
	}
	if width > 100 {
		return 100
	}
	return width
}

func startOfUTCWeek(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}
