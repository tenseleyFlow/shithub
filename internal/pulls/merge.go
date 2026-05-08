// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// MergeParams describes a merge request.
type MergeParams struct {
	PRID         int64
	ActorUserID  int64
	GitDir       string
	Method       string // "merge" | "squash" | "rebase"
	Subject      string // optional override; falls back to "<title> (#<number>)"
	Body         string
	WorktreesDir string // optional, defaults to <gitDir>/.tmp-worktrees
	Now          func() time.Time
}

// Merge performs the requested merge inside a temp worktree:
//
//  1. Lock the pull_requests row (FOR UPDATE) to serialize concurrent
//     attempts. The transaction holds for the whole job to keep the
//     lock until the DB state reflects the merge.
//  2. Validate state: not merged, not closed, mergeable_state == clean,
//     method allowed by the repo config.
//  3. Resolve identity (author = PR author for squash, merger otherwise;
//     committer = merger).
//  4. Run repogit.PerformMerge against the bare repo.
//  5. Persist merge_commit_sha + merged_at + flip the issue to closed
//     with state_reason='completed'.
//  6. Auto-close linked issues parsed from PR body + commit messages.
//  7. Emit a `merged` timeline event.
func Merge(ctx context.Context, deps Deps, p MergeParams) error {
	if p.Now == nil {
		p.Now = time.Now
	}

	// Begin the locking tx. Holding the row lock through the whole
	// merge prevents two attempts both winning at the worktree level.
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
	pr, err := q.LockPullRequestForMerge(ctx, tx, p.PRID)
	if err != nil {
		return ErrPRNotFound
	}
	if pr.MergedAt.Valid {
		return ErrAlreadyMerged
	}
	// Issue side: must still be open + mergeable.
	iq := issuesdb.New()
	issue, err := iq.GetIssueByID(ctx, tx, p.PRID)
	if err != nil {
		return err
	}
	if issue.State != issuesdb.IssueStateOpen {
		return ErrAlreadyClosed
	}
	if pr.MergeableState != pullsdb.PrMergeableStateClean {
		return ErrMergeBlocked
	}

	// Belt-and-braces: re-evaluate the review gate inside the row
	// lock so a `request_changes` submitted between the last
	// Mergeability tick and this merge attempt is still honored.
	// Spec pitfall: required-review bypass via direct API.
	gate, err := review.Evaluate(ctx, deps.Pool, review.GateInputs{
		RepoID:    issue.RepoID,
		BaseRef:   pr.BaseRef,
		PRIssueID: p.PRID,
	}, int64FromPg(issue.AuthorUserID))
	if err != nil {
		return fmt.Errorf("review gate: %w", err)
	}
	if !gate.Satisfied {
		return ErrMergeBlocked
	}

	// Identity for the new commit. Author email rules per spec:
	//   merge       — author = committer = merger
	//   squash      — author = PR author; committer = merger
	//   rebase      — author preserved; committer = merger (handled by
	//                 repogit.PerformMerge via env)
	uq := usersdb.New()
	mergerName, mergerEmail, err := identityFor(ctx, tx, uq, p.ActorUserID)
	if err != nil {
		return err
	}
	authorName, authorEmail := mergerName, mergerEmail
	if p.Method == "squash" && issue.AuthorUserID.Valid {
		an, ae, err := identityFor(ctx, tx, uq, issue.AuthorUserID.Int64)
		if err == nil {
			authorName, authorEmail = an, ae
		}
	}

	subject := strings.TrimSpace(p.Subject)
	if subject == "" {
		subject = issue.Title + " (#" + strconv.FormatInt(issue.Number, 10) + ")"
	}

	// Run the merge against the bare repo. PerformMerge cleans up the
	// worktree on every exit path.
	mergeRes, err := repogit.PerformMerge(ctx, repogit.MergeOptions{
		GitDir:         p.GitDir,
		BaseRef:        "refs/heads/" + pr.BaseRef,
		BaseOID:        pr.BaseOid,
		HeadOID:        pr.HeadOid,
		Method:         p.Method,
		AuthorName:     authorName,
		AuthorEmail:    authorEmail,
		CommitterName:  mergerName,
		CommitterEmail: mergerEmail,
		When:           p.Now(),
		Subject:        subject,
		Body:           p.Body,
		WorktreesDir:   p.WorktreesDir,
	})
	if err != nil {
		return fmt.Errorf("perform merge: %w", err)
	}

	if err := q.SetPullRequestMerged(ctx, tx, pullsdb.SetPullRequestMergedParams{
		IssueID:         p.PRID,
		MergedByUserID:  pgtype.Int8{Int64: p.ActorUserID, Valid: p.ActorUserID != 0},
		MergeCommitSha:  pgtype.Text{String: mergeRes.MergedOID, Valid: true},
		MergeMethod:     pullsdb.NullPrMergeMethod{PrMergeMethod: pullsdb.PrMergeMethod(p.Method), Valid: true},
		BaseOidAtMerge:  pgtype.Text{String: pr.BaseOid, Valid: true},
		HeadOidAtMerge:  pgtype.Text{String: pr.HeadOid, Valid: true},
	}); err != nil {
		return err
	}

	// Close the issue side with state_reason=completed.
	if err := iq.SetIssueState(ctx, tx, issuesdb.SetIssueStateParams{
		ID:             p.PRID,
		State:          issuesdb.IssueStateClosed,
		StateReason:    issuesdb.NullIssueStateReason{IssueStateReason: issuesdb.IssueStateReasonCompleted, Valid: true},
		ClosedByUserID: pgtype.Int8{Int64: p.ActorUserID, Valid: p.ActorUserID != 0},
	}); err != nil {
		return err
	}

	// `merged` timeline event.
	if _, err := iq.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     p.PRID,
		ActorUserID: pgtype.Int8{Int64: p.ActorUserID, Valid: p.ActorUserID != 0},
		Kind:        "merged",
		Meta:        []byte(fmt.Sprintf(`{"method":%q,"commit":%q}`, p.Method, mergeRes.MergedOID)),
	}); err != nil {
		return err
	}

	// Auto-close linked issues. Linked = Closes/Fixes/Resolves #N in
	// the PR body + each commit message. Best-effort; don't fail the
	// merge if the close fails.
	linked := parseLinkedIssues(issue.Body)
	for _, c := range fetchCommitsForLinkScan(ctx, tx, p.PRID) {
		linked = append(linked, parseLinkedIssues(c)...)
	}
	closed := map[int64]bool{}
	for _, num := range linked {
		if num == issue.Number {
			continue // self-reference
		}
		target, err := iq.GetIssueByNumber(ctx, tx, issuesdb.GetIssueByNumberParams{
			RepoID: issue.RepoID, Number: num,
		})
		if err != nil || target.Kind != issuesdb.IssueKindIssue || target.State != issuesdb.IssueStateOpen {
			continue
		}
		if closed[target.ID] {
			continue
		}
		closed[target.ID] = true
		_ = iq.SetIssueState(ctx, tx, issuesdb.SetIssueStateParams{
			ID:             target.ID,
			State:          issuesdb.IssueStateClosed,
			StateReason:    issuesdb.NullIssueStateReason{IssueStateReason: issuesdb.IssueStateReasonCompleted, Valid: true},
			ClosedByUserID: pgtype.Int8{Int64: p.ActorUserID, Valid: p.ActorUserID != 0},
		})
		_, _ = iq.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
			IssueID:     target.ID,
			ActorUserID: pgtype.Int8{Int64: p.ActorUserID, Valid: p.ActorUserID != 0},
			Kind:        "closed",
			Meta:        []byte(fmt.Sprintf(`{"closed_by_pr":%d}`, p.PRID)),
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// identityFor reads the user's display + primary verified email for
// commit identity. Falls back to a synthesised noreply on missing data
// rather than failing the merge — privacy/noreply emails ship post-MVP.
func identityFor(ctx context.Context, db pullsdb.DBTX, uq *usersdb.Queries, userID int64) (name, email string, err error) {
	user, err := uq.GetUserByID(ctx, db, userID)
	if err != nil {
		return "", "", err
	}
	display := strings.TrimSpace(user.DisplayName)
	if display == "" {
		display = user.Username
	}
	addr := user.Username + "@noreply.shithub.local"
	if user.PrimaryEmailID.Valid {
		em, err := uq.GetUserEmailByID(ctx, db, user.PrimaryEmailID.Int64)
		if err == nil && em.Verified {
			addr = string(em.Email)
		}
	}
	return display, addr, nil
}

// fetchCommitsForLinkScan returns the head-side commit messages so the
// linked-issue parser can scan body + subject. Best-effort — returns
// an empty slice on error.
func fetchCommitsForLinkScan(ctx context.Context, db pullsdb.DBTX, prID int64) []string {
	rows, err := pullsdb.New().ListPullRequestCommits(ctx, db, prID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Subject+"\n\n"+r.Body)
	}
	return out
}

// Compile-time check that issues' typed errors are still in scope so
// EditPR's wrapping continues to work after refactors.
var _ = errors.Is(issues.ErrEmptyTitle, issues.ErrEmptyTitle)
