// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan

// PRO-EXT01-10d: per-finding alert dispatcher. The scan worker
// (internal/worker/jobs/secret_scan_history.go) enqueues one alert job
// per *new* finding (timestamps equal at upsert time → it was the
// INSERT branch, not the ON CONFLICT branch). This package executes
// the send.
//
// Channels: email (via the existing email.Sender) and webhook
// (HMAC-SHA256-signed POST). Both opt-in via secret_scan_alert_prefs.
// A user with no prefs row is silent; with email_enabled true and a
// webhook_url set, both fire.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindSecretScanAlert is the worker job kind enqueued one-per-finding
// from the history scan worker. Payload shape is AlertPayload.
const KindSecretScanAlert worker.Kind = "secretscan:alert"

// AlertPayload is the JSON body of a queued alert job.
type AlertPayload struct {
	UserID    int64 `json:"user_id"`
	RepoID    int64 `json:"repo_id"`
	FindingID int64 `json:"finding_id"`
}

// AlertDeps wires the dispatcher. EmailSender + HTTPClient may be nil
// in tests; absence of a sender just no-ops that channel.
type AlertDeps struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	EmailSender email.Sender
	HTTPClient  HTTPDoer
	EmailFrom   string
	SiteName    string
	BaseURL     string
	// EnforceAlerts, when true, drops the send for any owner whose
	// FeatureSecretScanAlerts check denies. Off (the default) keeps
	// sending and logs the would-deny — soak path before PRO-EXT01-17.
	EnforceAlerts bool
	// SSRF is the outbound-URL validator. Zero value short-circuits
	// to webhook.DefaultSSRFConfig() — production deployments don't
	// need to set this; tests opt into a loopback-permitting config
	// via AllowPrivateNetworks. PRO-EXT_SR2-10 (audit C1): without
	// this gate the webhook channel POSTs HMAC-signed payloads to
	// any URL a user can configure, including 169.254.169.254 /
	// localhost / etc.
	SSRF webhook.SSRFConfig
}

func (d AlertDeps) ssrfConfig() webhook.SSRFConfig {
	if len(d.SSRF.AllowedSchemes) == 0 {
		return webhook.DefaultSSRFConfig()
	}
	return d.SSRF
}

// HTTPDoer matches *http.Client.Do so tests can drop in an in-memory
// transport without touching the network.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// webhookTimeout caps a single outbound POST. Pinned to 10s because
// the worker queue can't afford a slow user-controlled endpoint to
// monopolize a slot — the secret-scan alert is informational, not
// transactional. Hard-coded rather than configured because all the
// downstream tradeoffs assume a fast fail.
const webhookTimeout = 10 * time.Second

// DispatchAlert is the single-job entrypoint registered as
// KindSecretScanAlert. Loads the finding + owner + prefs, checks the
// entitlement, and fires the configured channels.
func DispatchAlert(deps AlertDeps) func(ctx context.Context, payload []byte) error {
	return func(ctx context.Context, payload []byte) error {
		if deps.Pool == nil {
			return errors.New("secretscan: DispatchAlert needs Pool")
		}
		var p AlertPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("decode alert payload: %w", err)
		}
		sq := secretscandb.New()
		finding, err := sq.GetSecretScanFinding(ctx, deps.Pool, secretscandb.GetSecretScanFindingParams{
			ID:     p.FindingID,
			RepoID: p.RepoID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Finding was deleted or repo was deleted between
				// enqueue and dispatch. Silent skip — the lifecycle
				// races are expected and not an operator concern.
				return nil
			}
			return fmt.Errorf("load finding: %w", err)
		}
		repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("load repo: %w", err)
		}
		if !alertAllowed(ctx, deps, p.UserID) {
			return nil
		}
		prefs, err := sq.GetSecretScanAlertPrefs(ctx, deps.Pool, p.UserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // no prefs row → user opted out
			}
			return fmt.Errorf("load prefs: %w", err)
		}

		owner, err := usersdb.New().GetUserByID(ctx, deps.Pool, p.UserID)
		if err != nil {
			return fmt.Errorf("load owner: %w", err)
		}

		sent := false
		if prefs.EmailEnabled {
			if err := sendEmailAlert(ctx, deps, owner, repo, finding); err != nil {
				logWarn(deps.Logger, ctx, "secretscan alert email failed",
					"user_id", p.UserID, "finding_id", p.FindingID, "error", err)
			} else {
				sent = true
			}
		}
		if prefs.WebhookUrl.Valid && len(prefs.WebhookSecret) > 0 {
			if err := sendWebhookAlert(ctx, deps, prefs, repo, finding); err != nil {
				logWarn(deps.Logger, ctx, "secretscan alert webhook failed",
					"user_id", p.UserID, "finding_id", p.FindingID, "error", err)
			} else {
				sent = true
			}
		}
		if sent {
			_ = sq.TouchSecretScanAlertPrefsAlertedAt(ctx, deps.Pool, p.UserID)
		}
		return nil
	}
}

// alertAllowed mirrors digestAllowed in internal/notifications: error
// → fail-soft on report-only, fail-closed on enforce. The
// `secretscan-alert` surface tag distinguishes this signal from
// `repo-settings` (where 10c gates the *configuration* writes).
func alertAllowed(ctx context.Context, deps AlertDeps, userID int64) bool {
	principal := billing.PrincipalForUser(userID)
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: deps.Pool}, principal, entitlements.FeatureSecretScanAlerts)
	if err != nil {
		return !deps.EnforceAlerts
	}
	if decision.Allowed {
		return true
	}
	if deps.Logger != nil {
		mode := "report_only"
		if deps.EnforceAlerts {
			mode = "enforce"
		}
		deps.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", principal.String(),
			"principal_kind", string(principal.Kind),
			"principal_id", userID,
			"feature", string(entitlements.FeatureSecretScanAlerts),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "secretscan-alert")
	}
	return !deps.EnforceAlerts
}

