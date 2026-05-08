// SPDX-License-Identifier: AGPL-3.0-or-later

package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

// UpdateParams describes a check-run patch. Empty optional fields
// keep the existing value (rather than clearing); callers that want
// to clear a field set it to the zero string explicitly via the
// dedicated *Set boolean — keeps the JSON contract simple while
// avoiding pointer-everywhere fields.
//
// Status transitions are validated: queued/pending → in_progress →
// completed; completed requires a conclusion. Re-applying the same
// status is a no-op, which makes external-system retries safe.
type UpdateParams struct {
	RunID       int64
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
	DetailsURL  string
	Output      Output

	HasStatus      bool
	HasConclusion  bool
	HasStartedAt   bool
	HasCompletedAt bool
	HasDetailsURL  bool
	HasOutput      bool
}

// Update patches a check run + recomputes its suite's rollup. Fields
// that aren't marked Has* keep their current value.
func Update(ctx context.Context, deps Deps, p UpdateParams) (checksdb.CheckRun, error) {
	q := checksdb.New()
	cur, err := q.GetCheckRun(ctx, deps.Pool, p.RunID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return checksdb.CheckRun{}, ErrCheckRunNotFound
		}
		return checksdb.CheckRun{}, err
	}

	// Resolve effective values.
	status := string(cur.Status)
	if p.HasStatus {
		s := strings.TrimSpace(p.Status)
		if !validStatus(s) {
			return checksdb.CheckRun{}, ErrInvalidStatus
		}
		status = s
	}
	conclusion := ""
	if cur.Conclusion.Valid {
		conclusion = string(cur.Conclusion.CheckConclusion)
	}
	if p.HasConclusion {
		c := strings.TrimSpace(p.Conclusion)
		if c != "" && !validConclusion(c) {
			return checksdb.CheckRun{}, ErrInvalidConclusion
		}
		conclusion = c
	}
	if status == "completed" && conclusion == "" {
		return checksdb.CheckRun{}, ErrCompletedNeedsConclusion
	}
	startedAt := cur.StartedAt
	if p.HasStartedAt {
		if p.StartedAt.IsZero() {
			startedAt = pgtype.Timestamptz{}
		} else {
			startedAt = pgtype.Timestamptz{Time: p.StartedAt, Valid: true}
		}
	}
	completedAt := cur.CompletedAt
	if p.HasCompletedAt {
		if p.CompletedAt.IsZero() {
			completedAt = pgtype.Timestamptz{}
		} else {
			completedAt = pgtype.Timestamptz{Time: p.CompletedAt, Valid: true}
		}
	}
	detailsURL := cur.DetailsUrl
	if p.HasDetailsURL {
		detailsURL = p.DetailsURL
	}
	outputBytes := cur.Output
	if p.HasOutput {
		if len(p.Output.Text) > MaxOutputTextBytes {
			return checksdb.CheckRun{}, ErrOutputTextTooLarge
		}
		if len(p.Output.Summary) > MaxOutputSummaryBytes {
			return checksdb.CheckRun{}, ErrOutputSummaryTooLarge
		}
		outputBytes, err = json.Marshal(p.Output)
		if err != nil {
			return checksdb.CheckRun{}, fmt.Errorf("output marshal: %w", err)
		}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return checksdb.CheckRun{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	conclusionParam := checksdb.NullCheckConclusion{}
	if conclusion != "" {
		conclusionParam = checksdb.NullCheckConclusion{
			CheckConclusion: checksdb.CheckConclusion(conclusion),
			Valid:           true,
		}
	}

	if err := q.UpdateCheckRun(ctx, tx, checksdb.UpdateCheckRunParams{
		ID:          p.RunID,
		Status:      checksdb.CheckStatus(status),
		Conclusion:  conclusionParam,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		DetailsUrl:  detailsURL,
		Output:      outputBytes,
	}); err != nil {
		return checksdb.CheckRun{}, fmt.Errorf("update run: %w", err)
	}
	if err := rollupSuiteInTx(ctx, tx, cur.SuiteID); err != nil {
		return checksdb.CheckRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return checksdb.CheckRun{}, err
	}
	committed = true

	updated, err := q.GetCheckRun(ctx, deps.Pool, p.RunID)
	if err != nil {
		return checksdb.CheckRun{}, err
	}
	return updated, nil
}
