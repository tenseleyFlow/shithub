// SPDX-License-Identifier: AGPL-3.0-or-later

package issues

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
)

// DefaultLabels is the seeded set applied to every new repo. Names +
// colors are GitHub-aligned so users feel at home.
var DefaultLabels = []LabelSeed{
	{"bug", "d73a4a", "Something isn't working"},
	{"documentation", "0075ca", "Improvements or additions to documentation"},
	{"duplicate", "cfd3d7", "This issue or pull request already exists"},
	{"enhancement", "a2eeef", "New feature or request"},
	{"good first issue", "7057ff", "Good for newcomers"},
	{"help wanted", "008672", "Extra attention is needed"},
	{"invalid", "e4e669", "This doesn't seem right"},
	{"question", "d876e3", "Further information is requested"},
	{"wontfix", "ffffff", "This will not be worked on"},
}

type LabelSeed struct{ Name, Color, Description string }

var reHexColor = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

// LabelCreateParams is input for CreateLabel.
type LabelCreateParams struct {
	RepoID      int64
	Name        string
	Color       string
	Description string
}

// CreateLabel validates and inserts a label. Color must be 6 hex chars.
func CreateLabel(ctx context.Context, deps Deps, p LabelCreateParams) (issuesdb.Label, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 50 {
		return issuesdb.Label{}, errors.New("issues: label name length 1–50")
	}
	color := normalizeColor(p.Color)
	if !reHexColor.MatchString(color) {
		return issuesdb.Label{}, ErrLabelInvalidColor
	}
	q := issuesdb.New()
	row, err := q.CreateLabel(ctx, deps.Pool, issuesdb.CreateLabelParams{
		RepoID:      p.RepoID,
		Name:        name,
		Color:       strings.ToLower(color),
		Description: p.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return issuesdb.Label{}, ErrLabelExists
		}
		return issuesdb.Label{}, err
	}
	return row, nil
}

// SeedDefaultLabels writes the DefaultLabels into the given repo on
// the supplied DBTX (typically the repo-create transaction). Existing
// rows are left alone — caller can re-run safely.
func SeedDefaultLabels(ctx context.Context, db issuesdb.DBTX, repoID int64) error {
	q := issuesdb.New()
	for _, l := range DefaultLabels {
		_, err := q.CreateLabel(ctx, db, issuesdb.CreateLabelParams{
			RepoID:      repoID,
			Name:        l.Name,
			Color:       l.Color,
			Description: l.Description,
		})
		if err != nil && !isUniqueViolation(err) {
			return err
		}
	}
	return nil
}

// LabelUpdateParams is input for UpdateLabel.
type LabelUpdateParams struct {
	ID          int64
	Name        string
	Color       string
	Description string
}

func UpdateLabel(ctx context.Context, deps Deps, p LabelUpdateParams) error {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 50 {
		return errors.New("issues: label name length 1–50")
	}
	color := normalizeColor(p.Color)
	if !reHexColor.MatchString(color) {
		return ErrLabelInvalidColor
	}
	q := issuesdb.New()
	if err := q.UpdateLabel(ctx, deps.Pool, issuesdb.UpdateLabelParams{
		ID:          p.ID,
		Name:        name,
		Color:       strings.ToLower(color),
		Description: p.Description,
	}); err != nil {
		if isUniqueViolation(err) {
			return ErrLabelExists
		}
		return err
	}
	return nil
}

// DeleteLabel removes a label.
func DeleteLabel(ctx context.Context, deps Deps, id int64) error {
	q := issuesdb.New()
	return q.DeleteLabel(ctx, deps.Pool, id)
}

// ApplyLabels replaces the issue's label set with `labelIDs`. Emits a
// `labeled` / `unlabeled` event for each delta. Caller is the actor.
func ApplyLabels(ctx context.Context, deps Deps, actorUserID, issueID int64, labelIDs []int64) error {
	q := issuesdb.New()
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
	current, err := q.ListLabelsOnIssue(ctx, tx, issueID)
	if err != nil {
		return err
	}
	want := map[int64]struct{}{}
	for _, id := range labelIDs {
		want[id] = struct{}{}
	}
	have := map[int64]struct{}{}
	for _, l := range current {
		have[l.ID] = struct{}{}
	}
	actor := pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0}
	for id := range want {
		if _, ok := have[id]; ok {
			continue
		}
		if err := q.AddIssueLabel(ctx, tx, issuesdb.AddIssueLabelParams{
			IssueID: issueID, LabelID: id, AppliedByUserID: actor,
		}); err != nil {
			return err
		}
		if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
			IssueID: issueID, ActorUserID: actor, Kind: "labeled",
			Meta: []byte(`{"label_id":` + strconv.FormatInt(id, 10) + `}`),
		}); err != nil {
			return err
		}
	}
	for id := range have {
		if _, ok := want[id]; ok {
			continue
		}
		if err := q.RemoveIssueLabel(ctx, tx, issuesdb.RemoveIssueLabelParams{
			IssueID: issueID, LabelID: id,
		}); err != nil {
			return err
		}
		if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
			IssueID: issueID, ActorUserID: actor, Kind: "unlabeled",
			Meta: []byte(`{"label_id":` + strconv.FormatInt(id, 10) + `}`),
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

func normalizeColor(c string) string {
	return strings.TrimPrefix(strings.TrimSpace(c), "#")
}

// isUniqueViolation maps Postgres SQLSTATE 23505 (unique_violation).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
