// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan_test

// PRO-EXT01-10d: covers the alert dispatcher end-to-end. The history
// worker's "is this a new finding" gate is unit-tested via the upsert
// returning equal timestamps; the integration here is between
// DispatchAlert + the prefs row + the entitlement.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/secretscan"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type captureSender struct {
	mu       sync.Mutex
	messages []email.Message
}

func (s *captureSender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *captureSender) Sent() []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]email.Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// TestDispatch_ProUserEmailFires is the happy path: Pro user, email
// channel enabled → one Send call lands with the expected subject and
// the verified primary address.
func TestDispatch_ProUserEmailFires(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	user := mkUser(t, pool, "alice-ssa")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "alice-ssa@example.test")
	repo, finding := seedRepoAndFinding(t, pool, user.ID)
	upsertPrefs(t, pool, user.ID, true, "", nil)

	sender := &captureSender{}
	dispatch := secretscan.DispatchAlert(secretscan.AlertDeps{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	})
	payload := mustMarshal(t, secretscan.AlertPayload{UserID: user.ID, RepoID: repo.ID, FindingID: finding.ID})
	if err := dispatch(context.Background(), payload); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(sent))
	}
	if sent[0].To != "alice-ssa@example.test" {
		t.Errorf("To = %q", sent[0].To)
	}
	if !strings.Contains(sent[0].Subject, finding.Pattern[0:3]) && !strings.Contains(sent[0].Subject, "Secret detected") {
		t.Errorf("subject missing Secret-detected marker: %q", sent[0].Subject)
	}
	if !strings.Contains(sent[0].Text, finding.Pattern) {
		t.Errorf("body must include pattern name (only the redacted excerpt is in the row): %s", sent[0].Text)
	}
}

