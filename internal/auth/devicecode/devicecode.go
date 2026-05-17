// SPDX-License-Identifier: AGPL-3.0-or-later

// Package devicecode owns the RFC 8628 (OAuth 2.0 Device Authorization
// Grant) state machine. The package is intentionally HTTP-shape-free:
// it exposes orchestrators (Create / Approve / Deny / Exchange) that
// HTTP handlers wrap in their thin request-decode / response-encode
// layers.
//
// Design note on token disclosure: the raw PAT is minted at Exchange
// time, not at Approve. This keeps the raw secret off the
// device_authorizations row entirely — Approve only records consent
// (user_id + approved_at + scopes are already there), and the first
// successful Exchange materialises the PAT, stamps its id on
// issued_token_id, and returns the raw value to the polling client.
// Subsequent Exchange calls see issued_token_id is set and return
// invalid_grant so the disclosure is exactly one-shot.
package devicecode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// Errors callers may need to distinguish. The HTTP layer maps these to
// the RFC 8628 §3.5 error codes.
var (
	ErrUnauthorizedClient   = errors.New("devicecode: unauthorized client")
	ErrInvalidScope         = errors.New("devicecode: invalid scope")
	ErrInvalidGrant         = errors.New("devicecode: invalid grant")
	ErrAuthorizationPending = errors.New("devicecode: authorization pending")
	ErrSlowDown             = errors.New("devicecode: slow down")
	ErrAccessDenied         = errors.New("devicecode: access denied")
	ErrExpiredToken         = errors.New("devicecode: expired token")
	ErrAlreadyTerminal      = errors.New("devicecode: already terminal")
)

// Config tunes the device-code grant. Zero values get RFC-shaped
// defaults via effective() so handler wiring can pass through bare.
type Config struct {
	// ClientIDs is the allowlist enforced on Create. An empty list
	// denies every request (deny-by-default).
	ClientIDs []string
	// DefaultScopes is applied when the request omits scope=. Mirrors
	// gh's behavior of granting a minimal read-only set by default.
	DefaultScopes []string
	// ExpiresIn is the grant lifetime. Clamped to 30 minutes max.
	ExpiresIn time.Duration
	// PollInterval is the advertised minimum polling cadence; the
	// Exchange path enforces slow_down against this.
	PollInterval time.Duration
}

// Defaults returns the canonical defaults: 15-minute grants, 5-second
// poll interval, user:read scope, shithub-cli allowlist.
func Defaults() Config {
	return Config{
		ClientIDs:     []string{"shithub-cli"},
		DefaultScopes: []string{"user:read"},
		ExpiresIn:     15 * time.Minute,
		PollInterval:  5 * time.Second,
	}
}

func (c Config) effective() Config {
	out := c
	if out.ExpiresIn <= 0 || out.ExpiresIn > 30*time.Minute {
		out.ExpiresIn = 15 * time.Minute
	}
	if out.PollInterval <= 0 || out.PollInterval > time.Minute {
		out.PollInterval = 5 * time.Second
	}
	if len(out.DefaultScopes) == 0 {
		out.DefaultScopes = []string{"user:read"}
	}
	return out
}

// Deps wires the package to the database.
type Deps struct {
	Pool *pgxpool.Pool
}

// Authorization is the package-facing projection of an in-flight
// device-code grant returned from Create.
type Authorization struct {
	DeviceCode   string // raw, returned to the client exactly once
	UserCode     string // "ABCD-EFGH" shown to the user
	ExpiresIn    time.Duration
	PollInterval time.Duration
	ClientID     string
	Scopes       []string
}

// ExchangeResult carries the outcome of a successful Exchange.
type ExchangeResult struct {
	AccessToken string // raw PAT — returned to the client exactly once
	TokenType   string // always "bearer"
	Scopes      []string
}

// Create issues a fresh device authorization. The raw device_code is
// stored only as its sha256 hash; the caller must propagate the
// returned raw value to the client without persisting it server-side.
func Create(ctx context.Context, deps Deps, cfg Config, clientID, scopeInput string) (Authorization, error) {
	c := cfg.effective()
	if !allowedClient(c.ClientIDs, clientID) {
		return Authorization{}, ErrUnauthorizedClient
	}
	scopes, err := parseScopes(scopeInput, c.DefaultScopes)
	if err != nil {
		return Authorization{}, err
	}

	deviceCodeRaw, deviceCodeHash, err := newDeviceCode()
	if err != nil {
		return Authorization{}, err
	}
	userCode, err := newUserCode()
	if err != nil {
		return Authorization{}, err
	}

	if _, err := usersdb.New().InsertDeviceAuthorization(ctx, deps.Pool, usersdb.InsertDeviceAuthorizationParams{
		DeviceCodeHash:  deviceCodeHash,
		UserCode:        userCode,
		ClientID:        clientID,
		Scopes:          scopes,
		IntervalSeconds: int32(c.PollInterval / time.Second),
		ExpiresAt:       pgtype.Timestamptz{Time: time.Now().Add(c.ExpiresIn), Valid: true},
	}); err != nil {
		return Authorization{}, fmt.Errorf("devicecode: insert: %w", err)
	}

	return Authorization{
		DeviceCode:   deviceCodeRaw,
		UserCode:     userCode,
		ExpiresIn:    c.ExpiresIn,
		PollInterval: c.PollInterval,
		ClientID:     clientID,
		Scopes:       scopes,
	}, nil
}