func sendEmailAlert(
	ctx context.Context, deps AlertDeps,
	owner usersdb.User, repo reposdb.Repo, finding secretscandb.SecretScanFinding,
) error {
	if deps.EmailSender == nil {
		return nil
	}
	to, err := primaryEmailFor(ctx, deps.Pool, owner.ID)
	if err != nil {
		return err
	}
	if to == "" {
		return nil
	}
	subject := fmt.Sprintf("[%s] Secret detected in %s", deps.SiteName, repo.Name)
	body := composeEmailBody(deps, owner, repo, finding)
	return deps.EmailSender.Send(ctx, email.Message{
		From:    deps.EmailFrom,
		To:      to,
		Subject: subject,
		Text:    body,
	})
}

func sendWebhookAlert(
	ctx context.Context, deps AlertDeps,
	prefs secretscandb.SecretScanAlertPref, repo reposdb.Repo, finding secretscandb.SecretScanFinding,
) error {
	if !prefs.WebhookUrl.Valid {
		return nil
	}
	ssrfCfg := deps.ssrfConfig()
	// PRO-EXT_SR2-10 (audit C1): re-check at send time even though
	// the settings save path validates too. The DB persisted what
	// passed validation *then*; an operator policy change since (or
	// a DNS rebind on a hostname) could make a previously-valid URL
	// dangerous now. Cheap and fail-closed.
	if err := ssrfCfg.ValidateWithResolve(ctx, prefs.WebhookUrl.String); err != nil {
		return fmt.Errorf("ssrf validate: %w", err)
	}

	httpClient := deps.HTTPClient
	if httpClient == nil {
		// SSRF-aware client: enforces the rules at dial time too,
		// defending against DNS rebinding between the resolve above
		// and the actual TCP connect.
		httpClient = ssrfCfg.HTTPClient()
	}
	payload, err := json.Marshal(webhookBody(repo, finding, deps.BaseURL))
	if err != nil {
		return fmt.Errorf("marshal webhook: %w", err)
	}
	mac := hmac.New(sha256.New, prefs.WebhookSecret)
	_, _ = mac.Write(payload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	reqCtx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, prefs.WebhookUrl.String, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", deps.SiteName+"-secret-scan-alerts")
	// PRO-EXT_SR2-13 (audit Q6): normalize header naming across the
	// three webhook surfaces. Repo webhooks emit X-Shithub-Signature-256
	// + X-Shithub-Event (internal/webhook/deliver.go). Webhook relay
	// emits X-Shithub-Relay-Signature-256. Secret-scan alerts had been
	// using mixed-case "X-ShitHub-Signature" with no algorithm suffix,
	// which forced downstream HMAC-verify snippets to special-case it.
	req.Header.Set("X-Shithub-Signature-256", signature)
	req.Header.Set("X-Shithub-Event", "secret_scan.finding.new")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func composeEmailBody(deps AlertDeps, owner usersdb.User, repo reposdb.Repo, finding secretscandb.SecretScanFinding) string {
	var b strings.Builder
	name := owner.DisplayName
	if name == "" {
		name = owner.Username
	}
	fmt.Fprintf(&b, "Hi %s,\n\n", name)
	fmt.Fprintf(&b, "A new potential secret was detected in %s.\n\n", repo.Name)
	fmt.Fprintf(&b, "Pattern: %s\n", finding.Pattern)
	fmt.Fprintf(&b, "Path: %s\n", finding.Path)
	fmt.Fprintf(&b, "Line: %d\n", finding.LineNo)
	fmt.Fprintf(&b, "Excerpt: %s\n", finding.Excerpt)
	fmt.Fprintf(&b, "\nReview the finding: %s/%s/%s/settings/secret-scanning\n",
		strings.TrimRight(deps.BaseURL, "/"), owner.Username, repo.Name)
	fmt.Fprintf(&b, "Manage alerts: %s/settings/secret-scanning/alerts\n",
		strings.TrimRight(deps.BaseURL, "/"))
	return b.String()
}

func webhookBody(repo reposdb.Repo, finding secretscandb.SecretScanFinding, baseURL string) map[string]any {
	return map[string]any{
		"event": "secret_scan.finding.new",
		"site":  strings.TrimRight(baseURL, "/"),
		"repo":  map[string]any{"id": repo.ID, "name": repo.Name},
		"finding": map[string]any{
			"id":         finding.ID,
			"pattern":    finding.Pattern,
			"path":       finding.Path,
			"line_no":    finding.LineNo,
			"excerpt":    finding.Excerpt,
			"first_seen": finding.FirstSeenAt.Time.UTC().Format(time.RFC3339),
			"first_oid":  finding.FirstSeenOid,
		},
	}
}

// primaryEmailFor mirrors the helper in internal/notifications but
// avoids the circular import by inlining the lookup.
func primaryEmailFor(ctx context.Context, pool *pgxpool.Pool, userID int64) (string, error) {
	q := usersdb.New()
	u, err := q.GetUserByID(ctx, pool, userID)
	if err != nil {
		return "", err
	}
	if !u.PrimaryEmailID.Valid {
		return "", nil
	}
	em, err := q.GetUserEmailByID(ctx, pool, u.PrimaryEmailID.Int64)
	if err != nil {
		return "", err
	}
	if !em.Verified {
		return "", nil
	}
	return em.Email, nil
}

func logWarn(l *slog.Logger, ctx context.Context, msg string, kv ...any) {
	if l == nil {
		return
	}
	l.WarnContext(ctx, msg, kv...)
}