// TestDispatch_NoPrefsRowSilent confirms absence of a prefs row is the
// off state. A user who never visited /settings/secret-scanning/alerts
// must not receive an email even though FeatureSecretScanAlerts says
// they could.
func TestDispatch_NoPrefsRowSilent(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	user := mkUser(t, pool, "no-prefs")
	upgradeToPro(t, pool, user.ID)
	verifyEmail(t, pool, user.ID, "no-prefs@example.test")
	repo, finding := seedRepoAndFinding(t, pool, user.ID)

	sender := &captureSender{}
	dispatch := secretscan.DispatchAlert(secretscan.AlertDeps{
		Pool:        pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailSender: sender,
		EmailFrom:   "noreply@shithub.test",
		SiteName:    "shithub",
		BaseURL:     "https://shithub.test",
	})
	if err := dispatch(context.Background(),
		mustMarshal(t, secretscan.AlertPayload{UserID: user.ID, RepoID: repo.ID, FindingID: finding.ID})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := len(sender.Sent()); got != 0 {
		t.Errorf("emails sent = %d, want 0 (no prefs row)", got)
	}
}

// TestDispatch_FreeUnderEnforceDrops: Free user, enforce=true → no
// email even with prefs set + entitlement-check would deny. The
// surface tag on the deny log differentiates from the 10c
// configuration-write gate.
func TestDispatch_FreeUnderEnforceDrops(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	logBuf := &bytes.Buffer{}
	user := mkUser(t, pool, "free-enforce")
	verifyEmail(t, pool, user.ID, "free-enforce@example.test")
	repo, finding := seedRepoAndFinding(t, pool, user.ID)
	upsertPrefs(t, pool, user.ID, true, "", nil)

	sender := &captureSender{}
	dispatch := secretscan.DispatchAlert(secretscan.AlertDeps{
		Pool:          pool,
		Logger:        slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		EmailSender:   sender,
		EmailFrom:     "noreply@shithub.test",
		SiteName:      "shithub",
		BaseURL:       "https://shithub.test",
		EnforceAlerts: true,
	})
	if err := dispatch(context.Background(),
		mustMarshal(t, secretscan.AlertPayload{UserID: user.ID, RepoID: repo.ID, FindingID: finding.ID})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := len(sender.Sent()); got != 0 {
		t.Errorf("emails sent = %d, want 0 (Free + enforce)", got)
	}
	out := logBuf.String()
	if !strings.Contains(out, `"surface":"secretscan-alert"`) {
		t.Errorf("expected secretscan-alert surface tag in log: %s", out)
	}
	if !strings.Contains(out, `"mode":"enforce"`) {
		t.Errorf("expected mode=enforce in log: %s", out)
	}
}

// TestDispatch_WebhookSignsAndPosts asserts the webhook channel hits
// the configured URL, includes a verifiable HMAC-SHA256 signature, and
// sends a JSON body shaped to event=secret_scan.finding.new.
func TestDispatch_WebhookSignsAndPosts(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	user := mkUser(t, pool, "webhook-user")
	upgradeToPro(t, pool, user.ID)
	repo, finding := seedRepoAndFinding(t, pool, user.ID)

	secret := mustRandom(t, 32)
	var (
		gotSignature string
		gotBody      []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-ShitHub-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	upsertPrefs(t, pool, user.ID, false, srv.URL, secret)

	dispatch := secretscan.DispatchAlert(secretscan.AlertDeps{
		Pool:       pool,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: srv.Client(),
		EmailFrom:  "noreply@shithub.test",
		SiteName:   "shithub",
		BaseURL:    "https://shithub.test",
	})
	if err := dispatch(context.Background(),
		mustMarshal(t, secretscan.AlertPayload{UserID: user.ID, RepoID: repo.ID, FindingID: finding.ID})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(gotBody) == 0 {
		t.Fatal("webhook handler received no body")
	}
	// Verify signature is computed over the *exact* body we received.
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSignature != want {
		t.Errorf("signature = %q, want %q", gotSignature, want)
	}
	// Body shape — minimal smoke test that the key fields land.
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["event"] != "secret_scan.finding.new" {
		t.Errorf("event = %v", body["event"])
	}
	if body["repo"] == nil || body["finding"] == nil {
		t.Errorf("missing repo or finding object: %s", gotBody)
	}
}

// --- helpers -----------------------------------------------------------

func seedRepoAndFinding(t *testing.T, pool *pgxpool.Pool, ownerUserID int64) (reposdb.Repo, secretscandb.SecretScanFinding) {
	t.Helper()
	repo, err := reposdb.New().CreateRepo(context.Background(), pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: ownerUserID, Valid: true},
		Name:          "demo-secret",
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	f, err := secretscandb.New().UpsertSecretScanFinding(context.Background(), pool, secretscandb.UpsertSecretScanFindingParams{
		RepoID:       repo.ID,
		Pattern:      "aws_access_key_id",
		Path:         "config/prod.yml",
		LineNo:       42,
		Excerpt:      "AKIA[REDACTED]",
		FirstSeenOid: "deadbeef",
	})
	if err != nil {
		t.Fatalf("UpsertSecretScanFinding: %v", err)
	}
	return repo, f
}

func upsertPrefs(t *testing.T, pool *pgxpool.Pool, userID int64, emailEnabled bool, webhookURL string, webhookSecret []byte) {
	t.Helper()
	params := secretscandb.UpsertSecretScanAlertPrefsParams{
		UserID:        userID,
		EmailEnabled:  emailEnabled,
		WebhookUrl:    pgtype.Text{},
		WebhookSecret: nil,
	}
	if webhookURL != "" {
		params.WebhookUrl = pgtype.Text{String: webhookURL, Valid: true}
		params.WebhookSecret = webhookSecret
	}
	if _, err := secretscandb.New().UpsertSecretScanAlertPrefs(context.Background(), pool, params); err != nil {
		t.Fatalf("UpsertSecretScanAlertPrefs: %v", err)
	}
}

func mkUser(t *testing.T, pool *pgxpool.Pool, username string) usersdb.User {
	t.Helper()
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func upgradeToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_ssa_" + time.Now().Format("150405.000000"), Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_ssa_" + time.Now().Format("150405.000000"),
	})
	if err != nil {
		t.Fatalf("upgrade to Pro: %v", err)
	}
}

func verifyEmail(t *testing.T, pool *pgxpool.Pool, userID int64, addr string) {
	t.Helper()
	q := usersdb.New()
	em, err := q.CreateUserEmail(context.Background(), pool, usersdb.CreateUserEmailParams{
		UserID: userID, Email: addr, IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := q.LinkUserPrimaryEmail(context.Background(), pool, usersdb.LinkUserPrimaryEmailParams{
		ID: userID, PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustRandom(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return buf
}
