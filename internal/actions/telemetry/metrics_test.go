// SPDX-License-Identifier: AGPL-3.0-or-later

package telemetry

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

func TestRecordRunTerminalBoundsLabelsAndDuration(t *testing.T) {
	metrics.ActionsRunsCompletedTotal.Reset()
	metrics.ActionsRunDurationSeconds.Reset()

	started := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	completed := started.Add(75 * time.Second)
	RecordRunTerminal(actionsdb.WorkflowRun{
		Event:       actionsdb.WorkflowRunEvent("pull_request"),
		Status:      actionsdb.WorkflowRunStatusCompleted,
		Conclusion:  actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
		StartedAt:   pgtype.Timestamptz{Time: started, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: completed, Valid: true},
	})

	var completedMetric dto.Metric
	if err := metrics.ActionsRunsCompletedTotal.WithLabelValues("pull_request", "success").Write(&completedMetric); err != nil {
		t.Fatalf("read completed counter: %v", err)
	}
	if got := completedMetric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("completed counter = %v, want 1", got)
	}

	histogram, ok := metrics.ActionsRunDurationSeconds.WithLabelValues("pull_request", "success").(prometheus.Histogram)
	if !ok {
		t.Fatalf("duration metric is not a prometheus.Histogram")
	}
	var durationMetric dto.Metric
	if err := histogram.Write(&durationMetric); err != nil {
		t.Fatalf("read duration histogram: %v", err)
	}
	if got := durationMetric.GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("duration sample count = %v, want 1", got)
	}
	if got := durationMetric.GetHistogram().GetSampleSum(); got != 75 {
		t.Fatalf("duration sample sum = %v, want 75", got)
	}
}

func TestRecordStepTerminalUsesBoundedStepTypes(t *testing.T) {
	metrics.ActionsStepsCompletedTotal.Reset()

	RecordStepTerminal(actionsdb.WorkflowStep{
		UsesAlias:  "actions/checkout@v4",
		Status:     actionsdb.WorkflowStepStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
	})
	RecordStepTerminal(actionsdb.WorkflowStep{
		UsesAlias:  "actions/setup-python@v5",
		Status:     actionsdb.WorkflowStepStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
	})
	RecordStepTerminal(actionsdb.WorkflowStep{
		StepName:   "user controlled label with high cardinality potential",
		UsesAlias:  "owner/custom-action@v1",
		Status:     actionsdb.WorkflowStepStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionFailure, Valid: true},
	})
	RecordStepTerminal(actionsdb.WorkflowStep{
		RunCommand: "go test ./...",
		Status:     actionsdb.WorkflowStepStatusCompleted,
		Conclusion: actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusionSuccess, Valid: true},
	})

	assertStepCounter(t, "checkout", "success", 1)
	assertStepCounter(t, "setup-python", "success", 1)
	assertStepCounter(t, "uses", "failure", 1)
	assertStepCounter(t, "run", "success", 1)
}

func assertStepCounter(t *testing.T, stepType, conclusion string, want float64) {
	t.Helper()
	var metric dto.Metric
	if err := metrics.ActionsStepsCompletedTotal.WithLabelValues(stepType, conclusion).Write(&metric); err != nil {
		t.Fatalf("read step counter %s/%s: %v", stepType, conclusion, err)
	}
	if got := metric.GetCounter().GetValue(); got != want {
		t.Fatalf("step counter %s/%s = %v, want %v", stepType, conclusion, got, want)
	}
}
