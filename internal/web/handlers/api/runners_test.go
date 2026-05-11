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

	"github.com/tenseleyFlow/shithub/internal/actions/finalize"
	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsecrets "github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
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
		strings.NewReader(`{"labels":["ubuntu-latest","linux"],"capacity":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		Job   struct {
			ID           int64          `json:"id"`
			RunID        int64          `json:"run_id"`
			RepoID       int64          `json:"repo_id"`
			EventPayload map[string]any `json:"event_payload"`
			Steps        []struct {
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

func enqueueRunnerAPIRun(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, repoID, userID int64) int64 {
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
		EventKind:      trigger.EventPush,
		EventPayload:   map[string]any{"ref": "refs/heads/trunk"},
		ActorUserID:    userID,
		TriggerEventID: "push:test",
		Workflow:       wf,
	})
	if err != nil {
		t.Fatalf("trigger.Enqueue: %v", err)
	}
	return res.RunID
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
