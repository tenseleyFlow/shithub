// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	dto "github.com/prometheus/client_model/go"

	"github.com/tenseleyFlow/shithub/internal/actions/finalize"
	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsecrets "github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

const runnerAPIFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestRunnerHeartbeatClaimsQueuedJob(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)

	token, runnerID := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	signer := runnerAPISigner(t, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	router := newRunnerAPIRouter(t, pool, logger, signer)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1,"host_name":"runner-host","version":"dev-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		Job   struct {
			ID            int64          `json:"id"`
			RunID         int64          `json:"run_id"`
			RepoID        int64          `json:"repo_id"`
			CheckoutURL   string         `json:"checkout_url"`
			CheckoutToken string         `json:"checkout_token"`
			EventPayload  map[string]any `json:"event_payload"`
			Steps         []struct {
				Run  string `json:"run"`
				Uses string `json:"uses"`
			} `json:"steps"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("response token is empty")
	}
	if resp.Job.RunID != runID || resp.Job.RepoID != repoID || len(resp.Job.Steps) != 2 {
		t.Fatalf("unexpected job payload: %+v", resp.Job)
	}
	if resp.Job.CheckoutURL != "https://shithub.test/alice/demo.git" || resp.Job.CheckoutToken == "" {
		t.Fatalf("checkout payload: url=%q token_empty=%t", resp.Job.CheckoutURL, resp.Job.CheckoutToken == "")
	}
	if resp.Job.EventPayload["ref"] != "refs/heads/trunk" {
		t.Fatalf("event payload not returned to runner: %#v", resp.Job.EventPayload)
	}
	claims, err := signer.Verify(resp.Token)
	if err != nil {
		t.Fatalf("verify runner JWT: %v", err)
	}
	if claims.JobID != resp.Job.ID || claims.RunID != runID || claims.RepoID != repoID {
		t.Fatalf("claims/job mismatch: claims=%+v job=%+v", claims, resp.Job)
	}
	claimRunnerID, err := claims.RunnerID()
	if err != nil {
		t.Fatalf("claims RunnerID: %v", err)
	}
	if claimRunnerID != runnerID {
		t.Fatalf("claims runner_id: got %d, want %d", claimRunnerID, runnerID)
	}
	if claims.Purpose != runnerjwt.PurposeAPI {
		t.Fatalf("api token purpose: got %q", claims.Purpose)
	}
	checkoutClaims, err := signer.Verify(resp.Job.CheckoutToken)
	if err != nil {
		t.Fatalf("verify checkout token: %v", err)
	}
	if checkoutClaims.JobID != resp.Job.ID || checkoutClaims.RunID != runID || checkoutClaims.RepoID != repoID ||
		checkoutClaims.Purpose != runnerjwt.PurposeCheckout {
		t.Fatalf("checkout claims/job mismatch: claims=%+v job=%+v", checkoutClaims, resp.Job)
	}
	runnerRow, err := actionsdb.New().GetRunnerByID(ctx, pool, runnerID)
	if err != nil {
		t.Fatalf("GetRunnerByID: %v", err)
	}
	if runnerRow.HostName != "runner-host" || runnerRow.Version != "dev-test" {
		t.Fatalf("runner metadata: host=%q version=%q", runnerRow.HostName, runnerRow.Version)
	}

	var logResp struct {
		Accepted  bool   `json:"accepted"`
		NextToken string `json:"next_token"`
	}
	logBody := fmt.Sprintf(`{"seq":0,"chunk":%q}`, base64.StdEncoding.EncodeToString([]byte("hello\n")))
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/logs", resp.Job.ID), strings.NewReader(logBody))
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("logs status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &logResp); err != nil {
		t.Fatalf("decode log response: %v", err)
	}
	if !logResp.Accepted || logResp.NextToken == "" {
		t.Fatalf("unexpected log response: %+v", logResp)
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/logs", resp.Job.ID), strings.NewReader(logBody))
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("replay status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/status", resp.Job.ID),
		strings.NewReader(`{"status":"completed","conclusion":"success"}`))
	req.Header.Set("Authorization", "Bearer "+logResp.NextToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("complete status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	q := actionsdb.New()
	job, err := q.GetWorkflowJobByID(ctx, pool, resp.Job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.Status != actionsdb.WorkflowJobStatusCompleted || !job.RunnerID.Valid || job.RunnerID.Int64 != runnerID ||
		!job.Conclusion.Valid || job.Conclusion.CheckConclusion != actionsdb.CheckConclusionSuccess {
		t.Fatalf("job not completed by runner: %+v", job)
	}
	run, err := q.GetWorkflowRunByID(ctx, pool, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusCompleted ||
		!run.Conclusion.Valid || run.Conclusion.CheckConclusion != actionsdb.CheckConclusionSuccess {
		t.Fatalf("run not completed successfully: %+v", run)
	}

	// The completed job is no longer counted against capacity, and no other
	// queued job exists, so the heartbeat is an empty 204.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerHeartbeatRespectsRepoConcurrencyCap(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	q := actionsdb.New()
	if _, err := q.UpsertActionsRepoPolicy(ctx, pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:                repoID,
		ActionsEnabled:        actionsdb.ActionsPolicyStateInherit,
		MaxRepoConcurrentJobs: pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}

	firstRunID := enqueueRunnerAPIRunWithTriggerID(t, pool, logger, repoID, userID, "repo-cap-1")
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 2)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))
	firstClaim := claimRunnerHeartbeat(t, router, token, 2)
	if firstClaim.Job.RunID != firstRunID {
		t.Fatalf("first claim run_id=%d want %d", firstClaim.Job.RunID, firstRunID)
	}

	secondRunID := enqueueRunnerAPIRunWithTriggerID(t, pool, logger, repoID, userID, "repo-cap-2")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":2}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("capped heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := q.ListJobsForRun(ctx, pool, secondRunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != actionsdb.WorkflowJobStatusQueued {
		t.Fatalf("second run job should remain queued/unclaimed: %+v", jobs)
	}
	secondJob, err := q.GetWorkflowJobByID(ctx, pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if secondJob.RunnerID.Valid {
		t.Fatalf("second run job should not have a runner: %+v", secondJob)
	}

	if _, err := q.UpdateWorkflowJobStatus(ctx, pool, actionsdb.UpdateWorkflowJobStatusParams{
		ID:         firstClaim.Job.ID,
		Status:     actionsdb.WorkflowJobStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
		CompletedAt: pgtype.Timestamptz{
			Time:  time.Now(),
			Valid: true,
		},
	}); err != nil {
		t.Fatalf("UpdateWorkflowJobStatus: %v", err)
	}
	secondClaim := claimRunnerHeartbeat(t, router, token, 2)
	if secondClaim.Job.RunID != secondRunID {
		t.Fatalf("second claim run_id=%d want %d", secondClaim.Job.RunID, secondRunID)
	}
}

func TestRunnerHeartbeatRespectsOwnerConcurrencyCap(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	firstRepoID, userID := setupRunnerAPIRepo(t, pool)
	secondRepo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: userID, Valid: true},
		Name:          "second",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo second: %v", err)
	}
	q := actionsdb.New()
	if _, err := q.UpsertActionsRepoPolicy(ctx, pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:                 secondRepo.ID,
		ActionsEnabled:         actionsdb.ActionsPolicyStateInherit,
		MaxOwnerConcurrentJobs: pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}

	firstRunID := enqueueRunnerAPIRunWithTriggerID(t, pool, logger, firstRepoID, userID, "owner-cap-1")
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 2)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))
	firstClaim := claimRunnerHeartbeat(t, router, token, 2)
	if firstClaim.Job.RunID != firstRunID {
		t.Fatalf("first claim run_id=%d want %d", firstClaim.Job.RunID, firstRunID)
	}

	secondRunID := enqueueRunnerAPIRunWithTriggerID(t, pool, logger, secondRepo.ID, userID, "owner-cap-2")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":2}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("owner-capped heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := q.ListJobsForRun(ctx, pool, secondRunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != actionsdb.WorkflowJobStatusQueued {
		t.Fatalf("second owner job should remain queued/unclaimed: %+v", jobs)
	}
	secondJob, err := q.GetWorkflowJobByID(ctx, pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if secondJob.RunnerID.Valid {
		t.Fatalf("second owner job should not have a runner: %+v", secondJob)
	}
}

func TestRunnerHeartbeatSiteDisableOverridesRepoEnable(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	q := actionsdb.New()
	if _, err := q.UpsertActionsRepoPolicy(ctx, pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:         repoID,
		ActionsEnabled: actionsdb.ActionsPolicyStateEnabled,
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}
	runID := enqueueRunnerAPIRunWithTriggerID(t, pool, logger, repoID, userID, "site-disable-claim")
	if _, err := pool.Exec(ctx, `UPDATE actions_site_policy SET actions_enabled = false WHERE id = true`); err != nil {
		t.Fatalf("disable site policy: %v", err)
	}

	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("site-disabled heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := q.ListJobsForRun(ctx, pool, runID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != actionsdb.WorkflowJobStatusQueued {
		t.Fatalf("site-disabled job should remain queued: %+v", jobs)
	}
	job, err := q.GetWorkflowJobByID(ctx, pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.RunnerID.Valid {
		t.Fatalf("site-disabled job should not have a runner: %+v", job)
	}
}

func TestRunnerHeartbeatDoesNotClaimWhenDraining(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, runnerID := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	q := actionsdb.New()
	if _, err := q.SetRunnerDraining(ctx, pool, actionsdb.SetRunnerDrainingParams{
		ID:          runnerID,
		DrainReason: "maintenance",
	}); err != nil {
		t.Fatalf("SetRunnerDraining: %v", err)
	}
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1,"host_name":"draining-host","version":"dev-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := q.ListJobsForRun(ctx, pool, runID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != actionsdb.WorkflowJobStatusQueued {
		t.Fatalf("job was claimed while runner drained: %#v", jobs)
	}
	job, err := q.GetWorkflowJobByID(ctx, pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.RunnerID.Valid {
		t.Fatalf("job was assigned to runner while drained: %+v", job)
	}
	runnerRow, err := q.GetRunnerByID(ctx, pool, runnerID)
	if err != nil {
		t.Fatalf("GetRunnerByID: %v", err)
	}
	if !runnerRow.DrainingAt.Valid || runnerRow.HostName != "draining-host" {
		t.Fatalf("runner drain/metadata not preserved: %+v", runnerRow)
	}
}

func TestRunnerJobTokenRejectedAfterRunnerRevoked(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, runnerID := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("claim status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	q := actionsdb.New()
	if _, err := q.RevokeRunner(ctx, pool, actionsdb.RevokeRunnerParams{
		ID:            runnerID,
		RevokedReason: "compromised",
	}); err != nil {
		t.Fatalf("RevokeRunner: %v", err)
	}
	if err := q.RevokeAllTokensForRunner(ctx, pool, runnerID); err != nil {
		t.Fatalf("RevokeAllTokensForRunner: %v", err)
	}

	statusReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/status", claim.Job.ID),
		strings.NewReader(`{"status":"running"}`))
	statusReq.Header.Set("Authorization", "Bearer "+claim.Token)
	statusRR := httptest.NewRecorder()
	router.ServeHTTP(statusRR, statusReq)
	if statusRR.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", statusRR.Code, statusRR.Body.String())
	}
}

func TestRunnerHeartbeatBypassesGlobalAnonAPILimit(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	signer := runnerAPISigner(t, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		BaseURL:     "https://shithub.test",
		RunnerJWT:   signer,
		RateLimiter: ratelimit.New(pool),
		APILimit: apilimit.Config{
			AuthedPerHour: 1,
			AnonPerHour:   1,
			Logger:        logger,
		},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	router := chi.NewRouter()
	h.Mount(router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	req.RemoteAddr = "10.0.0.77:12345"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("meta status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "10.0.0.77:12346"
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "60" {
		t.Errorf("runner heartbeat limit header: got %q, want 60", got)
	}
}

func TestRunnerSecretsAreClaimedAndServerScrubsLogs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	box := testRunnerAPISecretBox(t)
	if err := (actionsecrets.Deps{Pool: pool, Box: box}).Set(ctx, actionsecrets.RepoScope(repoID), "TOKEN", []byte("hunter2"), userID); err != nil {
		t.Fatalf("Set secret: %v", err)
	}

	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	signer := runnerAPISigner(t, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	router := newRunnerAPIRouterWithSecretBox(t, pool, logger, signer, box)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID         int64             `json:"id"`
			RunID      int64             `json:"run_id"`
			Secrets    map[string]string `json:"secrets"`
			MaskValues []string          `json:"mask_values"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claim.Job.RunID != runID || claim.Job.Secrets["TOKEN"] != "hunter2" || !containsString(claim.Job.MaskValues, "hunter2") {
		t.Fatalf("claim did not include masked secret context: %+v", claim.Job)
	}
	if _, err := actionsdb.New().GetWorkflowJobSecretMask(ctx, pool, claim.Job.ID); err != nil {
		t.Fatalf("GetWorkflowJobSecretMask: %v", err)
	}
	if err := (actionsecrets.Deps{Pool: pool, Box: box}).Set(ctx, actionsecrets.RepoScope(repoID), "TOKEN", []byte("rotated"), userID); err != nil {
		t.Fatalf("rotate secret after claim: %v", err)
	}

	rawLog := []byte("before hunter2 after\n")
	logBody := fmt.Sprintf(`{"seq":0,"chunk":%q}`, base64.StdEncoding.EncodeToString(rawLog))
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/logs", claim.Job.ID), strings.NewReader(logBody))
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("logs status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var logResp struct {
		NextToken string `json:"next_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &logResp); err != nil {
		t.Fatalf("decode log response: %v", err)
	}
	step, err := actionsdb.New().GetFirstStepForJob(ctx, pool, claim.Job.ID)
	if err != nil {
		t.Fatalf("GetFirstStepForJob: %v", err)
	}
	chunks, err := actionsdb.New().ListStepLogChunks(ctx, pool, actionsdb.ListStepLogChunksParams{
		StepID: step.ID,
		Seq:    -1,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListStepLogChunks: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks: %#v", chunks)
	}
	got := string(chunks[0].Chunk)
	if strings.Contains(got, "hunter2") || got != "before *** after\n" {
		t.Fatalf("stored log chunk was not scrubbed: %q", got)
	}

	postRunnerLogChunk(t, router, claim.Job.ID, logResp.NextToken, 1, []byte("::warning file=cmd/main.go,line=9,title=hunter2::message hunter2\n"))
	annotations, err := actionsdb.New().ListWorkflowAnnotationsForStep(ctx, pool, step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAnnotationsForStep: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("annotations=%#v", annotations)
	}
	ann := annotations[0]
	if ann.Level != actionsdb.WorkflowAnnotationLevelWarning ||
		ann.Title != "***" ||
		ann.Message != "message ***" ||
		ann.Path != "cmd/main.go" ||
		!ann.StartLine.Valid ||
		ann.StartLine.Int32 != 9 {
		t.Fatalf("annotation was not parsed and scrubbed: %#v", ann)
	}
	if strings.Contains(ann.Title, "hunter2") || strings.Contains(ann.Message, "hunter2") {
		t.Fatalf("annotation leaked secret: %#v", ann)
	}
}

func TestRunnerHeartbeatDoesNotClaimApprovalPendingRun(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	q := actionsdb.New()
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET need_approval = true WHERE id = $1`, runID); err != nil {
		t.Fatalf("mark run approval pending: %v", err)
	}
	if _, err := q.InsertWorkflowRunApproval(ctx, pool, actionsdb.InsertWorkflowRunApprovalParams{
		RunID:           runID,
		RequestedReason: "test approval",
	}); err != nil {
		t.Fatalf("InsertWorkflowRunApproval: %v", err)
	}

	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("pending heartbeat status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	jobs, err := q.ListJobsForRun(ctx, pool, runID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != actionsdb.WorkflowJobStatusQueued {
		t.Fatalf("approval-pending job changed: %+v", jobs)
	}

	if _, err := actionslifecycle.ApproveRun(ctx, actionslifecycle.Deps{Pool: pool, Logger: logger}, runID, userID); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("approved heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerDoesNotInjectSecretsIntoPullRequestRuns(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIEventRun(t, pool, logger, repoID, userID, trigger.EventPullRequest, map[string]any{"action": "opened"})
	box := testRunnerAPISecretBox(t)
	if err := (actionsecrets.Deps{Pool: pool, Box: box}).Set(ctx, actionsecrets.RepoScope(repoID), "TOKEN", []byte("hunter2"), userID); err != nil {
		t.Fatalf("Set secret: %v", err)
	}

	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	router := newRunnerAPIRouterWithSecretBox(t, pool, logger, runnerAPISigner(t, time.Now()), box)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Job struct {
			RunID      int64             `json:"run_id"`
			Secrets    map[string]string `json:"secrets"`
			MaskValues []string          `json:"mask_values"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claim.Job.RunID != runID {
		t.Fatalf("claimed wrong run: %+v", claim.Job)
	}
	if len(claim.Job.Secrets) != 0 || len(claim.Job.MaskValues) != 0 {
		t.Fatalf("pull_request run received secrets: %+v", claim.Job)
	}
}

func TestRunnerServerScrubsSecretSplitAcrossLogPosts(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	box := testRunnerAPISecretBox(t)
	if err := (actionsecrets.Deps{Pool: pool, Box: box}).Set(ctx, actionsecrets.RepoScope(repoID), "TOKEN", []byte("hunter2"), userID); err != nil {
		t.Fatalf("Set secret: %v", err)
	}

	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest", "linux"}, 1)
	signer := runnerAPISigner(t, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	router := newRunnerAPIRouterWithSecretBox(t, pool, logger, signer, box)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}

	next := postRunnerLogChunk(t, router, claim.Job.ID, claim.Token, 0, []byte("before hun"))
	next = postRunnerLogChunk(t, router, claim.Job.ID, next, 1, []byte("ter2 after\n"))

	step, err := actionsdb.New().GetFirstStepForJob(ctx, pool, claim.Job.ID)
	if err != nil {
		t.Fatalf("GetFirstStepForJob: %v", err)
	}
	chunks, err := actionsdb.New().ListStepLogChunks(ctx, pool, actionsdb.ListStepLogChunksParams{
		StepID: step.ID,
		Seq:    -1,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListStepLogChunks: %v", err)
	}
	var combined strings.Builder
	for _, chunk := range chunks {
		combined.Write(chunk.Chunk)
	}
	got := combined.String()
	if strings.Contains(got, "hunter2") || got != "before *** after\n" {
		t.Fatalf("stored log chunks were not scrubbed across boundary: chunks=%#v combined=%q next=%q", chunks, got, next)
	}
}

func TestRunnerHeartbeatRejectsBadToken(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newRunnerAPIRouter(t, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), runnerAPISigner(t, time.Now()))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer not-hex")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerArtifactUploadReturnsSignedURL(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()), storage.NewMemoryStore())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID    int64 `json:"id"`
			RunID int64 `json:"run_id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/artifacts/upload", claim.Job.ID),
		strings.NewReader(`{"name":"test-results.tgz","size_bytes":123}`))
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("artifact status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var upload struct {
		ArtifactID int64  `json:"artifact_id"`
		UploadURL  string `json:"upload_url"`
		NextToken  string `json:"next_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if upload.ArtifactID == 0 || upload.NextToken == "" ||
		!strings.HasPrefix(upload.UploadURL, "mem://actions/runs/") {
		t.Fatalf("unexpected upload response: %+v", upload)
	}
	artifacts, err := actionsdb.New().ListArtifactsForRun(ctx, pool, claim.Job.RunID)
	if err != nil {
		t.Fatalf("ListArtifactsForRun: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Name != "test-results.tgz" || artifacts[0].ByteCount != 123 {
		t.Fatalf("unexpected artifacts: %+v", artifacts)
	}
}

func TestRunnerArtifactUploadRejectsOrgStorageQuotaExceeded(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID, orgID := setupRunnerAPIOrgRepo(t, pool)
	if _, err := billing.UpsertOrgQuotaOverride(ctx, billing.Deps{Pool: pool}, billing.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            billing.QuotaKindStorageBytes,
		LimitValue:      100,
		Note:            "runner artifact quota test",
		CreatedByUserID: userID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride: %v", err)
	}
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()), storage.NewMemoryStore())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID    int64 `json:"id"`
			RunID int64 `json:"run_id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/artifacts/upload", claim.Job.ID),
		strings.NewReader(`{"name":"large.tgz","size_bytes":123}`))
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("artifact status: got %d, want 402; body=%s", rr.Code, rr.Body.String())
	}
	artifacts, err := actionsdb.New().ListArtifactsForRun(ctx, pool, claim.Job.RunID)
	if err != nil {
		t.Fatalf("ListArtifactsForRun: %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("quota-rejected upload still inserted artifacts: %+v", artifacts)
	}
}

