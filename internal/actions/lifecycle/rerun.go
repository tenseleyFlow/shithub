// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

var (
	ErrActorRequired              = errors.New("actions lifecycle: actor required")
	ErrRunNotRerunnable           = errors.New("actions lifecycle: run is not rerunnable")
	ErrWorkflowSourceUnavailable  = errors.New("actions lifecycle: workflow source unavailable")
	ErrWorkflowSourceInvalid      = errors.New("actions lifecycle: workflow source invalid")
	errWorkflowPayloadNotJSON     = errors.New("actions lifecycle: event payload is not a JSON object")
	errWorkflowSourceEmpty        = errors.New("actions lifecycle: workflow source is empty")
	errWorkflowSourceHasDiagError = errors.New("actions lifecycle: workflow source has error diagnostics")
)

// RerunResult summarizes the newly queued run produced from a terminal source
// run.
type RerunResult struct {
	ParentRunID int64
	RunID       int64
	RunIndex    int64
	CheckRunIDs []int64
}

// RerunRun queues a fresh workflow run from a terminal source run. It reads the
// workflow YAML from the source run's original head_sha, not from the current
// default branch, so reruns are reproducible even after workflow edits.
func RerunRun(ctx context.Context, deps Deps, runID, actorUserID int64) (RerunResult, error) {
	if deps.Pool == nil {
		return RerunResult{}, errors.New("actions lifecycle: nil Pool")
	}
	if deps.RepoFS == nil {
		return RerunResult{}, errors.New("actions lifecycle: nil RepoFS")
	}
	if actorUserID == 0 {
		return RerunResult{}, ErrActorRequired
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	q := actionsdb.New()
	run, err := q.GetWorkflowRunByID(ctx, deps.Pool, runID)
	if err != nil {
		return RerunResult{}, err
	}
	if !workflowRunRerunnable(run.Status) {
		return RerunResult{}, ErrRunNotRerunnable
	}
	payload, err := decodeRunEventPayload(run.EventPayload)
	if err != nil {
		return RerunResult{}, fmt.Errorf("%w: %w", ErrWorkflowSourceInvalid, err)
	}
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, run.RepoID)
	if err != nil {
		return RerunResult{}, err
	}
	owner, err := lifecycleRepoOwnerLogin(ctx, deps.Pool, repo)
	if err != nil {
		return RerunResult{}, err
	}
	gitDir, err := deps.RepoFS.RepoPath(owner, repo.Name)
	if err != nil {
		return RerunResult{}, err
	}
	body, err := repogit.ReadBlobBytes(ctx, gitDir, run.HeadSha, run.WorkflowFile, int64(workflow.MaxWorkflowFileBytes))
	if err != nil {
		return RerunResult{}, fmt.Errorf("%w: %w", ErrWorkflowSourceUnavailable, err)
	}
	if len(body) == 0 {
		return RerunResult{}, fmt.Errorf("%w: %w", ErrWorkflowSourceInvalid, errWorkflowSourceEmpty)
	}
	wf, diags, err := workflow.Parse(body)
	if err != nil {
		return RerunResult{}, fmt.Errorf("%w: %w", ErrWorkflowSourceInvalid, err)
	}
	if workflowHasErrorDiagnostics(diags) {
		return RerunResult{}, fmt.Errorf("%w: %w", ErrWorkflowSourceInvalid, errWorkflowSourceHasDiagError)
	}
	triggerID, err := rerunTriggerEventID(run.ID)
	if err != nil {
		return RerunResult{}, err
	}
	res, err := trigger.Enqueue(ctx, trigger.Deps{Pool: deps.Pool, Logger: logger}, trigger.EnqueueParams{
		RepoID:         run.RepoID,
		WorkflowFile:   run.WorkflowFile,
		HeadSHA:        run.HeadSha,
		HeadRef:        run.HeadRef,
		EventKind:      trigger.EventKind(run.Event),
		EventPayload:   payload,
		ActorUserID:    actorUserID,
		ParentRunID:    run.ID,
		TriggerEventID: triggerID,
		Workflow:       wf,
	})
	if err != nil {
		return RerunResult{}, err
	}
	return RerunResult{
		ParentRunID: run.ID,
		RunID:       res.RunID,
		RunIndex:    res.RunIndex,
		CheckRunIDs: res.CheckRunIDs,
	}, nil
}

func workflowRunRerunnable(status actionsdb.WorkflowRunStatus) bool {
	return status == actionsdb.WorkflowRunStatusCompleted ||
		status == actionsdb.WorkflowRunStatusCancelled
}

func decodeRunEventPayload(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errWorkflowPayloadNotJSON
	}
	return out, nil
}

func workflowHasErrorDiagnostics(diags []workflow.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == workflow.Error {
			return true
		}
	}
	return false
}

func rerunTriggerEventID(parentRunID int64) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("actions lifecycle: rerun id entropy: %w", err)
	}
	return fmt.Sprintf("rerun:%d:%s", parentRunID, hex.EncodeToString(b)), nil
}

func lifecycleRepoOwnerLogin(ctx context.Context, db interface {
	usersdb.DBTX
	orgsdb.DBTX
}, repo reposdb.Repo) (string, error) {
	if repo.OwnerUserID.Valid {
		u, err := usersdb.New().GetUserByID(ctx, db, repo.OwnerUserID.Int64)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}
	if repo.OwnerOrgID.Valid {
		o, err := orgsdb.New().GetOrgByID(ctx, db, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", err
		}
		return o.Slug, nil
	}
	return "", errors.New("repo has neither owner_user_id nor owner_org_id")
}
