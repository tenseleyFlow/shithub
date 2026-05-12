// SPDX-License-Identifier: AGPL-3.0-or-later

// Package concurrency owns workflow-level Actions concurrency groups.
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tenseleyFlow/shithub/internal/actions/runstate"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
)

const (
	// MaxGroupChars mirrors workflow_runs.concurrency_group's CHECK
	// constraint. Enforcing it before insert gives workflow authors a useful
	// error instead of a generic constraint failure.
	MaxGroupChars = 256

	// CancelReason is the metrics label used when a newer run cancels older
	// group occupants.
	CancelReason = "concurrency"
)

// ResolveInput carries the trigger-time context available to concurrency.group
// expression evaluation. Secrets are intentionally not populated here.
type ResolveInput struct {
	Workflow     *workflow.Workflow
	EventPayload map[string]any
	HeadSHA      string
	HeadRef      string
}

// Resolution is the trigger-time concurrency policy for one workflow run.
type Resolution struct {
	Group            string
	CancelInProgress bool
}

// Resolve evaluates workflow.concurrency.group against the trigger context.
func Resolve(in ResolveInput) (Resolution, error) {
	if in.Workflow == nil {
		return Resolution{}, errors.New("actions concurrency: nil Workflow")
	}
	c := in.Workflow.Concurrency
	raw := strings.TrimSpace(c.Group.Raw)
	if raw == "" {
		return Resolution{}, nil
	}
	group, err := ResolveGroup(raw, EvalContext{
		EventPayload: in.EventPayload,
		HeadSHA:      in.HeadSHA,
		HeadRef:      in.HeadRef,
	})
	if err != nil {
		return Resolution{}, err
	}
	if group == "" {
		return Resolution{}, nil
	}
	return Resolution{Group: group, CancelInProgress: c.CancelInProgress}, nil
}

// EnforceParams identifies a newly enqueued run whose concurrency group should
// be checked against older active runs in the same repo.
type EnforceParams struct {
	Run              actionsdb.WorkflowRun
	CancelInProgress bool
}

// EnforceResult summarizes what the slot manager observed and changed.
type EnforceResult struct {
	BlockingRuns  []actionsdb.WorkflowRun
	CancelledJobs []actionsdb.WorkflowJob
}

// Enforce applies workflow-level concurrency rules. With cancel-in-progress it
// requests cancellation for older active occupants. Without it, this only locks
// and reports blockers; ClaimQueuedWorkflowJob keeps the newer run pending.
func Enforce(
	ctx context.Context,
	q *actionsdb.Queries,
	db actionsdb.DBTX,
	p EnforceParams,
) (EnforceResult, error) {
	if q == nil {
		return EnforceResult{}, errors.New("actions concurrency: nil Queries")
	}
	if strings.TrimSpace(p.Run.ConcurrencyGroup) == "" {
		return EnforceResult{}, nil
	}
	blockers, err := q.ListBlockingConcurrencyRunsForUpdate(ctx, db, actionsdb.ListBlockingConcurrencyRunsForUpdateParams{
		RepoID:           p.Run.RepoID,
		ConcurrencyGroup: p.Run.ConcurrencyGroup,
		RunID:            p.Run.ID,
	})
	if err != nil {
		return EnforceResult{}, err
	}
	out := EnforceResult{BlockingRuns: blockers}
	if !p.CancelInProgress || len(blockers) == 0 {
		return out, nil
	}
	for _, blocker := range blockers {
		changed, err := q.RequestWorkflowRunCancel(ctx, db, blocker.ID)
		if err != nil {
			return EnforceResult{}, err
		}
		for _, job := range changed {
			if job.Status == actionsdb.WorkflowJobStatusCancelled {
				if _, err := q.CancelOpenWorkflowStepsForJob(ctx, db, job.ID); err != nil {
					return EnforceResult{}, err
				}
			}
		}
		if len(changed) > 0 {
			if _, _, err := runstate.RollupAfterCancel(ctx, q, db, blocker.ID); err != nil {
				return EnforceResult{}, err
			}
			out.CancelledJobs = append(out.CancelledJobs, changed...)
		}
	}
	return out, nil
}

func validateGroup(group string) (string, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return "", nil
	}
	if utf8.RuneCountInString(group) > MaxGroupChars {
		return "", fmt.Errorf("actions concurrency: group exceeds %d characters", MaxGroupChars)
	}
	return group, nil
}
