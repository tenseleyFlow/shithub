// SPDX-License-Identifier: AGPL-3.0-or-later

// Package telemetry records bounded-cardinality Actions metrics.
package telemetry

import (
	"strings"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// RecordRunTerminal records terminal workflow-run counters and duration. It is
// idempotency-sensitive: callers must invoke it only when a run first becomes
// terminal.
func RecordRunTerminal(run actionsdb.WorkflowRun) {
	if !run.CompletedAt.Valid || !run.Conclusion.Valid {
		return
	}
	start := run.CreatedAt.Time
	if run.StartedAt.Valid {
		start = run.StartedAt.Time
	}
	duration := run.CompletedAt.Time.Sub(start).Seconds()
	if duration < 0 {
		duration = 0
	}
	event := string(run.Event)
	conclusion := string(run.Conclusion.CheckConclusion)
	metrics.ActionsRunsCompletedTotal.WithLabelValues(event, conclusion).Inc()
	metrics.ActionsRunDurationSeconds.WithLabelValues(event, conclusion).Observe(duration)
}

// RecordStepTerminal records terminal step outcomes using a bounded step_type
// label. Do not label by user-authored step name; workflow YAML would then be
// able to create unbounded Prometheus series.
func RecordStepTerminal(step actionsdb.WorkflowStep) {
	if !step.Conclusion.Valid {
		return
	}
	metrics.ActionsStepsCompletedTotal.WithLabelValues(stepType(step), string(step.Conclusion.CheckConclusion)).Inc()
}

func stepType(step actionsdb.WorkflowStep) string {
	uses := strings.TrimSpace(step.UsesAlias)
	if uses != "" {
		switch uses {
		case "actions/checkout@v4":
			return "checkout"
		case "shithub/upload-artifact@v1":
			return "upload-artifact"
		case "shithub/download-artifact@v1":
			return "download-artifact"
		default:
			return "uses"
		}
	}
	return "run"
}
