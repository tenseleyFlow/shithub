// SPDX-License-Identifier: AGPL-3.0-or-later

package issues

import (
	"context"
	"errors"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// Two patterns:
//
//   #N            — same-repo reference. Captured digits.
//   owner/repo#N  — cross-repo reference. Captured owner, repo, digits.
//
// Word-boundary on the leading side so we don't grab "abc#1". The N is
// limited to 1–9 leading digit + arbitrary digits, capped at 9 total to
// keep crazy parses bounded.
var (
	reCrossRepoIssueRef = regexp.MustCompile(`(?:^|[^\w/])([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)#([0-9]{1,9})\b`)
	reSameRepoIssueRef  = regexp.MustCompile(`(?:^|[^\w/])#([0-9]{1,9})\b`)
)

// insertReferencesFromBody parses `body` for cross-references and
// records each one as an `issue_references` row plus a `referenced`
// event on the *target* issue. Best-effort: malformed refs are skipped
// silently. Self-references (target == source) are skipped to avoid
// noise on the same issue's timeline.
//
// `source` is the source issue (for repo scoping when resolving #N).
// `srcKind` is the issue_ref_source enum value. `srcObjectID` is the
// ID of the comment / issue / push event that originated the ref.
func insertReferencesFromBody(
	ctx context.Context,
	tx pgx.Tx,
	deps Deps,
	source issuesdb.Issue,
	body string,
	srcKind string,
	srcObjectID int64,
) error {
	if body == "" {
		return nil
	}

	q := issuesdb.New()
	repoQ := reposdb.New()
	userQ := usersdb.New()

	seen := map[int64]struct{}{}

	insertRef := func(targetID int64) error {
		if targetID == source.ID {
			return nil
		}
		if _, dup := seen[targetID]; dup {
			return nil
		}
		seen[targetID] = struct{}{}
		if err := q.InsertIssueReference(ctx, tx, issuesdb.InsertIssueReferenceParams{
			TargetIssueID:  targetID,
			SourceKind:     issuesdb.IssueRefSource(srcKind),
			SourceIssueID:  pgtype.Int8{Int64: source.ID, Valid: true},
			SourceObjectID: pgtype.Int8{Int64: srcObjectID, Valid: srcObjectID != 0},
		}); err != nil {
			return err
		}
		// Emit the timeline event on the *target* issue.
		if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
			IssueID:     targetID,
			ActorUserID: source.AuthorUserID,
			Kind:        "referenced",
			Meta:        []byte(`{"source_issue_id":` + strconv.FormatInt(source.ID, 10) + `}`),
			RefTargetID: pgtype.Int8{Int64: source.ID, Valid: true},
		}); err != nil {
			return err
		}
		return nil
	}

	// Cross-repo: owner/repo#N
	for _, m := range reCrossRepoIssueRef.FindAllStringSubmatch(body, -1) {
		owner, name, numStr := m[1], m[2], m[3]
		num, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			continue
		}
		u, err := userQ.GetUserByUsername(ctx, tx, owner)
		if err != nil {
			continue // org owners (S31) and unknown users get silently dropped
		}
		repo, err := repoQ.GetRepoByOwnerUserAndName(ctx, tx, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: u.ID, Valid: true},
			Name:        name,
		})
		if err != nil {
			continue
		}
		target, err := q.GetIssueByNumber(ctx, tx, issuesdb.GetIssueByNumberParams{
			RepoID: repo.ID, Number: num,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return err
		}
		if err := insertRef(target.ID); err != nil {
			return err
		}
	}

	// Same-repo: #N. Skip text positions already captured by the
	// cross-repo regex by stripping owner/repo#N matches first.
	stripped := reCrossRepoIssueRef.ReplaceAllString(body, " ")
	for _, m := range reSameRepoIssueRef.FindAllStringSubmatch(stripped, -1) {
		num, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		target, err := q.GetIssueByNumber(ctx, tx, issuesdb.GetIssueByNumberParams{
			RepoID: source.RepoID, Number: num,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return err
		}
		if err := insertRef(target.ID); err != nil {
			return err
		}
	}

	return nil
}

