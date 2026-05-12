// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
)

// CreateParams describes a create-org request.
type CreateParams struct {
	Slug            string
	DisplayName     string
	Description     string
	BillingEmail    string
	CreatedByUserID int64
}

// slugRE mirrors the username pattern (lowercase letters, digits,
// hyphens; cannot start or end with a hyphen). Same shape as
// users.username so the namespace unification is genuinely uniform.
var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

// Create validates the slug, opens a tx, inserts the org, and adds
// the creator as the sole owner. Slug uniqueness against the
// principals table is enforced by the DB-level PK on principals — a
// concurrent insert with the same slug fails with a unique-violation
// from the trigger, which we translate to ErrSlugTaken.
func Create(ctx context.Context, deps Deps, p CreateParams) (orgsdb.Org, error) {
	slug := strings.ToLower(strings.TrimSpace(p.Slug))
	if slug == "" {
		return orgsdb.Org{}, ErrEmptySlug
	}
	if len(slug) > 39 {
		return orgsdb.Org{}, ErrSlugTooLong
	}
	if !slugRE.MatchString(slug) {
		return orgsdb.Org{}, ErrSlugInvalid
	}
	if auth.IsReserved(slug) {
		return orgsdb.Org{}, ErrSlugReserved
	}
	if p.CreatedByUserID == 0 {
		return orgsdb.Org{}, errors.New("orgs: CreatedByUserID is required")
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return orgsdb.Org{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := orgsdb.New()
	displayName := strings.TrimSpace(p.DisplayName)
	if displayName == "" {
		displayName = slug
	}
	row, err := q.CreateOrg(ctx, tx, orgsdb.CreateOrgParams{
		Slug:            slug,
		DisplayName:     displayName,
		Description:     strings.TrimSpace(p.Description),
		BillingEmail:    strings.TrimSpace(p.BillingEmail),
		CreatedByUserID: pgtype.Int8{Int64: p.CreatedByUserID, Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Either the orgs.slug UNIQUE index OR the principals PK
			// fired. Either way, the slug is taken (by another org or
			// by an existing user/redirect).
			return orgsdb.Org{}, ErrSlugTaken
		}
		return orgsdb.Org{}, fmt.Errorf("create org: %w", err)
	}

	if err := q.AddOrgMember(ctx, tx, orgsdb.AddOrgMemberParams{
		OrgID:           row.ID,
		UserID:          p.CreatedByUserID,
		Role:            orgsdb.OrgRoleOwner,
		InvitedByUserID: pgtype.Int8{Valid: false},
	}); err != nil {
		return orgsdb.Org{}, fmt.Errorf("seed owner: %w", err)
	}
	if err := enqueueBillingSeatSync(ctx, tx, deps, row.ID); err != nil {
		return orgsdb.Org{}, fmt.Errorf("enqueue billing seat sync: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return orgsdb.Org{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return row, nil
}

// isUniqueViolation reports whether err is a Postgres unique-key
// violation (SQLSTATE 23505). The orgs.slug UNIQUE index, the
// principals PK, and any concurrent-insert race surface as 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconnError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