// LookupByUserCode resolves the user-facing code to the underlying row
// for the verification page. Returns ErrInvalidGrant on miss so the
// HTML handler can render a uniform "code not recognised" message.
func LookupByUserCode(ctx context.Context, deps Deps, userCode string) (usersdb.DeviceAuthorization, error) {
	row, err := usersdb.New().GetDeviceAuthorizationByUserCode(ctx, deps.Pool, normaliseUserCode(userCode))
	if err != nil {
		return usersdb.DeviceAuthorization{}, ErrInvalidGrant
	}
	return row, nil
}

// Approve records the user's consent. The PAT is NOT minted here — see
// the package doc comment for the reasoning. Approve returns the row's
// id so the caller can hand it back to the HTML success page without
// re-resolving by user_code.
func Approve(ctx context.Context, deps Deps, rowID, userID int64) error {
	full, err := loadByID(ctx, deps.Pool, rowID)
	if err != nil {
		return ErrInvalidGrant
	}
	if full.ApprovedAt.Valid || full.DeniedAt.Valid {
		return ErrAlreadyTerminal
	}
	if time.Now().After(full.ExpiresAt.Time) {
		return ErrExpiredToken
	}
	return usersdb.New().ApproveDeviceAuthorization(ctx, deps.Pool, usersdb.ApproveDeviceAuthorizationParams{
		ID:            full.ID,
		UserID:        pgtype.Int8{Int64: userID, Valid: true},
		IssuedTokenID: pgtype.Int8{}, // populated by Exchange
	})
}

// Deny terminates an in-flight authorization without minting a token.
// Future Exchange polls return ErrAccessDenied.
func Deny(ctx context.Context, deps Deps, rowID int64) error {
	full, err := loadByID(ctx, deps.Pool, rowID)
	if err != nil {
		return ErrInvalidGrant
	}
	if full.ApprovedAt.Valid || full.DeniedAt.Valid {
		return ErrAlreadyTerminal
	}
	return usersdb.New().DenyDeviceAuthorization(ctx, deps.Pool, full.ID)
}

