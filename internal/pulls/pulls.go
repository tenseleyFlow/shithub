// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pulls owns the pull-request orchestrator. PRs reuse the S21
// `issues` row for title/body/state/timeline; this package owns the
// PR-specific surface — opening, synchronizing, mergeability detection,
// merge execution.
//
// Entry points are:
//
//	Create     — opens a PR (creates the issue row + the pull_requests row)
//	Synchronize — refreshes commit + file lists + emits a synchronized event
//	Mergeability — recomputes mergeable / mergeable_state via merge-tree
//	Merge      — performs the requested merge strategy in a temp worktree
//	Edit       — title/body
//	SetState   — close / reopen
//	SetReady   — draft → ready
package pulls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsevent "github.com/tenseleyFlow/shithub/internal/actions/event"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// Deps wires this package against the rest of the runtime.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// Audit is optional; when non-nil, Merge and SetState write audit
	// rows. Mirrors the issues orchestrator's contract so PR-side
	// state changes are equally traceable (S00-S25 audit, M).
	Audit *audit.Recorder
}

// Errors surfaced to handlers.
var (
	ErrSameBranch       = errors.New("pulls: base and head must differ")
	ErrBaseNotFound     = errors.New("pulls: base ref not found")
	ErrHeadNotFound     = errors.New("pulls: head ref not found")
	ErrNoCommitsToMerge = errors.New("pulls: head has no commits ahead of base")
	ErrAlreadyMerged    = errors.New("pulls: already merged")
	ErrAlreadyClosed    = errors.New("pulls: already closed")
	ErrMergeBlocked     = errors.New("pulls: merge blocked (mergeable_state != clean)")
	ErrMergeMethodOff   = errors.New("pulls: requested merge method is disabled on this repo")
	ErrConcurrentMerge  = errors.New("pulls: PR is being merged by another request")
	ErrPRNotFound       = errors.New("pulls: PR not found")
)

// CreateParams describes a new-PR request.
type CreateParams struct {
	RepoID       int64
	AuthorUserID int64
	Title        string
	Body         string
	BaseRef      string
	HeadRef      string
	Draft        bool
	GitDir       string // resolved from RepoFS by the caller
}

// CreateResult bundles the issue row + the PR row (post-snapshot).
type CreateResult struct {
	Issue       issuesdb.Issue
	PullRequest pullsdb.PullRequest
}

