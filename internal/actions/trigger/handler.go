// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindWorkflowTrigger names the worker kind that runs the trigger
// pipeline: discover workflows in a repo's HEAD at a given SHA, match
// each against the triggering event, enqueue runs for the matches.
//
// Convention follows the webhook subsystem (`webhook:fanout`,
// `webhook:deliver`, etc.) — each subsystem owns its kind constants
// alongside its handlers.
const KindWorkflowTrigger worker.Kind = "workflow:trigger"

// JobDeps is the wired set of runtime dependencies the handler needs.
// Mirrors trigger.Deps but adds RepoFS for git-tree access.
type JobDeps struct {
	Deps
	RepoFS *storage.RepoFS
}

// JobPayload is the JSON shape the enqueueing site (push_process,
// PR pipeline, dispatch HTTP, schedule sweep) writes onto the worker
// jobs table for KindWorkflowTrigger.
//
// Filter-evaluation fields (Branch, Tag, Action, BaseRef, HeadRefShort,
// ChangedPaths, Cron) are extracted from the canonical event payload
// by the caller and passed explicitly so the handler can build a
// trigger.Event without needing to know the canonical event-payload
// schema's internal layout.
type JobPayload struct {
	RepoID         int64          `json:"repo_id"`
	HeadSHA        string         `json:"head_sha"`
	HeadRef        string         `json:"head_ref"`
	EventKind      EventKind      `json:"event_kind"`
	EventPayload   map[string]any `json:"event_payload"`
	ActorUserID    int64          `json:"actor_user_id,omitempty"`
	TriggerEventID string         `json:"trigger_event_id"`

	// Filter hints. Optional — populated only when relevant to the
	// event kind; see trigger.Event for the per-kind shape.
	Branch       string   `json:"branch,omitempty"`
	Tag          string   `json:"tag,omitempty"`
	Action       string   `json:"action,omitempty"`
	BaseRef      string   `json:"base_ref,omitempty"`
	HeadRefShort string   `json:"head_ref_short,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	Cron         string   `json:"cron,omitempty"`
}

// Handler returns a worker handler that:
//
//  1. Loads the repo (to resolve its on-disk git path).
//  2. Discovers `.shithub/workflows/*.yml` at the payload's head_sha.
//  3. Parses each. Skips files with parser errors (logged) so a
//     single bad workflow doesn't block its peers on the same commit.
//  4. Runs Match against the trigger.Event built from the payload's
//     filter hints.
//  5. For each matched workflow, calls Enqueue. AlreadyExists is
//     logged + ignored (idempotency working as designed).
//
// Errors during discovery or repo-lookup are returned (worker retries).
// Per-file errors after a successful discovery are logged + skipped:
// the handler returns nil so the worker doesn't replay the whole batch
// because of one malformed workflow.
func Handler(deps JobDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p JobPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("trigger: bad payload: %w", err))
		}
		if p.RepoID == 0 || p.HeadSHA == "" || p.TriggerEventID == "" {
			return worker.PoisonError(errors.New("trigger: payload missing repo_id, head_sha, or trigger_event_id"))
		}

		repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, p.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("trigger: repo %d not found", p.RepoID))
			}
			return fmt.Errorf("trigger: load repo: %w", err)
		}
		ownerLogin, err := resolveOwnerLogin(ctx, deps.Pool, repo)
		if err != nil {
			return fmt.Errorf("trigger: resolve owner: %w", err)
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerLogin, repo.Name)
		if err != nil {
			return fmt.Errorf("trigger: repo path: %w", err)
		}

		matchStart := time.Now()
		defer func() {
			metrics.ActionsTriggerMatchDurationSeconds.Observe(time.Since(matchStart).Seconds())
		}()

		files, skips, err := Discover(ctx, gitDir, p.HeadSHA)
		if err != nil {
			return fmt.Errorf("trigger: discover: %w", err)
		}
		for _, s := range skips {
			deps.Logger.WarnContext(ctx, "trigger: workflow skipped at discovery",
				"repo_id", p.RepoID, "head_sha", p.HeadSHA, "path", s.Path, "reason", s.Reason)
		}
		if len(files) == 0 {
			return nil // no CI on this repo; common case, not an error.
		}

		ev := eventFromPayload(p)
		for _, f := range files {
			w, diags, perr := workflow.Parse(f.Bytes)
			if perr != nil {
				deps.Logger.WarnContext(ctx, "trigger: parse error",
					"repo_id", p.RepoID, "path", f.Path, "error", perr)
				continue
			}
			if hasParseError(diags) {
				deps.Logger.WarnContext(ctx, "trigger: workflow has Error-severity diagnostics",
					"repo_id", p.RepoID, "path", f.Path, "diagnostics", diags)
				continue
			}
			if !Match(w, ev) {
				continue
			}
			if _, err := Enqueue(ctx, deps.Deps, EnqueueParams{
				RepoID:         p.RepoID,
				WorkflowFile:   f.Path,
				HeadSHA:        p.HeadSHA,
				HeadRef:        p.HeadRef,
				EventKind:      p.EventKind,
				EventPayload:   p.EventPayload,
				ActorUserID:    p.ActorUserID,
				TriggerEventID: p.TriggerEventID,
				Workflow:       w,
			}); err != nil {
				deps.Logger.WarnContext(ctx, "trigger: enqueue failed",
					"repo_id", p.RepoID, "path", f.Path, "trigger_event_id", p.TriggerEventID, "error", err)
				// Continue to the next workflow — one failure shouldn't
				// block the rest. The worker layer doesn't retry the
				// whole batch on a partial failure.
				continue
			}
		}
		return nil
	}
}

// eventFromPayload assembles the typed Event consumed by Match from
// the JSON payload's filter hints.
func eventFromPayload(p JobPayload) Event {
	return Event{
		Kind:         p.EventKind,
		Ref:          p.HeadRef,
		Branch:       p.Branch,
		Tag:          p.Tag,
		Action:       p.Action,
		BaseRef:      p.BaseRef,
		HeadRef:      p.HeadRefShort,
		ChangedPaths: p.ChangedPaths,
		Cron:         p.Cron,
	}
}

// hasParseError matches workflow.Error severity. Workflows with only
// Warning-severity diagnostics still trigger (per the github-alias
// deprecation pattern).
func hasParseError(diags []workflow.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == workflow.Error {
			return true
		}
	}
	return false
}

// resolveOwnerLogin maps a repo's owner FK (user XOR org) to the
// short login string used by RepoFS.RepoPath. Mirrors the lookup
// the web layer does in its repo-resolution middleware.
func resolveOwnerLogin(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo) (string, error) {
	if repo.OwnerUserID.Valid {
		u, err := usersdb.New().GetUserByID(ctx, pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}
	if repo.OwnerOrgID.Valid {
		o, err := orgsdb.New().GetOrgByID(ctx, pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", err
		}
		return o.Slug, nil
	}
	return "", errors.New("repo has neither owner_user_id nor owner_org_id")
}
