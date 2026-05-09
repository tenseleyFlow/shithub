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

// CreateParams describes a check-run create request. Defaults
// (per spec): status='queued', app_slug='external'.
type CreateParams struct {
	RepoID      int64
	HeadSHA     string
	AppSlug     string // "" → "external"
	Name        string
	Status      string // "" → "queued"
	Conclusion  string // optional
	StartedAt   time.Time
	CompletedAt time.Time
	DetailsURL  string
	Output      Output
	ExternalID  string // optional; enables idempotent retry-create
}

// Create inserts (or returns existing-by-external-id) a check run,
// auto-creating the matching suite. Suite rollup is recomputed inside
// the same tx so the API response reflects the latest derived state.
//
// `external_id` semantics:
//   - When empty: always insert a new run.
//   - When non-empty: lookup by (repo, head_sha, name, external_id);
//     if found, return existing without insert. This makes retries
//     by external CI safe.
func Create(ctx context.Context, deps Deps, p CreateParams) (checksdb.CheckRun, error) {
	if err := validateCreate(&p); err != nil {
		return checksdb.CheckRun{}, err
	}
	q := checksdb.New()

	// Idempotency check.
	if p.ExternalID != "" {
		existing, err := q.GetCheckRunByExternalID(ctx, deps.Pool, checksdb.GetCheckRunByExternalIDParams{
			RepoID:     p.RepoID,
			HeadSha:    p.HeadSHA,
			Name:       p.Name,
			ExternalID: pgtype.Text{String: p.ExternalID, Valid: true},
		})
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return checksdb.CheckRun{}, err
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

	suite, err := q.GetOrCreateCheckSuite(ctx, tx, checksdb.GetOrCreateCheckSuiteParams{
		RepoID:  p.RepoID,
		HeadSha: p.HeadSHA,
		AppSlug: p.AppSlug,
	})
	if err != nil {
		return checksdb.CheckRun{}, fmt.Errorf("suite: %w", err)
	}

	outputJSON, err := json.Marshal(p.Output)
	if err != nil {
		return checksdb.CheckRun{}, fmt.Errorf("output marshal: %w", err)
	}

	startedAt := pgtype.Timestamptz{}
	if !p.StartedAt.IsZero() {
		startedAt = pgtype.Timestamptz{Time: p.StartedAt, Valid: true}
	}
	completedAt := pgtype.Timestamptz{}
	if !p.CompletedAt.IsZero() {
		completedAt = pgtype.Timestamptz{Time: p.CompletedAt, Valid: true}
	}
	conclusion := checksdb.NullCheckConclusion{}
	if p.Conclusion != "" {
		conclusion = checksdb.NullCheckConclusion{
			CheckConclusion: checksdb.CheckConclusion(p.Conclusion),
			Valid:           true,
		}
	}
	extID := pgtype.Text{}
	if p.ExternalID != "" {
		extID = pgtype.Text{String: p.ExternalID, Valid: true}
	}

	run, err := q.CreateCheckRun(ctx, tx, checksdb.CreateCheckRunParams{
		SuiteID:     suite.ID,
		RepoID:      p.RepoID,
		HeadSha:     p.HeadSHA,
		Name:        p.Name,
		Status:      checksdb.CheckStatus(p.Status),
		Conclusion:  conclusion,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		DetailsUrl:  p.DetailsURL,
		Output:      outputJSON,
		ExternalID:  extID,
	})
	if err != nil {
		return checksdb.CheckRun{}, fmt.Errorf("insert run: %w", err)
	}

	if err := rollupSuiteInTx(ctx, tx, suite.ID); err != nil {
		return checksdb.CheckRun{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return checksdb.CheckRun{}, err
	}
	committed = true
	return run, nil
}

// validateCreate normalizes defaults + applies per-field bounds.
func validateCreate(p *CreateParams) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return ErrEmptyName
	}
	if len(p.Name) > 200 {
		return ErrNameTooLong
	}
	p.HeadSHA = strings.TrimSpace(p.HeadSHA)
	if len(p.HeadSHA) < 7 || len(p.HeadSHA) > 64 {
		return ErrShortHeadSHA
	}
	p.AppSlug = strings.TrimSpace(p.AppSlug)
	if p.AppSlug == "" {
		p.AppSlug = "external"
	}
	p.Status = strings.TrimSpace(p.Status)
	if p.Status == "" {
		p.Status = "queued"
	}
	if !validStatus(p.Status) {
		return ErrInvalidStatus
	}
	if p.Conclusion != "" && !validConclusion(p.Conclusion) {
		return ErrInvalidConclusion
	}
	if p.Status == "completed" && p.Conclusion == "" {
		return ErrCompletedNeedsConclusion
	}
	if len(p.Output.Text) > MaxOutputTextBytes {
		return ErrOutputTextTooLarge
	}
	if len(p.Output.Summary) > MaxOutputSummaryBytes {
		return ErrOutputSummaryTooLarge
	}
	return nil
}
