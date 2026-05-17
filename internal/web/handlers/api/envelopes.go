// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"strings"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// S60 — GitHub-compat response envelopes. The flat `author_id: 1`
// shape we shipped initially breaks every gh-compatible client that
// expects `user: {login, id, ...}` (the shithub-cli dogfood audit
// found 6 broken flows in one sitting; see A01-dogfood-audit). We
// keep the legacy flat field on each response for one release cycle
// to soften the transition; new clients should consume the envelope.

// userEnvelope is the minimal GitHub-compat user shape. The CLI's
// internal/api.User type accepts both `login` and `username` for
// migration safety, so we emit `login` (gh-canonical) and the CLI
// will work either way.
type userEnvelope struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Type      string `json:"type,omitempty"`
	HTMLURL   string `json:"html_url,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// presentUserEnvelope converts a sqlc User into the public-facing
// envelope. Anonymous / deleted users render as a sentinel so the
// caller can decide whether to omit the field or display "ghost".
// Returns nil when the input user is the zero value (id==0) so the
// `omitempty` on the parent struct's User pointer kicks in.
func presentUserEnvelope(u usersdb.User, baseURL string) *userEnvelope {
	if u.ID == 0 || u.Username == "" {
		return nil
	}
	out := &userEnvelope{
		ID:    u.ID,
		Login: u.Username,
		Type:  "User",
	}
	if base := strings.TrimRight(baseURL, "/"); base != "" {
		out.HTMLURL = base + "/" + u.Username
	}
	return out
}

// resolveUserEnvelope fetches one user by id and returns the envelope.
// nil id → nil envelope (the caller is responsible for omitempty).
// On any error (deleted/missing/anything) returns nil so the response
// stays well-formed; the legacy `author_id` field still carries the
// raw FK for callers that need it.
func (h *Handlers) resolveUserEnvelope(ctx context.Context, id int64) *userEnvelope {
	if id == 0 {
		return nil
	}
	u, err := h.q.GetUserByID(ctx, h.d.Pool, id)
	if err != nil {
		return nil
	}
	return presentUserEnvelope(u, h.d.BaseURL)
}

// labelEnvelope is the GitHub-compat label shape. The pre-S60 surface
// returned `labels: ["bug"]` (string array) which made the CLI's
// strongly-typed issues.Label{Name, Color, ...} decoder panic on
// every labeled issue. We now emit the full row.
type labelEnvelope struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

// presentLabelEnvelopes converts a slice of sqlc Label rows into the
// public envelope. Order is preserved (server-side query controls it).
func presentLabelEnvelopes(rows []issuesdb.Label) []labelEnvelope {
	if len(rows) == 0 {
		return nil
	}
	out := make([]labelEnvelope, 0, len(rows))
	for _, l := range rows {
		out = append(out, labelEnvelope{
			ID:          l.ID,
			Name:        l.Name,
			Color:       l.Color,
			Description: l.Description,
		})
	}
	return out
}

// resolveUserEnvelopesBatch returns a map id→*userEnvelope for the
// provided id set in a single query. Used by list endpoints so we
// don't fan out one GetUserByID per row. Missing ids are absent from
// the map; callers map[id] returning nil is the "user unknown" path.
func (h *Handlers) resolveUserEnvelopesBatch(ctx context.Context, ids []int64) map[int64]*userEnvelope {
	if len(ids) == 0 {
		return nil
	}
	// Deduplicate so we don't ask the DB for the same id twice when
	// a list has 50 issues all authored by the same user.
	seen := make(map[int64]struct{}, len(ids))
	uniq := ids[:0:0]
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return nil
	}
	rows, err := h.q.ListUsersByIDs(ctx, h.d.Pool, uniq)
	if err != nil {
		return nil
	}
	out := make(map[int64]*userEnvelope, len(rows))
	for _, u := range rows {
		out[u.ID] = presentUserEnvelope(u, h.d.BaseURL)
	}
	return out
}