// Create opens a PR. Validates that base/head are distinct and resolve
// in the on-disk repo, snapshots their OIDs, then creates the issues
// row + pull_requests row in one tx. Mergeability is `unknown` until
// the worker job ticks.
func Create(ctx context.Context, deps Deps, p CreateParams) (CreateResult, error) {
	base := strings.TrimSpace(p.BaseRef)
	head := strings.TrimSpace(p.HeadRef)
	if base == "" || head == "" || base == head {
		return CreateResult{}, ErrSameBranch
	}
	baseOID, err := repogit.ResolveRefOID(ctx, p.GitDir, base)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return CreateResult{}, ErrBaseNotFound
		}
		return CreateResult{}, fmt.Errorf("resolve base: %w", err)
	}
	headOID, err := repogit.ResolveRefOID(ctx, p.GitDir, head)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return CreateResult{}, ErrHeadNotFound
		}
		return CreateResult{}, fmt.Errorf("resolve head: %w", err)
	}
	if baseOID == headOID {
		return CreateResult{}, ErrNoCommitsToMerge
	}

	// Open the issues row first via the issues orchestrator so we get
	// the per-repo number allocation, body markdown render, and
	// reference indexing for free.
	issueRow, err := issues.Create(ctx, issues.Deps{Pool: deps.Pool, Logger: deps.Logger}, issues.CreateParams{
		RepoID:       p.RepoID,
		AuthorUserID: p.AuthorUserID,
		Title:        p.Title,
		Body:         p.Body,
		Kind:         "pr",
	})
	if err != nil {
		return CreateResult{}, err
	}

	prRow, err := pullsdb.New().CreatePullRequest(ctx, deps.Pool, pullsdb.CreatePullRequestParams{
		IssueID:    issueRow.ID,
		BaseRef:    base,
		HeadRef:    head,
		HeadRepoID: p.RepoID,
		BaseOid:    baseOID,
		HeadOid:    headOID,
		Draft:      p.Draft,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("create pull_request: %w", err)
	}

	// Best-effort initial synchronize so the PR view has commits + files
	// even before the worker queue runs. Failures here don't fail the
	// open — the worker will retry on the next tick.
	if err := refreshCommitsAndFiles(ctx, deps, p.GitDir, prRow.IssueID, baseOID, headOID); err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "pulls: initial sync", "error", err, "pr_id", prRow.IssueID)
		}
	} else {
		maybeRequestCodeOwners(ctx, deps, p.GitDir, p.RepoID, prRow.IssueID, p.AuthorUserID, baseOID)
	}

	// Actions trigger (S41b): on PR open, fan out a workflow:trigger
	// with action="opened". Best-effort — failures log and let the
	// PR creation succeed. The collaborator gate lives in the PR
	// trigger helper (pr_jobs.go); this site just enqueues with
	// enough payload for the handler to evaluate.
	if err := enqueueOpenedActionsTrigger(ctx, deps, p, prRow, issueRow.Number, baseOID, headOID); err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "pulls: enqueue workflow:trigger",
				"error", err, "pr_id", prRow.IssueID)
		}
	}

	return CreateResult{Issue: issueRow, PullRequest: prRow}, nil
}

