// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestRefreshActionsPublishesQueueRunnerAndStorageGauges(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := actionsdb.New()

	user, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     "metrics-observer",
		DisplayName:  "Metrics Observer",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	repo, err := reposdb.New().CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: user.ID, Valid: true},
		Name:          "actions-metrics",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	run, err := q.InsertWorkflowRun(ctx, pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       repo.ID,
		RunIndex:     1,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "CI",
		HeadSha:      "0123456789abcdef0123456789abcdef01234567",
		HeadRef:      "trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte(`{}`),
		ActorUserID:  pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	job, err := q.InsertWorkflowJob(ctx, pool, actionsdb.InsertWorkflowJobParams{
		RunID:          run.ID,
		JobIndex:       0,
		JobKey:         "build",
		JobName:        "Build",
		RunsOn:         `["ubuntu-latest"]`,
		NeedsJobs:      []string{},
		TimeoutMinutes: 30,
		Permissions:    []byte(`{}`),
		JobEnv:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowJob: %v", err)
	}
	step, err := q.InsertWorkflowStep(ctx, pool, actionsdb.InsertWorkflowStepParams{
		JobID:            job.ID,
		StepIndex:        0,
		StepID:           "test",
		StepName:         "Test",
		RunCommand:       "go test ./...",
		WorkingDirectory: ".",
		StepEnv:          []byte(`{}`),
		StepWith:         []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowStep: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_steps SET log_object_key = $1, log_byte_count = $2 WHERE id = $3`, "actions/logs/test.log", int64(123), step.ID); err != nil {
		t.Fatalf("mark step log object: %v", err)
	}
	if _, err := q.AppendStepLogChunk(ctx, pool, actionsdb.AppendStepLogChunkParams{
		StepID: step.ID,
		Seq:    0,
		Chunk:  []byte("hello"),
	}); err != nil {
		t.Fatalf("AppendStepLogChunk: %v", err)
	}
	if _, err := q.InsertArtifact(ctx, pool, actionsdb.InsertArtifactParams{
		RunID:     run.ID,
		Name:      "bundle",
		ObjectKey: "actions/artifacts/bundle.zip",
		ByteCount: 2048,
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().UTC().Add(24 * time.Hour),
			Valid: true,
		},
	}); err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}
	runner, err := q.InsertRunner(ctx, pool, actionsdb.InsertRunnerParams{
		Name:               "runner-a",
		Labels:             []string{"self-hosted", "linux", "ubuntu-latest"},
		Capacity:           3,
		RegisteredByUserID: pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRunner: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runners SET status = 'busy', last_heartbeat_at = now() - interval '75 seconds', draining_at = now(), drain_reason = 'maintenance' WHERE id = $1`, runner.ID); err != nil {
		t.Fatalf("touch runner heartbeat: %v", err)
	}

	resetActionsObserverGauges()
	refreshActions(ctx, pool)

	assertGauge(t, ActionsQueueDepth, []string{"runs"}, 1)
	assertGauge(t, ActionsQueueDepth, []string{"jobs"}, 1)
	assertGauge(t, ActionsQueueDepthByLabels, []string{`["ubuntu-latest"]`}, 1)
	assertGauge(t, ActionsActive, []string{"runs"}, 0)
	assertGauge(t, ActionsActive, []string{"jobs"}, 0)
	assertGauge(t, ActionsRunnerCapacity, []string{"runner-a", "busy"}, 3)
	assertGauge(t, ActionsRunnerOnline, []string{"runner-a"}, 0)
	assertGauge(t, ActionsRunnerDraining, []string{"runner-a"}, 1)
	assertPlainGauge(t, ActionsRunnerStaleTotal, 1)
	if got := gaugeValue(t, ActionsRunnerHeartbeatAgeSeconds, []string{"runner-a", "busy"}); got < 60 {
		t.Fatalf("runner heartbeat age = %v, want >= 60", got)
	}
	assertGauge(t, ActionsStorageObjects, []string{"artifacts"}, 1)
	assertGauge(t, ActionsStorageBytes, []string{"artifacts"}, 2048)
	assertGauge(t, ActionsStorageObjects, []string{"step_logs"}, 1)
	assertGauge(t, ActionsStorageBytes, []string{"step_logs"}, 123)
	assertGauge(t, ActionsStorageObjects, []string{"hot_log_chunks"}, 1)
	assertGauge(t, ActionsStorageBytes, []string{"hot_log_chunks"}, 5)
}

type labeledGauge interface {
	WithLabelValues(lvs ...string) prometheus.Gauge
}

func resetActionsObserverGauges() {
	ActionsQueueDepth.Reset()
	ActionsQueueDepthByLabels.Reset()
	ActionsActive.Reset()
	ActionsRunnerHeartbeatAgeSeconds.Reset()
	ActionsRunnerOnline.Reset()
	ActionsRunnerDraining.Reset()
	ActionsRunnerStaleTotal.Set(0)
	ActionsRunnerCapacity.Reset()
	ActionsStorageObjects.Reset()
	ActionsStorageBytes.Reset()
}

func assertGauge(t *testing.T, vec labeledGauge, labels []string, want float64) {
	t.Helper()
	if got := gaugeValue(t, vec, labels); got != want {
		t.Fatalf("gauge %v = %v, want %v", labels, got, want)
	}
}

func gaugeValue(t *testing.T, vec labeledGauge, labels []string) float64 {
	t.Helper()
	var metric dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&metric); err != nil {
		t.Fatalf("read gauge %v: %v", labels, err)
	}
	if metric.Gauge == nil {
		t.Fatalf("gauge %v missing", labels)
	}
	return metric.Gauge.GetValue()
}

func assertPlainGauge(t *testing.T, gauge prometheus.Gauge, want float64) {
	t.Helper()
	var metric dto.Metric
	if err := gauge.Write(&metric); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	if metric.Gauge == nil {
		t.Fatal("gauge missing")
	}
	if got := metric.Gauge.GetValue(); got != want {
		t.Fatalf("gauge = %v, want %v", got, want)
	}
}
