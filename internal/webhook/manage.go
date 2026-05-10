// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

// ManageDeps wires the create/update/delete orchestrator.
type ManageDeps struct {
	Pool      *pgxpool.Pool
	SecretBox *secretbox.Box
	SSRF      SSRFConfig
}

// CreateParams is the create-form input. Empty Secret means "mint
// one"; a non-empty value comes through verbatim from the operator
// (matches GitHub's behaviour where the user can paste their own).
type CreateParams struct {
	OwnerKind   string // "repo" or "org"
	OwnerID     int64
	URL         string
	ContentType string // "json" or "form"
	Events      []string
	Secret      string
	Active      bool
	SSL         bool
	ActorUserID int64
}

// CreateErrors surfaces user-friendly validation failures so handlers
// can render them in-page.
var (
	ErrBadURL         = errors.New("webhook: URL must be http or https with a host")
	ErrBadContentType = errors.New("webhook: content_type must be json or form")
	ErrBadOwnerKind   = errors.New("webhook: owner_kind must be repo or org")
	ErrBadEvent       = errors.New("webhook: event names must be 1–64 lowercase chars")
)

// Create persists a new webhook + emits a synthetic ping delivery so
// the operator sees an immediate round-trip. Returns the created row.
func Create(ctx context.Context, deps ManageDeps, params CreateParams) (webhookdb.Webhook, error) {
	if err := validateURL(params.URL); err != nil {
		return webhookdb.Webhook{}, err
	}
	// SSRF defense: reject loopback / private / disallowed-port URLs at
	// create time so the form returns synchronously instead of every
	// delivery silently failing later (SR2 H3). ValidateWithResolve
	// runs scheme/port + DNS-resolve + IP-block-list — the delivery
	// path's dialContext re-resolves as defense in depth (DNS rebinding).
	if err := deps.SSRF.ValidateWithResolve(ctx, params.URL); err != nil {
		return webhookdb.Webhook{}, fmt.Errorf("%w: %v", ErrBadURL, err)
	}
	ownerKind, err := parseOwnerKind(params.OwnerKind)
	if err != nil {
		return webhookdb.Webhook{}, err
	}
	contentType, err := parseContentType(params.ContentType)
	if err != nil {
		return webhookdb.Webhook{}, err
	}
	events, err := normalizeEvents(params.Events)
	if err != nil {
		return webhookdb.Webhook{}, err
	}
	secret := params.Secret
	if secret == "" {
		secret, err = GenerateSecret()
		if err != nil {
			return webhookdb.Webhook{}, err
		}
	}
	cipher, nonce, err := SealSecret(deps.SecretBox, secret)
	if err != nil {
		return webhookdb.Webhook{}, err
	}
	q := webhookdb.New()
	row, err := q.CreateWebhook(ctx, deps.Pool, webhookdb.CreateWebhookParams{
		OwnerKind:            ownerKind,
		OwnerID:              params.OwnerID,
		Url:                  params.URL,
		ContentType:          contentType,
		Events:               events,
		SecretCiphertext:     cipher,
		SecretNonce:          nonce,
		Active:               params.Active,
		SslVerification:      params.SSL,
		AutoDisableThreshold: 50,
		CreatedByUserID:      pgtype.Int8{Int64: params.ActorUserID, Valid: params.ActorUserID != 0},
	})
	if err != nil {
		return webhookdb.Webhook{}, err
	}
	// Synthetic ping. Best-effort — failure to enqueue is logged at
	// the caller, but we don't unwind the create.
	_ = EnqueuePing(ctx, FanoutDeps{Pool: deps.Pool}, row.ID)
	return row, nil
}

// UpdateParams mirrors the edit form.
type UpdateParams struct {
	URL         string
	ContentType string
	Events      []string
	Active      bool
	SSL         bool
	NewSecret   string // when non-empty, rotate the secret
}

// Update applies a config change to a webhook. Returns ErrBadURL etc.
// on validation failure.
func Update(ctx context.Context, deps ManageDeps, hookID int64, params UpdateParams) error {
	if err := validateURL(params.URL); err != nil {
		return err
	}
	// SSRF re-validation at update time mirrors Create (SR2 H3). An
	// admin who flips a previously-valid hook's URL to an internal
	// address gets the same synchronous rejection.
	if err := deps.SSRF.ValidateWithResolve(ctx, params.URL); err != nil {
		return fmt.Errorf("%w: %v", ErrBadURL, err)
	}
	contentType, err := parseContentType(params.ContentType)
	if err != nil {
		return err
	}
	events, err := normalizeEvents(params.Events)
	if err != nil {
		return err
	}
	q := webhookdb.New()
	if err := q.UpdateWebhook(ctx, deps.Pool, webhookdb.UpdateWebhookParams{
		ID:                   hookID,
		Url:                  params.URL,
		ContentType:          contentType,
		Events:               events,
		Active:               params.Active,
		SslVerification:      params.SSL,
		AutoDisableThreshold: 50,
	}); err != nil {
		return err
	}
	if params.NewSecret != "" {
		cipher, nonce, err := SealSecret(deps.SecretBox, params.NewSecret)
		if err != nil {
			return err
		}
		if err := q.UpdateWebhookSecret(ctx, deps.Pool, webhookdb.UpdateWebhookSecretParams{
			ID: hookID, SecretCiphertext: cipher, SecretNonce: nonce,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a webhook + cascades its deliveries (FK ON DELETE CASCADE).
func Delete(ctx context.Context, deps ManageDeps, hookID int64) error {
	return webhookdb.New().DeleteWebhook(ctx, deps.Pool, hookID)
}

// SetActive toggles the active flag, clearing any auto-disable state.
func SetActive(ctx context.Context, deps ManageDeps, hookID int64, active bool) error {
	return webhookdb.New().SetWebhookActive(ctx, deps.Pool, webhookdb.SetWebhookActiveParams{
		ID: hookID, Active: active,
	})
}

func parseOwnerKind(s string) (webhookdb.WebhookOwnerKind, error) {
	switch s {
	case "repo":
		return webhookdb.WebhookOwnerKindRepo, nil
	case "org":
		return webhookdb.WebhookOwnerKindOrg, nil
	}
	return "", ErrBadOwnerKind
}

func parseContentType(s string) (webhookdb.WebhookContentType, error) {
	switch s {
	case "", "json":
		return webhookdb.WebhookContentTypeJson, nil
	case "form":
		return webhookdb.WebhookContentTypeForm, nil
	}
	return "", ErrBadContentType
}

// normalizeEvents lowercases + dedups + length-checks. An empty input
// = all events ([]string{}). The shape is left permissive for now;
// tightening to a known-kinds enum is a post-MVP follow-up.
func normalizeEvents(events []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := []string{}
	for _, e := range events {
		e = lower(trim(e))
		if e == "" {
			continue
		}
		if len(e) > 64 {
			return nil, ErrBadEvent
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return ErrBadURL
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrBadURL
	}
	return nil
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
