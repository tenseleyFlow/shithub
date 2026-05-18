// SPDX-License-Identifier: AGPL-3.0-or-later

// Package traffic records GitHub-style repository Traffic aggregates.
//
// The package intentionally stores aggregate counters plus scoped visitor
// hashes only. Raw IP addresses, user agents, authenticated user IDs, and full
// referrer URLs stay out of the database.
package traffic

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	pathpkg "path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

const (
	DefaultWindowDays = 14
	defaultTopLimit   = 10
	maxPathLen        = 2048
	maxReferrerLen    = 255

	metricView     = "view"
	metricClone    = "clone"
	metricPath     = "path"
	metricReferrer = "referrer"
)

// Event is one observed repository access. Caller-provided VisitorKey can be
// a stable in-memory identifier such as "user:123" or "anon:<ip>|<ua>"; only a
// scoped SHA-256 digest is persisted.
type Event struct {
	RepoID       int64
	OccurredAt   time.Time
	VisitorKey   string
	Path         string
	ReferrerHost string
}

// Summary is the rendered Traffic page model.
type Summary struct {
	Days            []Day
	TopPaths        []TopEntry
	TopReferrers    []TopEntry
	TotalViews      int64
	UniqueViews     int64
	TotalClones     int64
	UniqueClones    int64
	MaxViews        int64
	MaxClones       int64
	HasViews        bool
	HasClones       bool
	HasPathData     bool
	HasReferrerData bool
}

// Day is one day in the 14-day traffic graph.
type Day struct {
	Date          time.Time
	Label         string
	Views         int64
	UniqueViews   int64
	Clones        int64
	UniqueClones  int64
	ViewBarWidth  int
	CloneBarWidth int
}

// TopEntry is a top path or referrer row.
type TopEntry struct {
	Name        string
	Views       int64
	UniqueViews int64
	BarWidth    int
}