func TestRunnerStepStatusEnqueuesFinalizeWorker(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()), storage.NewMemoryStore())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID    int64 `json:"id"`
			Steps []struct {
				ID  int64  `json:"id"`
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if len(claim.Job.Steps) < 2 {
		t.Fatalf("claim steps: %+v", claim.Job.Steps)
	}
	stepID := claim.Job.Steps[1].ID

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/steps/%d/status", claim.Job.ID, stepID),
		strings.NewReader(`{"status":"completed","conclusion":"success"}`))
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("step status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var statusResp struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		NextToken  string `json:"next_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if statusResp.Status != "completed" || statusResp.Conclusion != "success" || statusResp.NextToken == "" {
		t.Fatalf("unexpected step status response: %+v", statusResp)
	}
	step, err := actionsdb.New().GetWorkflowStepByID(ctx, pool, stepID)
	if err != nil {
		t.Fatalf("GetWorkflowStepByID: %v", err)
	}
	if step.Status != actionsdb.WorkflowStepStatusCompleted ||
		!step.Conclusion.Valid || step.Conclusion.CheckConclusion != actionsdb.CheckConclusionSuccess {
		t.Fatalf("step was not completed: %+v", step)
	}
	job, err := workerdb.New().ClaimJob(ctx, pool, workerdb.ClaimJobParams{
		Kind:     string(finalize.KindWorkflowFinalizeStep),
		LockedBy: pgtype.Text{String: "test", Valid: true},
	})
	if err != nil {
		t.Fatalf("ClaimJob finalize: %v", err)
	}
	var payload finalize.Payload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode worker payload: %v", err)
	}
	if payload.StepID != stepID {
		t.Fatalf("worker payload: %+v, want step_id %d", payload, stepID)
	}
}