// Exchange is the CLI-facing poll. Returns the minted PAT exactly
// once: on the first successful exchange after approval. Subsequent
// polls (after issued_token_id is set) return ErrInvalidGrant so the
// raw token is disclosed at most once.
//
// The slow_down enforcement uses last_polled_at + interval_seconds.
// The check fires BEFORE the approval check so a fast-polling client
// gets a clear back-off signal instead of an accidental "pending".
func Exchange(ctx context.Context, deps Deps, clientID, rawDeviceCode, tokenName string) (ExchangeResult, error) {
	hash, hashErr := hashDeviceCode(rawDeviceCode)
	if hashErr != nil {
		return ExchangeResult{}, ErrInvalidGrant
	}
	q := usersdb.New()

	// Pre-checks run without a lock: they're read-only state queries
	// and the races they tolerate (e.g. two simultaneous fast-polls
	// both racing slow_down) are explicitly allowed by RFC 8628. The
	// only race we MUST close is the post-approval mint race, handled
	// by the FOR UPDATE block below.
	row, dbErr := q.GetDeviceAuthorizationByCodeHash(ctx, deps.Pool, hash)
	if dbErr != nil {
		return ExchangeResult{}, ErrInvalidGrant
	}
	if row.ClientID != clientID {
		return ExchangeResult{}, ErrUnauthorizedClient
	}
	if time.Now().After(row.ExpiresAt.Time) {
		return ExchangeResult{}, ErrExpiredToken
	}
	if row.DeniedAt.Valid {
		return ExchangeResult{}, ErrAccessDenied
	}

	if row.LastPolledAt.Valid {
		minNext := row.LastPolledAt.Time.Add(time.Duration(row.IntervalSeconds) * time.Second)
		if time.Now().Before(minNext) {
			_ = q.TouchDeviceAuthorizationPoll(ctx, deps.Pool, row.ID)
			return ExchangeResult{}, ErrSlowDown
		}
	}
	_ = q.TouchDeviceAuthorizationPoll(ctx, deps.Pool, row.ID)

	if !row.ApprovedAt.Valid {
		return ExchangeResult{}, ErrAuthorizationPending
	}
	if row.IssuedTokenID.Valid {
		// Cheap fast-path: a previous Exchange already minted on this
		// grant, no need to take a write lock to confirm.
		return ExchangeResult{}, ErrInvalidGrant
	}
	if !row.UserID.Valid {
		return ExchangeResult{}, ErrInvalidGrant
	}

	// Hot path: this looks like the first Exchange after approval.
	// Wrap mint+insert+stamp in a TX with SELECT FOR UPDATE so two
	// concurrent first-polls serialize at the DB — exactly one mints
	// the PAT, the other re-reads issued_token_id under the lock and
	// returns invalid_grant. Also closes the orphan-token bug where a
	// process death between InsertUserToken and the stamp would leave
	// a stranded PAT row.
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := q.GetDeviceAuthorizationByCodeHashForUpdate(ctx, tx, hash)
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: lock grant: %w", err)
	}
	if locked.IssuedTokenID.Valid {
		// A concurrent Exchange landed the mint before us; one-shot
		// disclosure preserved.
		return ExchangeResult{}, ErrInvalidGrant
	}
	if !locked.UserID.Valid {
		return ExchangeResult{}, ErrInvalidGrant
	}

	raw, hashBytes, prefix, err := pat.Mint()
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: mint pat: %w", err)
	}
	tok, err := q.InsertUserToken(ctx, tx, usersdb.InsertUserTokenParams{
		UserID:      locked.UserID.Int64,
		Name:        tokenName,
		TokenHash:   hashBytes,
		TokenPrefix: prefix,
		Scopes:      locked.Scopes,
		ExpiresAt:   pgtype.Timestamptz{},
	})
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: insert token: %w", err)
	}
	if err := q.StampIssuedTokenID(ctx, tx, usersdb.StampIssuedTokenIDParams{
		ID:            locked.ID,
		IssuedTokenID: pgtype.Int8{Int64: tok.ID, Valid: true},
	}); err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: stamp token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ExchangeResult{}, fmt.Errorf("devicecode: commit tx: %w", err)
	}

	return ExchangeResult{
		AccessToken: raw,
		TokenType:   "bearer",
		Scopes:      locked.Scopes,
	}, nil
}

func allowedClient(allow []string, clientID string) bool {
	for _, v := range allow {
		if v == clientID {
			return true
		}
	}
	return false
}

// parseScopes accepts space- or comma-separated scopes and returns the
// normalised, deduped, validated slice. Unknown scopes → ErrInvalidScope.
func parseScopes(input string, defaults []string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		out := make([]string, len(defaults))
		copy(out, defaults)
		return out, nil
	}
	raw := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ' ' })
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !pat.ValidScope(s) {
			return nil, ErrInvalidScope
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		out = append(out, defaults...)
	}
	return out, nil
}

func newDeviceCode() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("devicecode: rand: %w", err)
	}
	raw = hexEncode(buf)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

func hashDeviceCode(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidGrant
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

// newUserCode produces an 8-character "ABCD-EFGH" identifier from a
// 32-symbol alphabet that excludes 0/O/1/I to avoid typing ambiguity.
func newUserCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, b := range buf {
		out = append(out, alphabet[int(b)%len(alphabet)])
		if i == 3 {
			out = append(out, '-')
		}
	}
	return string(out), nil
}

func normaliseUserCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	if len(s) == 8 && !strings.Contains(s, "-") {
		s = s[:4] + "-" + s[4:]
	}
	return s
}

func hexEncode(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexd[v>>4]
		out[i*2+1] = hexd[v&0x0f]
	}
	return string(out)
}

// loadByID resolves a row by its bigserial id. sqlc didn't generate a
// dedicated GetByID because every other consumer comes in via
// device_code_hash or user_code; the Approve / Deny path needs id
// because it has it from the prior LookupByUserCode call.
func loadByID(ctx context.Context, pool *pgxpool.Pool, id int64) (usersdb.DeviceAuthorization, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, device_code_hash, user_code, client_id, scopes, user_id,
		       approved_at, denied_at, issued_token_id, interval_seconds,
		       expires_at, last_polled_at, created_at
		FROM device_authorizations
		WHERE id = $1
	`, id)
	if err != nil {
		return usersdb.DeviceAuthorization{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return usersdb.DeviceAuthorization{}, ErrInvalidGrant
	}
	var a usersdb.DeviceAuthorization
	if err := rows.Scan(
		&a.ID, &a.DeviceCodeHash, &a.UserCode, &a.ClientID, &a.Scopes, &a.UserID,
		&a.ApprovedAt, &a.DeniedAt, &a.IssuedTokenID, &a.IntervalSeconds,
		&a.ExpiresAt, &a.LastPolledAt, &a.CreatedAt,
	); err != nil {
		return usersdb.DeviceAuthorization{}, err
	}
	return a, nil
}
