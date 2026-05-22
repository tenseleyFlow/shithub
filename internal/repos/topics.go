// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"context"
	"errors"
	"regexp"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// MaxTopicsPerRepo and MaxTopicLength match the spec's day-1 lean
// (20 topics, 50 chars each). The shape regex covers GitHub's topic
// rules: lowercase letters/digits/hyphens, can't start/end with a
// hyphen, 1–50 chars.
const (
	MaxTopicsPerRepo = 20
	MaxTopicLength   = 50
)

// Errors surfaced by topic mutation.
var (
	ErrTooManyTopics = errors.New("repos: too many topics (max 20)")
	ErrInvalidTopic  = errors.New("repos: topic must be lowercase letters, digits, and hyphens; 1-50 chars")
)

var topicRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,48}[a-z0-9])?$`)

// NormalizeTopics dedups an incoming list and validates each entry.
// Returns the cleaned slice (stable-ordered, dedup'd) or the first
// validation error.
//
// H3 (H12): byte-exact match. Pre-fix the `ToLower(TrimSpace(...))`
// chain silently normalized "FOOBAR" / " foo " to "foobar"/"foo"; raw
// API callers got 422 for "FOOBAR" while CLI callers silently
// succeeded after lowercasing. Now both surface 422 — the topic regex
// at line 29 already restricts to `[a-z0-9-]` so uppercase / whitespace
// fail the same way as embedded tabs.
func NormalizeTopics(raw []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if t == "" {
			continue
		}
		if !topicRE.MatchString(t) {
			return nil, ErrInvalidTopic
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > MaxTopicsPerRepo {
		return nil, ErrTooManyTopics
	}
	return out, nil
}

// ReplaceTopics atomically swaps the topic set for a repo. The
// orchestrator owns tx ordering: DELETE + per-row INSERT in one tx
// so a partial failure rolls back. The unique index on (repo_id,
// topic) makes the inserts idempotent against a concurrent run.
func ReplaceTopics(ctx context.Context, deps Deps, repoID int64, topics []string) error {
	clean, err := NormalizeTopics(topics)
	if err != nil {
		return err
	}
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	q := reposdb.New()
	if err := q.ReplaceRepoTopics(ctx, tx, repoID); err != nil {
		return err
	}
	for _, t := range clean {
		if err := q.InsertRepoTopic(ctx, tx, reposdb.InsertRepoTopicParams{
			RepoID: repoID, Topic: t,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
