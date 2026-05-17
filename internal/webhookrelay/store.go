// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	relaydb "github.com/tenseleyFlow/shithub/internal/webhookrelay/sqlc"
)

// MaxDestinations caps the fan-out factor per inbound POST. This is
// the amplification bound — N inbound × MaxDestinations outbound. PR
// 13c's create handler will reject CreateInput with more destinations
// than this; the receiver also clamps so a destinations array that
// somehow exceeds the cap (operator-written row, manual SQL) doesn't
// turn into an unbounded fan-out.
const MaxDestinations = 8

// Errors surfaced by the Store. Callers map these to HTTP status:
// ErrNotFound → 404, ErrDisabled → 410, ErrTooManyDestinations → 400,
// ErrInvalidDestination → 400.
var (
	ErrNotFound            = errors.New("webhookrelay: not found")
	ErrDisabled            = errors.New("webhookrelay: relay disabled")
	ErrTooManyDestinations = fmt.Errorf("webhookrelay: too many destinations (max %d)", MaxDestinations)
	ErrEmptyName           = errors.New("webhookrelay: name must be non-empty")
	// ErrInvalidDestination wraps an SSRF or scheme failure on a
	// destination URL at create time. PRO-EXT_SR2-10 (audit H1):
	// previously destinations rode unchecked into the DB; an operator
	// SSRF policy change after a relay was created would only catch
	// the bad destination at delivery time, leaving an always-failing
	// relay in place. Validation at create time gives users an
	// immediate "fix this" signal.
	ErrInvalidDestination = errors.New("webhookrelay: invalid destination URL")
)

// Deps wires the store. SecretBox encrypts the per-relay HMAC secret
// used to sign outbound deliveries. SSRF, when zero-valued, defaults
// to webhook.DefaultSSRFConfig() at validate time (see Create).
type Deps struct {
	Pool *pgxpool.Pool
	Box  *secretbox.Box
	SSRF webhook.SSRFConfig
}

func (d Deps) ssrfConfig() webhook.SSRFConfig {
	if len(d.SSRF.AllowedSchemes) == 0 {
		return webhook.DefaultSSRFConfig()
	}
	return d.SSRF
}

// Destination is a single fan-out target. The shape is intentionally
// minimal in 13a — URL only. 13c may extend with custom headers but
// inbound headers never flow through (see the sprint doc for the
// credential-leak rationale).
type Destination struct {
	URL string `json:"url"`
}

// Relay is the in-package view of a row. The HMAC secret + token hash
// stay on the sqlc row; callers reading via Store get this struct so
// they don't accidentally serialize secrets to logs.
type Relay struct {
	ID           int64
	UserID       int64
	Name         string
	TokenPrefix  string
	Destinations []Destination
	Disabled     bool
}

// CreateInput is the input shape for Create.
type CreateInput struct {
	UserID       int64
	Name         string
	HMACSecret   []byte // raw HMAC secret; encrypted before persist
	Destinations []Destination
}

// CreateResult bundles the row + the raw token shown to the caller
// exactly once.
type CreateResult struct {
	Relay
	RawToken string
}

// Create mints a token, encrypts the HMAC secret, inserts the row.
// The raw token returned is the only chance the caller has to read it
// — it is not persisted plaintext anywhere.
func (d Deps) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	if d.Pool == nil || d.Box == nil {
		return CreateResult{}, errors.New("webhookrelay: Create needs Pool + Box")
	}
	if in.Name == "" {
		return CreateResult{}, ErrEmptyName
	}
	if len(in.Destinations) > MaxDestinations {
		return CreateResult{}, ErrTooManyDestinations
	}
	// PRO-EXT_SR2-10 (audit H1): validate every destination URL at
	// create time so the persisted relay can't already be pointing at
	// 169.254.169.254 / localhost / non-http schemes. Deliver-time
	// validation (in deliver.go) is the defense-in-depth — this is
	// the user-feedback layer.
	ssrfCfg := d.ssrfConfig()
	for i, dest := range in.Destinations {
		if err := ssrfCfg.ValidateWithResolve(ctx, dest.URL); err != nil {
			return CreateResult{}, fmt.Errorf("%w (destination %d: %s): %s",
				ErrInvalidDestination, i+1, dest.URL, err.Error())
		}
	}
	raw, hash, prefix, err := Mint()
	if err != nil {
		return CreateResult{}, err
	}
	ct, nonce, err := d.Box.Seal(in.HMACSecret)
	if err != nil {
		return CreateResult{}, fmt.Errorf("webhookrelay: seal hmac: %w", err)
	}
	destsJSON, err := json.Marshal(in.Destinations)
	if err != nil {
		return CreateResult{}, fmt.Errorf("webhookrelay: marshal destinations: %w", err)
	}
	row, err := relaydb.New().CreateRelay(ctx, d.Pool, relaydb.CreateRelayParams{
		UserID:               in.UserID,
		Name:                 in.Name,
		TokenHash:            hash,
		TokenPrefix:          prefix,
		HmacSecretCiphertext: ct,
		HmacSecretNonce:      nonce,
		Destinations:         destsJSON,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("webhookrelay: insert: %w", err)
	}
	return CreateResult{
		Relay:    toRelay(row),
		RawToken: raw,
	}, nil
}

