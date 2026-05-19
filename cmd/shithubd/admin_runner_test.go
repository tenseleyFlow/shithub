// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

func TestParseRunnerLabels(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty", raw: "", want: []string{}},
		{name: "trim and dedupe", raw: " self-hosted, linux,linux,ubuntu-24.04 ", want: []string{"self-hosted", "linux", "ubuntu-24.04"}},
		{name: "underscore dot dash", raw: "gpu_cuda-12.4", want: []string{"gpu_cuda-12.4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRunnerLabels(tt.raw)
			if err != nil {
				t.Fatalf("parseRunnerLabels: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("labels: got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRunnerLabelsRejectsInvalid(t *testing.T) {
	tests := []string{
		"linux,",
		"linux,,x64",
		"has space",
		"semi;colon",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseRunnerLabels(raw); err == nil {
				t.Fatal("parseRunnerLabels returned nil error")
			}
		})
	}
}

func TestWriteRunnerRegisterOutputJSON(t *testing.T) {
	expiresAt := time.Date(2026, 5, 12, 16, 30, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := writeRunnerRegisterOutput(&buf, "json", runnerRegisterOutput{
		ID:             7,
		Name:           "runner-7",
		Labels:         []string{"self-hosted", "linux", "ubuntu-latest", "x64"},
		Capacity:       2,
		Token:          "shithub_runner_token",
		TokenExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatalf("writeRunnerRegisterOutput: %v", err)
	}
	if strings.Contains(buf.String(), "Store this token") {
		t.Fatalf("json output included human prose: %s", buf.String())
	}
	var got struct {
		ID             int64     `json:"id"`
		Name           string    `json:"name"`
		Labels         []string  `json:"labels"`
		Capacity       int32     `json:"capacity"`
		Token          string    `json:"token"`
		TokenExpiresAt time.Time `json:"token_expires_at"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.ID != 7 || got.Name != "runner-7" || got.Capacity != 2 || got.Token != "shithub_runner_token" {
		t.Fatalf("unexpected output: %+v", got)
	}
	if !reflect.DeepEqual(got.Labels, []string{"self-hosted", "linux", "ubuntu-latest", "x64"}) {
		t.Fatalf("labels: got %#v", got.Labels)
	}
	if !got.TokenExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at: got %s want %s", got.TokenExpiresAt, expiresAt)
	}
}

func TestWriteRunnerRegisterOutputText(t *testing.T) {
	var buf bytes.Buffer
	err := writeRunnerRegisterOutput(&buf, "text", runnerRegisterOutput{
		ID:       8,
		Name:     "runner-8",
		Labels:   []string{"self-hosted", "linux", "ubuntu-latest", "x64"},
		Capacity: 1,
		Token:    "token-once",
	})
	if err != nil {
		t.Fatalf("writeRunnerRegisterOutput: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		"runner registered",
		"labels: self-hosted,linux,ubuntu-latest,x64",
		"token_expires_at: never",
		"token: token-once",
		"Store this token now",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("text output missing %q in %s", want, body)
		}
	}
}

func TestWriteRunnerQueueOutputJSON(t *testing.T) {
	now := time.Date(2026, 5, 12, 16, 30, 0, 0, time.UTC)
	rows := []actionsdb.ListQueuedWorkflowJobRunsOnRow{{
		RunsOn:              "windows-latest",
		QueuedJobs:          3,
		MatchingRunnerCount: 0,
		OldestQueuedAt:      pgtype.Timestamptz{Time: now.Add(-90 * time.Second), Valid: true},
	}}
	var buf bytes.Buffer
	if err := writeRunnerQueueOutput(&buf, "json", rows, now); err != nil {
		t.Fatalf("writeRunnerQueueOutput: %v", err)
	}
	var got []runnerQueueOutputRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows=%d body=%s", len(got), buf.String())
	}
	if got[0].RunsOn != "windows-latest" || got[0].QueuedJobs != 3 || got[0].MatchingRunnerCount != 0 {
		t.Fatalf("unexpected row: %+v", got[0])
	}
	if got[0].OldestQueuedSeconds != 90 {
		t.Fatalf("oldest seconds=%d", got[0].OldestQueuedSeconds)
	}
}

func TestWriteRunnerListOutputJSONIncludesOpsFields(t *testing.T) {
	now := time.Date(2026, 5, 12, 16, 30, 0, 0, time.UTC)
	rows := []actionsdb.ListRunnersRow{{
		ID:              7,
		Name:            "runner-a",
		Labels:          []string{"self-hosted", "linux"},
		Capacity:        2,
		Status:          actionsdb.WorkflowRunnerStatusBusy,
		LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-75 * time.Second), Valid: true},
		HostName:        "host-a",
		Version:         "dev-test",
		DrainingAt:      pgtype.Timestamptz{Time: now.Add(-30 * time.Second), Valid: true},
		DrainReason:     "maintenance",
		ActiveJobCount:  1,
	}}
	var buf bytes.Buffer
	if err := writeRunnerListOutput(&buf, "json", rows, now); err != nil {
		t.Fatalf("writeRunnerListOutput: %v", err)
	}
	var got []runnerListOutputRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows=%d body=%s", len(got), buf.String())
	}
	if got[0].HostName != "host-a" || got[0].Version != "dev-test" ||
		got[0].ActiveJobCount != 1 || got[0].LastHeartbeatAgeSeconds != 75 ||
		got[0].DrainingAt == "" || got[0].DrainReason != "maintenance" {
		t.Fatalf("unexpected row: %+v", got[0])
	}
}

func TestBuildRunnerPreflightReportOK(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	rows := []actionsdb.ListRunnersRow{
		{
			ID:              2,
			Name:            "shared-linux-1",
			Labels:          []string{"self-hosted", "linux", "ubuntu-latest", "x64"},
			Capacity:        1,
			Status:          actionsdb.WorkflowRunnerStatusIdle,
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-15 * time.Second), Valid: true},
			HostName:        "runner-host-1",
			Version:         "v0.1.0-1543-gdfb5ff88 (dfb5ff88, built 2026-05-19T09:50:24Z)",
		},
		{
			ID:              99,
			Name:            "windows-later",
			Labels:          []string{"windows-latest"},
			Status:          actionsdb.WorkflowRunnerStatusIdle,
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-15 * time.Second), Valid: true},
			Version:         "v0.1.0-1543-gdfb5ff88 (dfb5ff88, built 2026-05-19T09:50:24Z)",
		},
	}
	report := buildRunnerPreflightReport(rows, runnerPreflightOptions{
		ExpectedCommit:  "dfb5ff88f00dbabe",
		Labels:          []string{"ubuntu-latest"},
		MinRunners:      1,
		MaxHeartbeatAge: time.Minute,
		Now:             now,
	})
	if !report.OK {
		t.Fatalf("report not ok: %+v", report)
	}
	if report.CheckedRunnerCount != 1 || report.ReadyRunnerCount != 1 || report.SkippedRunnerCount != 1 {
		t.Fatalf("unexpected counts: %+v", report)
	}
	if report.Runners[0].LastHeartbeatAgeSeconds != 15 {
		t.Fatalf("heartbeat age = %d", report.Runners[0].LastHeartbeatAgeSeconds)
	}
}

func TestBuildRunnerPreflightReportFlagsDrift(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	rows := []actionsdb.ListRunnersRow{
		{
			ID:              2,
			Name:            "stale-runner",
			Labels:          []string{"ubuntu-latest"},
			Capacity:        1,
			Status:          actionsdb.WorkflowRunnerStatusIdle,
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true},
			Version:         "v0.1.0-1537-gf04cb8a3 (f04cb8a3, built 2026-05-19T08:40:54Z)",
		},
		{
			ID:              3,
			Name:            "missing-version",
			Labels:          []string{"ubuntu-latest"},
			Capacity:        1,
			Status:          actionsdb.WorkflowRunnerStatusIdle,
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-10 * time.Second), Valid: true},
		},
		{
			ID:              4,
			Name:            "draining-runner",
			Labels:          []string{"ubuntu-latest"},
			Capacity:        1,
			Status:          actionsdb.WorkflowRunnerStatusBusy,
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-10 * time.Second), Valid: true},
			Version:         "v0.1.0-1543-gdfb5ff88 (dfb5ff88, built 2026-05-19T09:50:24Z)",
			DrainingAt:      pgtype.Timestamptz{Time: now.Add(-30 * time.Second), Valid: true},
		},
		{
			ID:              5,
			Name:            "revoked-runner",
			Labels:          []string{"ubuntu-latest"},
			Status:          actionsdb.WorkflowRunnerStatusOffline,
			RevokedAt:       pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
			LastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-10 * time.Second), Valid: true},
		},
	}
	report := buildRunnerPreflightReport(rows, runnerPreflightOptions{
		ExpectedCommit:  "dfb5ff88",
		Labels:          []string{"ubuntu-latest"},
		MinRunners:      3,
		MaxHeartbeatAge: time.Minute,
		Now:             now,
	})
	if report.OK {
		t.Fatalf("report unexpectedly ok: %+v", report)
	}
	if report.CheckedRunnerCount != 3 || report.SkippedRunnerCount != 1 ||
		report.StaleCount != 1 || report.VersionDriftCount != 1 ||
		report.MissingVersionCount != 1 || report.DrainingCount != 1 {
		t.Fatalf("unexpected counts: %+v", report)
	}
	if err := runnerPreflightError(report); err == nil ||
		!strings.Contains(err.Error(), "stale=1") ||
		!strings.Contains(err.Error(), "version_drift=1") ||
		!strings.Contains(err.Error(), "missing_version=1") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerVersionMatchesCommit(t *testing.T) {
	version := "v0.1.0-1543-gdfb5ff88 (dfb5ff88, built 2026-05-19T09:50:24Z)"
	for _, expected := range []string{"dfb5ff88", "dfb5ff88f00dbabe"} {
		if !runnerVersionMatchesCommit(version, expected) {
			t.Fatalf("expected %q to match %q", version, expected)
		}
	}
	if runnerVersionMatchesCommit(version, "f04cb8a3") {
		t.Fatalf("stale commit unexpectedly matched")
	}
}

func TestWriteRunnerJobsOutputJSON(t *testing.T) {
	now := time.Date(2026, 5, 12, 16, 30, 0, 0, time.UTC)
	rows := []actionsdb.ListRunnerRunningJobsForAdminRow{{
		ID:                    101,
		RunID:                 55,
		JobKey:                "build",
		JobName:               "Build",
		RunsOn:                "ubuntu-latest",
		RunnerID:              pgtype.Int8{Int64: 7, Valid: true},
		Status:                actionsdb.WorkflowJobStatusRunning,
		CancelRequested:       true,
		StartedAt:             pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		RepoID:                9,
		RunIndex:              12,
		WorkflowFile:          ".shithub/workflows/ci.yml",
		WorkflowName:          "CI",
		HeadRef:               "refs/heads/trunk",
		HeadSha:               "abc1234",
		RepoName:              "scratch",
		OwnerLogin:            "mfwolffe",
		RunnerName:            pgtype.Text{String: "runner-7", Valid: true},
		RunnerStatus:          actionsdb.NullWorkflowRunnerStatus{WorkflowRunnerStatus: actionsdb.WorkflowRunnerStatusBusy, Valid: true},
		RunnerLastHeartbeatAt: pgtype.Timestamptz{Time: now.Add(-10 * time.Second), Valid: true},
	}}
	var buf bytes.Buffer
	if err := writeRunnerJobsOutput(&buf, "json", rows, now); err != nil {
		t.Fatalf("writeRunnerJobsOutput: %v", err)
	}
	var got []runnerJobOutputRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows=%d body=%s", len(got), buf.String())
	}
	row := got[0]
	if row.ID != 101 || row.Repository != "mfwolffe/scratch" || row.RunnerName != "runner-7" ||
		row.StartedAgeSeconds != 120 || row.RunnerLastHeartbeatAgeSeconds != 10 || !row.CancelRequested {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestFilterRunnerJobCandidatesHonorsActiveSet(t *testing.T) {
	now := time.Date(2026, 5, 12, 16, 30, 0, 0, time.UTC)
	rows := []actionsdb.ListRunnerRunningJobsForAdminRow{
		{ID: 101, RunID: 1, JobKey: "keep", Status: actionsdb.WorkflowJobStatusRunning, RepoID: 1, RepoName: "repo", OwnerLogin: "owner"},
		{ID: 102, RunID: 1, JobKey: "cancel", Status: actionsdb.WorkflowJobStatusRunning, RepoID: 1, RepoName: "repo", OwnerLogin: "owner"},
	}
	got := filterRunnerJobCandidates(rows, []int64{101}, now)
	if len(got) != 1 || got[0].ID != 102 {
		t.Fatalf("candidates = %+v, want only job 102", got)
	}
}

func TestWriteRunnerStateOutputText(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRunnerStateOutput(&buf, "text", "runner draining", runnerStateOutput{
		ID:          7,
		Name:        "runner-a",
		Status:      "busy",
		DrainingAt:  "2026-05-12T16:30:00Z",
		DrainReason: "maintenance",
	}); err != nil {
		t.Fatalf("writeRunnerStateOutput: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		"runner draining",
		"id: 7",
		"name: runner-a",
		"drain_reason: maintenance",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("text output missing %q in %s", want, body)
		}
	}
}

func TestParseRunnerIDRejectsInvalid(t *testing.T) {
	for _, raw := range []string{"", "0", "-1", "abc"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseRunnerID("admin runner test", raw); err == nil {
				t.Fatal("parseRunnerID returned nil error")
			}
		})
	}
}