// RecordView increments daily view totals and, when supplied, top path and
// referrer aggregates.
func RecordView(ctx context.Context, pool *pgxpool.Pool, event Event) error {
	if pool == nil {
		return errors.New("traffic: nil pool")
	}
	if event.RepoID == 0 {
		return errors.New("traffic: missing repo id")
	}
	day := dateFor(event.OccurredAt)
	path := NormalizePath(event.Path)
	referrer := NormalizeReferrerHost(event.ReferrerHost)
	visitor := normalizeVisitorKey(event.VisitorKey)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := reposdb.New()
	uniqueView, err := insertUnique(ctx, q, tx, event.RepoID, day, metricView, "", visitor)
	if err != nil {
		return fmt.Errorf("unique view: %w", err)
	}
	if err := q.UpsertRepoTrafficDailyView(ctx, tx, reposdb.UpsertRepoTrafficDailyViewParams{
		RepoID:      event.RepoID,
		Day:         day,
		UniqueViews: boolDelta(uniqueView),
	}); err != nil {
		return fmt.Errorf("daily view: %w", err)
	}

	uniquePath, err := insertUnique(ctx, q, tx, event.RepoID, day, metricPath, path, visitor)
	if err != nil {
		return fmt.Errorf("unique path: %w", err)
	}
	if err := q.UpsertRepoTrafficPathView(ctx, tx, reposdb.UpsertRepoTrafficPathViewParams{
		RepoID:      event.RepoID,
		Day:         day,
		Path:        path,
		UniqueViews: boolDelta(uniquePath),
	}); err != nil {
		return fmt.Errorf("path view: %w", err)
	}

	if referrer != "" {
		uniqueReferrer, err := insertUnique(ctx, q, tx, event.RepoID, day, metricReferrer, referrer, visitor)
		if err != nil {
			return fmt.Errorf("unique referrer: %w", err)
		}
		if err := q.UpsertRepoTrafficReferrerView(ctx, tx, reposdb.UpsertRepoTrafficReferrerViewParams{
			RepoID:      event.RepoID,
			Day:         day,
			Referrer:    referrer,
			UniqueViews: boolDelta(uniqueReferrer),
		}); err != nil {
			return fmt.Errorf("referrer view: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// RecordClone increments daily git clone/fetch totals.
func RecordClone(ctx context.Context, pool *pgxpool.Pool, event Event) error {
	if pool == nil {
		return errors.New("traffic: nil pool")
	}
	if event.RepoID == 0 {
		return errors.New("traffic: missing repo id")
	}
	day := dateFor(event.OccurredAt)
	visitor := normalizeVisitorKey(event.VisitorKey)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := reposdb.New()
	uniqueClone, err := insertUnique(ctx, q, tx, event.RepoID, day, metricClone, "", visitor)
	if err != nil {
		return fmt.Errorf("unique clone: %w", err)
	}
	if err := q.UpsertRepoTrafficDailyClone(ctx, tx, reposdb.UpsertRepoTrafficDailyCloneParams{
		RepoID:       event.RepoID,
		Day:          day,
		UniqueClones: boolDelta(uniqueClone),
	}); err != nil {
		return fmt.Errorf("daily clone: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// LoadSummary returns the GitHub-style 14-day traffic window.
func LoadSummary(ctx context.Context, pool *pgxpool.Pool, repoID int64, now time.Time) (Summary, error) {
	return LoadSummaryWithLimit(ctx, pool, repoID, now, DefaultWindowDays, defaultTopLimit)
}

// LoadSummaryWithLimit is exported for focused tests and future admin views.
func LoadSummaryWithLimit(ctx context.Context, pool *pgxpool.Pool, repoID int64, now time.Time, days int, limit int32) (Summary, error) {
	if pool == nil {
		return Summary{}, errors.New("traffic: nil pool")
	}
	if repoID == 0 {
		return Summary{}, errors.New("traffic: missing repo id")
	}
	if days <= 0 {
		days = DefaultWindowDays
	}
	if limit <= 0 {
		limit = defaultTopLimit
	}

	start := startDate(now).AddDate(0, 0, -(days - 1))
	since := pgtype.Date{Time: start, Valid: true}
	q := reposdb.New()
	rows, err := q.ListRepoTrafficDaily(ctx, pool, reposdb.ListRepoTrafficDailyParams{
		RepoID: repoID,
		Day:    since,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("daily: %w", err)
	}
	paths, err := q.ListRepoTrafficPaths(ctx, pool, reposdb.ListRepoTrafficPathsParams{
		RepoID: repoID,
		Day:    since,
		Limit:  limit,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("paths: %w", err)
	}
	referrers, err := q.ListRepoTrafficReferrers(ctx, pool, reposdb.ListRepoTrafficReferrersParams{
		RepoID: repoID,
		Day:    since,
		Limit:  limit,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("referrers: %w", err)
	}

	byDay := make(map[string]reposdb.RepoTrafficDaily, len(rows))
	for _, row := range rows {
		byDay[dateKey(row.Day.Time)] = row
	}

	out := Summary{Days: make([]Day, 0, days)}
	for i := 0; i < days; i++ {
		current := start.AddDate(0, 0, i)
		day := Day{Date: current, Label: current.Format("Jan 2")}
		if row, ok := byDay[dateKey(current)]; ok {
			day.Views = row.Views
			day.UniqueViews = row.UniqueViews
			day.Clones = row.Clones
			day.UniqueClones = row.UniqueClones
		}
		out.TotalViews += day.Views
		out.UniqueViews += day.UniqueViews
		out.TotalClones += day.Clones
		out.UniqueClones += day.UniqueClones
		if day.Views > out.MaxViews {
			out.MaxViews = day.Views
		}
		if day.Clones > out.MaxClones {
			out.MaxClones = day.Clones
		}
		out.Days = append(out.Days, day)
	}
	out.HasViews = out.TotalViews > 0
	out.HasClones = out.TotalClones > 0

	for i := range out.Days {
		out.Days[i].ViewBarWidth = percent(out.Days[i].Views, out.MaxViews)
		out.Days[i].CloneBarWidth = percent(out.Days[i].Clones, out.MaxClones)
	}

	out.TopPaths = make([]TopEntry, 0, len(paths))
	var maxPathViews int64
	for _, row := range paths {
		if row.Views > maxPathViews {
			maxPathViews = row.Views
		}
		out.TopPaths = append(out.TopPaths, TopEntry{Name: row.Path, Views: row.Views, UniqueViews: row.UniqueViews})
	}
	for i := range out.TopPaths {
		out.TopPaths[i].BarWidth = percent(out.TopPaths[i].Views, maxPathViews)
	}
	out.HasPathData = len(out.TopPaths) > 0

	out.TopReferrers = make([]TopEntry, 0, len(referrers))
	var maxReferrerViews int64
	for _, row := range referrers {
		if row.Views > maxReferrerViews {
			maxReferrerViews = row.Views
		}
		out.TopReferrers = append(out.TopReferrers, TopEntry{Name: row.Referrer, Views: row.Views, UniqueViews: row.UniqueViews})
	}
	for i := range out.TopReferrers {
		out.TopReferrers[i].BarWidth = percent(out.TopReferrers[i].Views, maxReferrerViews)
	}
	out.HasReferrerData = len(out.TopReferrers) > 0

	return out, nil
}

// NormalizePath converts a repository-local request path to the compact form
// GitHub shows in "Popular content".
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/"
	}
	if u, err := url.Parse(p); err == nil && u.Path != "" {
		p = u.Path
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = pathpkg.Clean(p)
	if p == "." {
		p = "/"
	}
	p = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, p)
	if p == "" {
		p = "/"
	}
	if len(p) > maxPathLen {
		p = p[:maxPathLen]
	}
	return p
}

// ExternalReferrerHost returns a lowercased host for an off-site Referer
// header. Same-host and malformed referrers are dropped.
func ExternalReferrerHost(raw, requestHost string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := normalizeHost(u.Host)
	if host == "" || host == normalizeHost(requestHost) {
		return ""
	}
	return NormalizeReferrerHost(host)
}

// NormalizeReferrerHost is used by tests and callers that already parsed a
// host from a trusted layer.
func NormalizeReferrerHost(host string) string {
	host = normalizeHost(host)
	if len(host) > maxReferrerLen {
		host = host[:maxReferrerLen]
	}
	return host
}

func insertUnique(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, repoID int64, day pgtype.Date, metric, key, visitorKey string) (bool, error) {
	inserted, err := q.InsertRepoTrafficUnique(ctx, db, reposdb.InsertRepoTrafficUniqueParams{
		RepoID:      repoID,
		Day:         day,
		Metric:      metric,
		Key:         key,
		VisitorHash: visitorHash(repoID, day, metric, key, visitorKey),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return inserted, err
}

func visitorHash(repoID int64, day pgtype.Date, metric, key, visitorKey string) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("repo=%d;day=%s;metric=%s;key=%s;visitor=%s",
		repoID, dateKey(day.Time), metric, key, visitorKey)))
	return sum[:]
}

func dateFor(t time.Time) pgtype.Date {
	return pgtype.Date{Time: startDate(t), Valid: true}
}

func startDate(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func dateKey(t time.Time) string {
	return startDate(t).Format("2006-01-02")
}

func boolDelta(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func normalizeVisitorKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	return v
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	raw = strings.TrimSuffix(raw, ".")
	if len(raw) > maxReferrerLen {
		raw = raw[:maxReferrerLen]
	}
	return raw
}

func percent(value, max int64) int {
	if value <= 0 || max <= 0 {
		return 0
	}
	p := int((value * 100) / max)
	if p <= 0 {
		return 1
	}
	if p > 100 {
		return 100
	}
	return p
}
