// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeartbeat_ClaimsJob(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runners/heartbeat" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer runner-token" {
			t.Fatalf("Authorization: %q", got)
		}
		var req HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if req.Capacity != 2 || strings.Join(req.Labels, ",") != "self-hosted,linux" {
			t.Fatalf("request: %#v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"token":"job-token",
			"expires_at":"2026-05-10T21:00:00Z",
			"job":{"id":10,"run_id":20,"repo_id":30,"run_index":1,"workflow_file":"ci.yml","workflow_name":"CI","head_sha":"abc","head_ref":"trunk","event":"push","job_key":"test","job_name":"test","runs_on":"ubuntu-latest","needs":[],"if":"","timeout_minutes":30,"permissions":{},"env":{"A":"B"},"steps":[{"id":40,"index":0,"step_id":"s1","name":"Run","if":"","run":"echo hi","uses":"","working_directory":"","env":{},"with":{},"continue_on_error":false}]}
		}`))
	}))
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claim, err := client.Heartbeat(t.Context(), HeartbeatRequest{Labels: []string{"self-hosted", "linux"}, Capacity: 2})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if claim.Token != "job-token" || claim.Job.ID != 10 || claim.Job.Steps[0].Run != "echo hi" {
		t.Fatalf("claim: %#v", claim)
	}
}

func TestHeartbeat_NoJob(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claim, err := client.Heartbeat(t.Context(), HeartbeatRequest{Capacity: 1})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if claim != nil {
		t.Fatalf("claim: %#v", claim)
	}
}

func TestUpdateStatus_UsesJobTokenAndParsesNextToken(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 5, 10, 21, 0, 0, 123, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs/10/status" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer job-token" {
			t.Fatalf("Authorization: %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if body["status"] != "running" || !strings.HasPrefix(body["started_at"], "2026-05-10T21:00:00.") {
			t.Fatalf("body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running","conclusion":null,"next_token":"next","next_token_expires_at":"2026-05-10T21:15:00Z"}`))
	}))
	defer srv.Close()
	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := client.UpdateStatus(t.Context(), 10, "job-token", StatusRequest{Status: "running", StartedAt: started})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if resp.NextToken != "next" {
		t.Fatalf("NextToken: %q", resp.NextToken)
	}
}

func TestUpdateStepStatus_UsesStepPathAndToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs/10/steps/20/status" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer job-token" {
			t.Fatalf("Authorization: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","conclusion":"success","next_token":"next","next_token_expires_at":"2026-05-10T21:15:00Z"}`))
	}))
	defer srv.Close()
	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := client.UpdateStepStatus(t.Context(), 10, 20, "job-token", StatusRequest{
		Status:     "completed",
		Conclusion: "success",
	})
	if err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	if resp.NextToken != "next" {
		t.Fatalf("NextToken: %q", resp.NextToken)
	}
}

func TestAppendLog_Base64EncodesChunk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if body["chunk"] != "aGkK" || body["seq"].(float64) != 7 {
			t.Fatalf("body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":true,"next_token":"next","next_token_expires_at":"2026-05-10T21:15:00Z"}`))
	}))
	defer srv.Close()
	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.AppendLog(t.Context(), 10, "job-token", LogRequest{Seq: 7, Chunk: []byte("hi\n")}); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
}

func TestCancelCheck_UsesJobTokenAndParsesResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs/10/cancel-check" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer job-token" {
			t.Fatalf("Authorization: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cancelled":true,"next_token":"next","next_token_expires_at":"2026-05-10T21:15:00Z"}`))
	}))
	defer srv.Close()
	client, err := New(Config{BaseURL: srv.URL, RunnerToken: "runner-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := client.CancelCheck(t.Context(), 10, "job-token")
	if err != nil {
		t.Fatalf("CancelCheck: %v", err)
	}
	if !resp.Cancelled || resp.NextToken != "next" {
		t.Fatalf("response: %#v", resp)
	}
}