func TestRunnerStepStatusRecordsTimeoutMetricOnce(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	token, _ := registerRunnerForTest(t, pool, []string{"ubuntu-latest"}, 1)
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat",
		strings.NewReader(`{"labels":["ubuntu-latest"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim struct {
		Token string `json:"token"`
		Job   struct {
			ID    int64 `json:"id"`
			Steps []struct {
				ID int64 `json:"id"`
			} `json:"steps"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if len(claim.Job.Steps) == 0 {
		t.Fatalf("claim steps: %+v", claim.Job.Steps)
	}
	stepID := claim.Job.Steps[0].ID
	before := actionsStepTimeoutsValue(t)

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/steps/%d/status", claim.Job.ID, stepID),
		strings.NewReader(`{"status":"completed","conclusion":"timed_out"}`))
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("step status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var statusResp struct {
		NextToken string `json:"next_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if statusResp.NextToken == "" {
		t.Fatalf("missing next token: %s", rr.Body.String())
	}
	if got := actionsStepTimeoutsValue(t); got != before+1 {
		t.Fatalf("timeout metric after first report: got %v, want %v", got, before+1)
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/steps/%d/status", claim.Job.ID, stepID),
		strings.NewReader(`{"status":"completed","conclusion":"timed_out"}`))
	req.Header.Set("Authorization", "Bearer "+statusResp.NextToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("duplicate step status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := actionsStepTimeoutsValue(t); got != before+1 {
		t.Fatalf("timeout metric after duplicate report: got %v, want still %v", got, before+1)
	}
}

func TestWorkflowJobCancelAPIRequestsCancellation(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	runID := enqueueRunnerAPIRun(t, pool, logger, repoID, userID)
	jobs, err := actionsdb.New().ListJobsForRun(ctx, pool, runID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs: %+v", jobs)
	}
	rawPAT := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	router := newRunnerAPIRouter(t, pool, logger, runnerAPISigner(t, time.Now()))

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/cancel", jobs[0].ID), nil)
	req.Header.Set("Authorization", "Bearer "+rawPAT)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		ChangedJobs  int  `json:"changed_jobs"`
		RunCompleted bool `json:"run_completed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ChangedJobs != 1 || !body.RunCompleted {
		t.Fatalf("response: %+v", body)
	}
	job, err := actionsdb.New().GetWorkflowJobByID(ctx, pool, jobs[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowJobByID: %v", err)
	}
	if job.Status != actionsdb.WorkflowJobStatusCancelled || !job.CancelRequested ||
		!job.Conclusion.Valid || job.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("job: %+v", job)
	}
	run, err := actionsdb.New().GetWorkflowRunByID(ctx, pool, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID: %v", err)
	}
	if run.Status != actionsdb.WorkflowRunStatusCompleted ||
		!run.Conclusion.Valid || run.Conclusion.CheckConclusion != actionsdb.CheckConclusionCancelled {
		t.Fatalf("run: %+v", run)
	}
}

