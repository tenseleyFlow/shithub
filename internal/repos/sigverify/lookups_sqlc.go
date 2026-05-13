// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// DBTX is the pgx interface the sqlc-backed Lookups need. Matches the
// sqlc-generated DBTX exactly so callers can pass either a *pgxpool.Pool
// or a pgx.Tx without conversion.
type DBTX interface {
	usersdb.DBTX
}

// SQLCLookups is the production Lookups implementation, backed by the
// usersdb-generated queries. Used by the orchestrator when invoked
// from the backfill worker, the verification render path, and the
// commits REST handler.
type SQLCLookups struct {
	DB DBTX
	Q  *usersdb.Queries
}

// NewSQLCLookups constructs a Lookups bound to the given DB handle.
// The handle can be a connection pool (for concurrent reads) or a tx
// (for invalidation-followed-by-re-verify flows that need to see
// uncommitted writes within the same transaction).
func NewSQLCLookups(db DBTX) *SQLCLookups {
	return &SQLCLookups{DB: db, Q: usersdb.New()}
}

// SubkeyByFingerprint resolves a 40-hex fingerprint to a Subkey row.
// Returns (_, false, nil) on miss; only DB errors propagate.
func (l *SQLCLookups) SubkeyByFingerprint(ctx context.Context, fingerprint string) (Subkey, bool, error) {
	row, err := l.Q.GetUserGPGSubkeyByFingerprint(ctx, l.DB, fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subkey{}, false, nil
	}
	if err != nil {
		return Subkey{}, false, err
	}
	sk := Subkey{
		ID:                row.ID,
		GPGKeyID:          row.GpgKeyID,
		Fingerprint:       row.Fingerprint,
		KeyID:             row.KeyID,
		CanSign:           row.CanSign,
		CanEncryptComms:   row.CanEncryptComms,
		CanEncryptStorage: row.CanEncryptStorage,
		CanCertify:        row.CanCertify,
	}
	if row.ExpiresAt.Valid {
		sk.ExpiresAt = row.ExpiresAt.Time
	}
	if row.RevokedAt.Valid {
		sk.RevokedAt = row.RevokedAt.Time
	}
	return sk, true, nil
}

// GPGKeyByID resolves a primary user_gpg_keys row by id (NOT scoped
// to a user_id — the verification path discovers user_id from the
// row itself). Returns (_, false, nil) on miss; only DB errors
// propagate.
func (l *SQLCLookups) GPGKeyByID(ctx context.Context, id int64) (GPGKey, bool, error) {
	row, err := l.Q.GetUserGPGKeyForVerification(ctx, l.DB, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return GPGKey{}, false, nil
	}
	if err != nil {
		return GPGKey{}, false, err
	}
	return GPGKey{
		ID:          row.ID,
		UserID:      row.UserID,
		Fingerprint: row.Fingerprint,
		KeyID:       row.KeyID,
		Armored:     row.Armored,
	}, true, nil
}

// UserEmailsByUserID returns every email associated with the user
// (verified or not) so the orchestrator can run the bad_email vs
// unverified_email vs valid discrimination.
func (l *SQLCLookups) UserEmailsByUserID(ctx context.Context, userID int64) ([]UserEmail, error) {
	rows, err := l.Q.ListUserEmailsForUser(ctx, l.DB, userID)
	if err != nil {
		return nil, err
	}
	emails := make([]UserEmail, 0, len(rows))
	for _, row := range rows {
		emails = append(emails, UserEmail{
			Email:    row.Email,
			Verified: row.Verified,
		})
	}
	return emails, nil
}
