// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
)

// PrincipalKind discriminates the resolution result.
type PrincipalKind string

const (
	PrincipalUser PrincipalKind = "user"
	PrincipalOrg  PrincipalKind = "org"
)

// Principal is the resolution result. ID is the user_id when Kind is
// "user", org_id when "org".
type Principal struct {
	Slug string
	Kind PrincipalKind
	ID   int64
}

// ErrNoPrincipal is returned when /{slug} doesn't match a known
// principal. Web handlers translate this to 404.
var ErrNoPrincipal = errors.New("orgs: no principal for slug")

// Resolve answers the central /{slug} routing question with one
// indexed lookup. Returns ErrNoPrincipal for unknown slugs (handler
// 404s); other errors are surfaced as-is.
func Resolve(ctx context.Context, pool *pgxpool.Pool, slug string) (Principal, error) {
	row, err := orgsdb.New().ResolvePrincipal(ctx, pool, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrNoPrincipal
		}
		return Principal{}, err
	}
	return Principal{
		Slug: string(row.Slug),
		Kind: PrincipalKind(row.Kind),
		ID:   row.ID,
	}, nil
}