func TestWorkflowRunRerunAPIQueuesOriginalCommitWorkflow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repoID, userID := setupRunnerAPIRepo(t, pool)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := rfs.InitBare(ctx, gitDir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	oldSHA := commitRunnerAPIWorkflow(t, gitDir, runnerAPIOldWorkflow, time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	wf := parseRunnerAPIWorkflow(t, runnerAPIOldWorkflow)
	original, err := trigger.Enqueue(ctx, trigger.Deps{Pool: pool, Logger: logger}, trigger.EnqueueParams{
		RepoID:         repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        oldSHA,
		HeadRef:        "refs/heads/trunk",
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    userID,
		TriggerEventID: "push:api-rerun",
		Workflow:       wf,
	})
	if err != nil {
		t.Fatalf("trigger.Enqueue original: %v", err)
	}
	if _, err := actionsdb.New().CompleteWorkflowRun(ctx, pool, actionsdb.CompleteWorkflowRunParams{
		ID:         original.RunID,
		Conclusion: actionsdb.CheckConclusionFailure,
	}); err != nil {
		t.Fatalf("CompleteWorkflowRun: %v", err)
	}
	_ = commitRunnerAPIWorkflow(t, gitDir, runnerAPINewWorkflow, time.Date(2026, 5, 11, 12, 5, 0, 0, time.UTC))

	rawPAT := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	router := newRunnerAPIRouterWithRepoFS(t, pool, logger, runnerAPISigner(t, time.Now()), rfs)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/rerun", original.RunID), nil)
	req.Header.Set("Authorization", "Bearer "+rawPAT)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("rerun status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		RunID       int64 `json:"run_id"`
		RunIndex    int64 `json:"run_index"`
		ParentRunID int64 `json:"parent_run_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.RunID == 0 || body.RunID == original.RunID || body.ParentRunID != original.RunID {
		t.Fatalf("response: %+v original=%+v", body, original)
	}
	rerun, err := actionsdb.New().GetWorkflowRunByID(ctx, pool, body.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRunByID rerun: %v", err)
	}
	if rerun.HeadSha != oldSHA || !rerun.ParentRunID.Valid || rerun.ParentRunID.Int64 != original.RunID {
		t.Fatalf("rerun row: %+v oldSHA=%s", rerun, oldSHA)
	}
	jobs, err := actionsdb.New().ListJobsForRun(ctx, pool, body.RunID)
	if err != nil {
		t.Fatalf("ListJobsForRun: %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobKey != "old_job" {
		t.Fatalf("rerun jobs came from wrong workflow: %+v", jobs)
	}
}

func postRunnerLogChunk(t *testing.T, router http.Handler, jobID int64, token string, seq int32, chunk []byte) string {
	t.Helper()
	body := fmt.Sprintf(`{"seq":%d,"chunk":%q}`, seq, base64.StdEncoding.EncodeToString(chunk))
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/logs", jobID), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("logs status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		NextToken string `json:"next_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode log response: %v", err)
	}
	if resp.NextToken == "" {
		t.Fatalf("empty next token in log response: %s", rr.Body.String())
	}
	return resp.NextToken
}