// enqueueOpenedActionsTrigger is the PR-create-side counterpart to
// jobs.enqueuePRActionsTrigger (which handles synchronize). Lives
// here in the pulls orchestrator so the open path stays
// self-contained — the alternative (a domain_event watcher in the
// jobs package) would need to round-trip through the queue just to
// observe the open.
//
// Trust gate: the trigger handler evaluates the author through
// internal/auth/policy and either queues the run as trusted or marks it
// approval-required. This helper only supplies the canonical PR event.
func enqueueOpenedActionsTrigger(ctx context.Context, deps Deps, p CreateParams, prRow pullsdb.PullRequest, prNumber int64, baseOID, headOID string) error {
	repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, p.RepoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	changed, err := repogit.ChangedPaths(ctx, p.GitDir, baseOID, headOID)
	if err != nil {
		changed = nil // best-effort; path-filtered workflows skip
	}
	authorLogin := ""
	if u, err := usersdb.New().GetUserByID(ctx, deps.Pool, p.AuthorUserID); err == nil {
		authorLogin = u.Username
	}
	payload := actionsevent.PullRequest(
		"opened", prNumber, p.Title,
		actionsevent.PRRef{Ref: prRow.HeadRef, SHA: prRow.HeadOid},
		actionsevent.PRRef{Ref: prRow.BaseRef, SHA: prRow.BaseOid},
		authorLogin,
	)
	job := trigger.JobPayload{
		RepoID:         repo.ID,
		HeadSHA:        prRow.HeadOid,
		HeadRef:        "refs/heads/" + prRow.HeadRef,
		EventKind:      trigger.EventPullRequest,
		EventPayload:   payload,
		ActorUserID:    p.AuthorUserID,
		TriggerEventID: fmt.Sprintf("pr_opened:%d:%s", prRow.IssueID, prRow.HeadOid),
		Action:         "opened",
		BaseRef:        prRow.BaseRef,
		HeadRefShort:   prRow.HeadRef,
		ChangedPaths:   changed,
	}
	if _, err := worker.Enqueue(ctx, deps.Pool, trigger.KindWorkflowTrigger, job, worker.EnqueueOptions{}); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

// refreshCommitsAndFiles is shared by Create + Synchronize. Truncates +
// re-fills `pull_request_commits` and `pull_request_files`.
func refreshCommitsAndFiles(ctx context.Context, deps Deps, gitDir string, prID int64, baseOID, headOID string) error {
	commits, err := repogit.CommitsBetweenDetail(ctx, gitDir, baseOID, headOID, 250)
	if err != nil {
		return fmt.Errorf("commits: %w", err)
	}
	files, err := repogit.FilesChangedBetween(ctx, gitDir, baseOID, headOID)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	q := pullsdb.New()
	if err := q.ClearPullRequestCommits(ctx, tx, prID); err != nil {
		return err
	}
	if err := q.ClearPullRequestFiles(ctx, tx, prID); err != nil {
		return err
	}
	for i, c := range commits {
		var ats, cts pgtype.Timestamptz
		if !c.AuthorWhen.IsZero() {
			ats = pgtype.Timestamptz{Time: c.AuthorWhen, Valid: true}
		}
		if !c.CommitterWhen.IsZero() {
			cts = pgtype.Timestamptz{Time: c.CommitterWhen, Valid: true}
		}
		if err := q.InsertPullRequestCommit(ctx, tx, pullsdb.InsertPullRequestCommitParams{
			PrID:           prID,
			Sha:            c.OID,
			Position:       int32(i),
			AuthorName:     c.AuthorName,
			AuthorEmail:    c.AuthorEmail,
			CommitterName:  c.CommitterName,
			CommitterEmail: c.CommitterEmail,
			Subject:        c.Subject,
			Body:           c.Body,
			AuthoredAt:     ats,
			CommittedAt:    cts,
		}); err != nil {
			return err
		}
	}
	for _, f := range files {
		oldPath := pgtype.Text{}
		if f.OldPath != "" {
			oldPath = pgtype.Text{String: f.OldPath, Valid: true}
		}
		if err := q.InsertPullRequestFile(ctx, tx, pullsdb.InsertPullRequestFileParams{
			PrID:      prID,
			Path:      f.Path,
			Status:    pullsdb.PrFileStatus(f.Status),
			OldPath:   oldPath,
			Additions: int32(f.Additions),
			Deletions: int32(f.Deletions),
			Changes:   int32(f.Additions + f.Deletions),
		}); err != nil {
			return err
		}
	}
	if err := q.SetPullRequestSnapshot(ctx, tx, pullsdb.SetPullRequestSnapshotParams{
		IssueID: prID, BaseOid: baseOID, HeadOid: headOID,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// Synchronize re-snapshots the PR's base/head OIDs, refreshes the
// commits + files lists, and emits a `synchronized` event into the
// issue timeline. Called from the pr:synchronize worker job after
// any push to the head ref.
func Synchronize(ctx context.Context, deps Deps, gitDir string, prID int64) error {
	q := pullsdb.New()
	pr, err := q.GetPullRequestByIssueID(ctx, deps.Pool, prID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPRNotFound
		}
		return err
	}
	baseOID, err := repogit.ResolveRefOID(ctx, gitDir, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("resolve base: %w", err)
	}
	headOID, err := repogit.ResolveRefOID(ctx, gitDir, pr.HeadRef)
	if err != nil {
		return fmt.Errorf("resolve head: %w", err)
	}
	if err := refreshCommitsAndFiles(ctx, deps, gitDir, prID, baseOID, headOID); err != nil {
		return err
	}
	issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, prID)
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	actorID := int64FromPg(issue.AuthorUserID)
	maybeRequestCodeOwners(ctx, deps, gitDir, issue.RepoID, prID, actorID, baseOID)
	// Re-anchor review comments against the new snapshot. Comments
	// whose original line still exists keep their thread; the rest
	// outdate (current_position=NULL) and surface in the "Show
	// outdated" toggle of the Files tab.
	if err := review.RemapAllForPR(ctx, review.Deps{Pool: deps.Pool, Logger: deps.Logger}, gitDir, prID, baseOID, headOID); err != nil {
		// Best-effort: a position-map miss shouldn't block the sync
		// pipeline. Log + continue.
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "pulls: position remap", "error", err, "pr_id", prID)
		}
	}
	// Reset mergeability to unknown so the next mergeability tick
	// recomputes against the fresh snapshot.
	if err := q.SetPullRequestMergeability(ctx, deps.Pool, pullsdb.SetPullRequestMergeabilityParams{
		IssueID:        prID,
		Mergeable:      pgtype.Bool{},
		MergeableState: pullsdb.PrMergeableStateUnknown,
	}); err != nil {
		return fmt.Errorf("reset mergeability: %w", err)
	}
	// Emit the synchronized timeline event.
	iq := issuesdb.New()
	if _, err := iq.InsertIssueEvent(ctx, deps.Pool, issuesdb.InsertIssueEventParams{
		IssueID: prID,
		Kind:    "synchronized",
		Meta:    []byte(fmt.Sprintf(`{"head_oid":%q}`, headOID)),
	}); err != nil {
		return fmt.Errorf("emit event: %w", err)
	}
	return nil
}

