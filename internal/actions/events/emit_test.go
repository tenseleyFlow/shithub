// SPDX-License-Identifier: AGPL-3.0-or-later

package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

func TestPayloadsExcludeSensitiveWorkflowState(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC), Valid: true}
	run := actionsdb.WorkflowRun{
		ID:             11,
		RepoID:         22,
		RunIndex:       3,
		WorkflowFile:   ".shithub/workflows/ci.yml",
		WorkflowName:   "CI",
		HeadSha:        strings.Repeat("a", 40),
		HeadRef:        "refs/heads/trunk",
		Event:          actionsdb.WorkflowRunEventPush,
		EventPayload:   []byte(`{"secret":"do-not-emit"}`),
		ActorUserID:    pgtype.Int8{Int64: 33, Valid: true},
		Status:         actionsdb.WorkflowRunStatusRunning,
		TriggerEventID: "push:demo",
		CreatedAt:      now,
		UpdatedAt:      now,
		StartedAt:      now,
	}
	job := actionsdb.WorkflowJob{
		ID:             44,
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "build",
		JobName:        "Build",
		RunsOn:         "ubuntu-latest",
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{"contents":"read"}`),
		JobEnv:         []byte(`{"TOKEN":"do-not-emit"}`),
		Status:         actionsdb.WorkflowJobStatusCompleted,
		Conclusion:     actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
		CreatedAt:      now,
		UpdatedAt:      now,
		StartedAt:      now,
		CompletedAt:    now,
	}

	payload, err := json.Marshal(jobEvent(run, job, ActionCompleted).Extra)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	got := string(payload)
	for _, forbidden := range []string{
		"event_payload",
		"do-not-emit",
		"permissions",
		"job_env",
		"TOKEN",
		"secret",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("payload leaked %q: %s", forbidden, got)
		}
	}
	for _, required := range []string{
		`"action":"completed"`,
		`"workflow_run"`,
		`"workflow_job"`,
		`"status":"completed"`,
		`"conclusion":"success"`,
		`"trigger_event_id":"push:demo"`,
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("payload missing %q: %s", required, got)
		}
	}
}