func newRunnerAPIRouter(
	t *testing.T,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	signer *runnerjwt.Signer,
	stores ...storage.ObjectStore,
) http.Handler {
	t.Helper()
	var store storage.ObjectStore
	if len(stores) > 0 {
		store = stores[0]
	}
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		BaseURL:     "https://shithub.test",
		RunnerJWT:   signer,
		ObjectStore: store,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func newRunnerAPIRouterWithRepoFS(
	t *testing.T,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	signer *runnerjwt.Signer,
	rfs *storage.RepoFS,
) http.Handler {
	t.Helper()
	h, err := apih.New(apih.Deps{
		Pool:      pool,
		Logger:    logger,
		BaseURL:   "https://shithub.test",
		RunnerJWT: signer,
		RepoFS:    rfs,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func newRunnerAPIRouterWithSecretBox(
	t *testing.T,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	signer *runnerjwt.Signer,
	box *secretbox.Box,
) http.Handler {
	t.Helper()
	h, err := apih.New(apih.Deps{
		Pool:      pool,
		Logger:    logger,
		BaseURL:   "https://shithub.test",
		RunnerJWT: signer,
		SecretBox: box,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func actionsStepTimeoutsValue(t *testing.T) float64 {
	t.Helper()
	var metric dto.Metric
	if err := metrics.ActionsStepTimeoutsTotal.Write(&metric); err != nil {
		t.Fatalf("read timeout metric: %v", err)
	}
	if metric.Counter == nil {
		return 0
	}
	return metric.Counter.GetValue()
}

const runnerAPIOldWorkflow = `name: CI
on: push
jobs:
  old_job:
    name: Old job
    runs-on: ubuntu-latest
    steps:
      - run: echo old
`

const runnerAPINewWorkflow = `name: CI
on: push
jobs:
  new_job:
    name: New job
    runs-on: ubuntu-latest
    steps:
      - run: echo new
`

func commitRunnerAPIWorkflow(t *testing.T, gitDir, body string, when time.Time) string {
	t.Helper()
	commit, err := (repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.test",
		Branch:      "trunk",
		Message:     "Update workflow",
		When:        when,
		Files: []repogit.FileEntry{
			{Path: ".shithub/workflows/ci.yml", Body: []byte(body)},
		},
	}).Build(context.Background())
	if err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return commit
}

func parseRunnerAPIWorkflow(t *testing.T, body string) *workflow.Workflow {
	t.Helper()
	wf, diags, err := workflow.Parse([]byte(body))
	if err != nil {
		t.Fatalf("workflow.Parse: %v", err)
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			t.Fatalf("workflow diagnostic: %v", d)
		}
	}
	return wf
}

func testRunnerAPISecretBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	box, err := secretbox.FromBytes(key)
	if err != nil {
		t.Fatalf("secretbox.FromBytes: %v", err)
	}
	return box
}

func setupRunnerAPIRepo(t *testing.T, pool *pgxpool.Pool) (repoID, userID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID, user.ID
}

func setupRunnerAPIOrgRepo(t *testing.T, pool *pgxpool.Pool) (repoID, userID, orgID int64) {
	t.Helper()
	ctx := context.Background()
	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, orgs.CreateParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo.ID, user.ID, org.ID
}

func enqueueRunnerAPIRun(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64) int64 {
	t.Helper()
	return enqueueRunnerAPIRunWithTriggerID(t, pool, logger, repoID, userID, "runner-api-test:push")
}

func enqueueRunnerAPIRunWithTriggerID(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64, triggerID string) int64 {
	t.Helper()
	return enqueueRunnerAPIEventRunWithTriggerID(t, pool, logger, repoID, userID, trigger.EventPush, map[string]any{"ref": "refs/heads/trunk"}, triggerID)
}

func enqueueRunnerAPIEventRun(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64, event trigger.EventKind, payload map[string]any) int64 {
	t.Helper()
	return enqueueRunnerAPIEventRunWithTriggerID(t, pool, logger, repoID, userID, event, payload, "runner-api-test:"+string(event))
}

func enqueueRunnerAPIEventRunWithTriggerID(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64, event trigger.EventKind, payload map[string]any, triggerID string) int64 {
	t.Helper()
	wf, diags, err := workflow.Parse([]byte(`name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`))
	if err != nil {
		t.Fatalf("workflow.Parse: %v", err)
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			t.Fatalf("workflow diagnostic: %v", d)
		}
	}
	res, err := trigger.Enqueue(context.Background(), trigger.Deps{Pool: pool, Logger: logger}, trigger.EnqueueParams{
		RepoID:         repoID,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		HeadSHA:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		EventKind:      event,
		EventPayload:   payload,
		ActorUserID:    userID,
		TriggerEventID: triggerID,
		Workflow:       wf,
	})
	if err != nil {
		t.Fatalf("trigger.Enqueue: %v", err)
	}
	return res.RunID
}

type runnerHeartbeatClaim struct {
	Token string `json:"token"`
	Job   struct {
		ID    int64 `json:"id"`
		RunID int64 `json:"run_id"`
	} `json:"job"`
}

func claimRunnerHeartbeat(t *testing.T, router http.Handler, token string, capacity int) runnerHeartbeatClaim {
	t.Helper()
	body := fmt.Sprintf(`{"labels":["ubuntu-latest","linux"],"capacity":%d}`, capacity)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var claim runnerHeartbeatClaim
	if err := json.Unmarshal(rr.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode heartbeat claim: %v", err)
	}
	if claim.Token == "" || claim.Job.ID == 0 || claim.Job.RunID == 0 {
		t.Fatalf("incomplete heartbeat claim: %+v", claim)
	}
	return claim
}

func registerRunnerForTest(t *testing.T, pool *pgxpool.Pool, labels []string, capacity int32) (token string, runnerID int64) {
	t.Helper()
	token, tokenHash, err := runnertoken.New()
	if err != nil {
		t.Fatalf("runnertoken.New: %v", err)
	}
	q := actionsdb.New()
	runner, err := q.InsertRunner(context.Background(), pool, actionsdb.InsertRunnerParams{
		Name:     "runner1",
		Labels:   labels,
		Capacity: capacity,
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := q.InsertRunnerToken(context.Background(), pool, actionsdb.InsertRunnerTokenParams{
		RunnerID:  runner.ID,
		TokenHash: tokenHash,
	}); err != nil {
		t.Fatalf("InsertRunnerToken: %v", err)
	}
	return token, runner.ID
}

func mintRunnerAPIPAT(t *testing.T, pool *pgxpool.Pool, userID int64, scopes ...string) string {
	t.Helper()
	raw, hash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("pat.Mint: %v", err)
	}
	if _, err := usersdb.New().InsertUserToken(context.Background(), pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "api test",
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      scopes,
	}); err != nil {
		t.Fatalf("InsertUserToken: %v", err)
	}
	return raw
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func runnerAPISigner(t *testing.T, now time.Time) *runnerjwt.Signer {
	t.Helper()
	signer, err := runnerjwt.NewFromKey(
		bytes.Repeat([]byte{0x7c}, 32),
		runnerjwt.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("runnerjwt.NewFromKey: %v", err)
	}
	return signer
}
