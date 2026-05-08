// SPDX-License-Identifier: AGPL-3.0-or-later

package social

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

// EmitEvent is the canonical seam for inserting a row into
// `domain_events`. Other packages (issues, pulls, repos lifecycle)
// will call this rather than reaching into the sqlc query directly,
// so the event-shape contract has one place to evolve as S29 and
// S33 wire up their consumers.
//
// Pass repoID = 0 for user-scoped events (where there's no repo
// involved). The handler / orchestrator owns the public-flag
// decision: public-repo events should set true; private-repo events
// must set false.
type EmitParams struct {
	ActorUserID int64
	Kind        string
	RepoID      int64 // 0 for user-scoped
	SourceKind  string
	SourceID    int64
	Public      bool
	Payload     []byte // already-marshaled JSON; pass `[]byte("{}")` for empty
}

// Emit writes one row to domain_events. Non-fatal at the caller's
// discretion; orchestrators typically log on error rather than
// failing the whole transaction (the in-band action is more
// important than the event log).
func Emit(ctx context.Context, deps Deps, p EmitParams) error {
	payload := p.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	_, err := socialdb.New().InsertDomainEvent(ctx, deps.Pool, socialdb.InsertDomainEventParams{
		ActorUserID: pgInt(p.ActorUserID),
		Kind:        p.Kind,
		RepoID:      pgInt(p.RepoID),
		SourceKind:  p.SourceKind,
		SourceID:    p.SourceID,
		Public:      p.Public,
		Payload:     payload,
	})
	return err
}

func pgInt(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: v != 0}
}