// Mergeability runs the merge-tree probe and persists the result.
// Order of state checks (highest priority first):
//
//	dirty   — git merge-tree reports conflicts
//	behind  — head has no commits ahead of base
//	blocked — required reviews missing OR an undismissed
//	          request_changes review exists (S23 gate)
//	clean   — merge-tree clean and review gate satisfied
//
// `blocked` is set by the S23 review evaluator; when no protection
// rule applies and no request_changes review exists, the gate is a
// no-op and we fall through to clean.
func Mergeability(ctx context.Context, deps Deps, gitDir string, prID int64) error {
	q := pullsdb.New()
	pr, err := q.GetPullRequestByIssueID(ctx, deps.Pool, prID)
	if err != nil {
		return err
	}
	if pr.BaseOid == "" || pr.HeadOid == "" {
		return nil // synchronize hasn't run yet; nothing to probe
	}
	// Behind: head has no commits ahead of base.
	commits, err := repogit.CommitsBetweenDetail(ctx, gitDir, pr.BaseOid, pr.HeadOid, 1)
	if err != nil && !errors.Is(err, repogit.ErrRefNotFound) {
		return err
	}
	if len(commits) == 0 {
		return q.SetPullRequestMergeability(ctx, deps.Pool, pullsdb.SetPullRequestMergeabilityParams{
			IssueID:        prID,
			Mergeable:      pgtype.Bool{Bool: false, Valid: true},
			MergeableState: pullsdb.PrMergeableStateBehind,
		})
	}
	res, err := repogit.ProbeMerge(ctx, gitDir, pr.BaseOid, pr.HeadOid)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if res.HasConflict {
		return q.SetPullRequestMergeability(ctx, deps.Pool, pullsdb.SetPullRequestMergeabilityParams{
			IssueID:        prID,
			Mergeable:      pgtype.Bool{Bool: false, Valid: true},
			MergeableState: pullsdb.PrMergeableStateDirty,
		})
	}
	// Composed gate: review (S23) + required-checks (S24). Either one
	// failing produces `blocked`. The two evaluators are independent;
	// each loads its own slice of the protection rule.
	issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, prID)
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	reviewGate, err := review.Evaluate(ctx, deps.Pool, review.GateInputs{
		RepoID:    issue.RepoID,
		BaseRef:   pr.BaseRef,
		BaseOID:   pr.BaseOid,
		GitDir:    gitDir,
		PRIssueID: prID,
	}, int64FromPg(issue.AuthorUserID))
	if err != nil {
		return fmt.Errorf("review gate: %w", err)
	}
	requiredCheckNames, err := loadRequiredCheckNames(ctx, deps.Pool, issue.RepoID, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("required-check rule lookup: %w", err)
	}
	checksGate, err := checks.EvaluateRequiredChecks(ctx, deps.Pool, checks.GateInputs{
		RepoID:        issue.RepoID,
		HeadSHA:       pr.HeadOid,
		RequiredNames: requiredCheckNames,
	})
	if err != nil {
		return fmt.Errorf("checks gate: %w", err)
	}
	state := pullsdb.PrMergeableStateClean
	mergeable := true
	if !reviewGate.Satisfied || !checksGate.Satisfied {
		state = pullsdb.PrMergeableStateBlocked
		mergeable = false
	}
	return q.SetPullRequestMergeability(ctx, deps.Pool, pullsdb.SetPullRequestMergeabilityParams{
		IssueID:        prID,
		Mergeable:      pgtype.Bool{Bool: mergeable, Valid: true},
		MergeableState: state,
	})
}