// LookupByToken hashes raw and returns the row, the decrypted HMAC
// secret, and the destinations. Used by the receiver path. Returns
// ErrMalformed for unparseable input, ErrNotFound for an unknown hash,
// and ErrDisabled for a row with disabled_at set.
//
// The HMAC secret is returned here (not just by the worker) because
// the receiver doesn't need it but the worker does — and the worker
// loads the row fresh by ID. Refactor candidate, but keeping it in
// one helper avoids two near-identical lookup functions.
func (d Deps) LookupByToken(ctx context.Context, raw string) (Relay, []byte, [][]byte, error) {
	hash, err := HashOf(raw)
	if err != nil {
		return Relay{}, nil, nil, err
	}
	row, err := relaydb.New().GetRelayByTokenHash(ctx, d.Pool, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Relay{}, nil, nil, ErrNotFound
		}
		return Relay{}, nil, nil, fmt.Errorf("webhookrelay: lookup: %w", err)
	}
	if row.DisabledAt.Valid {
		return toRelay(row), nil, nil, ErrDisabled
	}
	hmacSecret, err := d.Box.Open(row.HmacSecretCiphertext, row.HmacSecretNonce)
	if err != nil {
		return Relay{}, nil, nil, fmt.Errorf("webhookrelay: open hmac: %w", err)
	}
	// We don't currently surface the raw destination list separately
	// from the Relay struct — kept the slot in the signature for the
	// next-step Ingest call, which currently re-decodes from Relay.
	return toRelay(row), hmacSecret, nil, nil
}

// GetByID loads a relay by primary key + decrypted HMAC secret. The
// worker uses this on every delivery so a mid-flight disable does
// the right thing.
func (d Deps) GetByID(ctx context.Context, id int64) (Relay, []byte, error) {
	row, err := relaydb.New().GetRelayByID(ctx, d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Relay{}, nil, ErrNotFound
		}
		return Relay{}, nil, fmt.Errorf("webhookrelay: get: %w", err)
	}
	hmacSecret, err := d.Box.Open(row.HmacSecretCiphertext, row.HmacSecretNonce)
	if err != nil {
		return Relay{}, nil, fmt.Errorf("webhookrelay: open hmac: %w", err)
	}
	return toRelay(row), hmacSecret, nil
}

// ListForUser returns every relay owned by a user. Used by the
// settings page in PR 13c.
func (d Deps) ListForUser(ctx context.Context, userID int64) ([]Relay, error) {
	rows, err := relaydb.New().ListRelaysForUser(ctx, d.Pool, userID)
	if err != nil {
		return nil, fmt.Errorf("webhookrelay: list: %w", err)
	}
	out := make([]Relay, len(rows))
	for i, r := range rows {
		out[i] = toRelay(r)
	}
	return out, nil
}

// Disable flips disabled_at. The receiver returns 410 for disabled
// relays. Pending deliveries continue to drain — operator decides
// whether to delete the relay (which cascades) or let history play
// out.
func (d Deps) Disable(ctx context.Context, id int64) error {
	return relaydb.New().DisableRelay(ctx, d.Pool, id)
}

// Delete removes the relay row. Pending deliveries cascade.
func (d Deps) Delete(ctx context.Context, id int64) error {
	return relaydb.New().DeleteRelay(ctx, d.Pool, id)
}

func toRelay(row relaydb.UserWebhookRelay) Relay {
	dests := []Destination{}
	_ = json.Unmarshal(row.Destinations, &dests)
	return Relay{
		ID:           row.ID,
		UserID:       row.UserID,
		Name:         row.Name,
		TokenPrefix:  row.TokenPrefix,
		Destinations: dests,
		Disabled:     row.DisabledAt.Valid,
	}
}
