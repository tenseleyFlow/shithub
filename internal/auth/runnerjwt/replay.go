// SPDX-License-Identifier: AGPL-3.0-or-later

package runnerjwt

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

var ErrReplay = errors.New("runnerjwt: token replay")

// Consume records claims.JTI as used. It returns ErrReplay when the jti was
// already consumed, which callers translate to 401.
func Consume(ctx context.Context, db actionsdb.DBTX, claims Claims) error {
	runnerID, err := claims.RunnerID()
	if err != nil {
		return err
	}
	if err := validateClaims(claims); err != nil {
		return err
	}
	_, err = actionsdb.New().MarkRunnerJWTUsed(ctx, db, actionsdb.MarkRunnerJWTUsedParams{
		Jti:       claims.JTI,
		RunnerID:  runnerID,
		JobID:     claims.JobID,
		RunID:     claims.RunID,
		RepoID:    claims.RepoID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Unix(claims.Exp, 0), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReplay
	}
	return err
}