// loadRequiredCheckNames returns the `status_checks_required` list
// from the longest-pattern-matching protection rule for `baseRef`.
// Empty slice means no rule, no required checks.
func loadRequiredCheckNames(ctx context.Context, pool *pgxpool.Pool, repoID int64, baseRef string) ([]string, error) {
	rules, err := reposdb.New().ListBranchProtectionRules(ctx, pool, repoID)
	if err != nil {
		return nil, err
	}
	rule, ok := protection.MatchLongestRule(rules, baseRef)
	if !ok {
		return []string{}, nil
	}
	return rule.StatusChecksRequired, nil
}

// int64FromPg unwraps a pgtype.Int8; returns 0 when invalid.
func int64FromPg(p pgtype.Int8) int64 {
	if !p.Valid {
		return 0
	}
	return p.Int64
}

// EditPR updates the PR's title + body. Body markdown is re-rendered
// via the same pipeline issues.Create uses so HTML is consistent.
func EditPR(ctx context.Context, deps Deps, prID int64, title, body string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return issues.ErrEmptyTitle
	}
	if len(title) > 256 {
		return issues.ErrTitleTooLong
	}
	if len(body) > 65535 {
		return issues.ErrBodyTooLong
	}
	html := renderBodyHTML(ctx, deps, body)
	q := issuesdb.New()
	return q.UpdateIssueTitleBody(ctx, deps.Pool, issuesdb.UpdateIssueTitleBodyParams{
		ID:             prID,
		Title:          title,
		Body:           body,
		BodyHtmlCached: pgtype.Text{String: html, Valid: html != ""},
	})
}

// SetReady flips draft → false and emits a `ready_for_review` event.
func SetReady(ctx context.Context, deps Deps, actorUserID, prID int64) error {
	q := pullsdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := q.SetPullRequestDraft(ctx, tx, pullsdb.SetPullRequestDraftParams{IssueID: prID, Draft: false}); err != nil {
		return err
	}
	iq := issuesdb.New()
	if _, err := iq.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     prID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        "ready_for_review",
		Meta:        []byte("{}"),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// AllowedMethod returns true when the repo allows the named merge
// strategy. Falls open for unknown methods so callers get a clear
// error from the orchestrator.
func AllowedMethod(repo reposdb.Repo, method string) bool {
	switch method {
	case "merge":
		return repo.AllowMergeCommit
	case "squash":
		return repo.AllowSquashMerge
	case "rebase":
		return repo.AllowRebaseMerge
	}
	return false
}

// renderBodyHTML wraps markdown.RenderHTML with a logger-aware error
// path. PR body length is bounded upstream at 65535 chars by the
// orchestrator; markdown caps at 1 MiB. ErrInputTooLarge here means
// a precondition regressed — log loudly. (S00-S25 audit, M.)
func renderBodyHTML(ctx context.Context, deps Deps, body string) string {
	html, err := mdrender.RenderHTML([]byte(body))
	if err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "pulls: markdown render failed",
			"error", err, "body_bytes", len(body))
	}
	return html
}
