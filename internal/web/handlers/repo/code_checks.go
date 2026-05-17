// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"fmt"
	"strings"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

type codeCommitCheckSummary struct {
	Show       bool
	Label      string
	StateClass string
	StateIcon  string
	Href       string
}

func (h *Handlers) codeCommitCheckSummary(ctx context.Context, owner, repoName string, repoID int64, headSHA string) codeCommitCheckSummary {
	return h.codeCommitCheckSummaries(ctx, owner, repoName, repoID, []string{headSHA})[headSHA]
}

func (h *Handlers) codeCommitCheckSummaries(ctx context.Context, owner, repoName string, repoID int64, headSHAs []string) map[string]codeCommitCheckSummary {
	heads := uniqueHeadSHAs(headSHAs)
	if len(heads) == 0 {
		return map[string]codeCommitCheckSummary{}
	}
	runs, err := h.cq.ListCheckRunsForCommits(ctx, h.d.Pool, checksdb.ListCheckRunsForCommitsParams{
		RepoID:   repoID,
		HeadShas: heads,
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "code: ListCheckRunsForCommits", "repo_id", repoID, "head_count", len(heads), "error", err)
		return map[string]codeCommitCheckSummary{}
	}
	byHead := make(map[string][]checksdb.CheckRun, len(heads))
	for _, run := range runs {
		byHead[run.HeadSha] = append(byHead[run.HeadSha], run)
	}
	out := make(map[string]codeCommitCheckSummary, len(byHead))
	for head, headRuns := range byHead {
		summary := summarizeCodeCommitChecks(headRuns)
		if !summary.Show {
			continue
		}
		summary.Href = codeCheckSummaryHref(owner, repoName, headRuns)
		out[head] = summary
	}
	return out
}

func summarizeCodeCommitChecks(runs []checksdb.CheckRun) codeCommitCheckSummary {
	if len(runs) == 0 {
		return codeCommitCheckSummary{}
	}
	total := len(runs)
	pending := 0
	failing := 0
	cancelled := 0
	successful := 0
	skipped := 0
	neutral := 0
	for _, run := range runs {
		if run.Status != checksdb.CheckStatusCompleted || !run.Conclusion.Valid {
			pending++
			continue
		}
		switch run.Conclusion.CheckConclusion {
		case checksdb.CheckConclusionSuccess:
			successful++
		case checksdb.CheckConclusionFailure, checksdb.CheckConclusionTimedOut, checksdb.CheckConclusionActionRequired:
			failing++
		case checksdb.CheckConclusionCancelled:
			cancelled++
		case checksdb.CheckConclusionSkipped:
			skipped++
		case checksdb.CheckConclusionNeutral, checksdb.CheckConclusionStale:
			neutral++
		default:
			neutral++
		}
	}
	switch {
	case failing > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(failing, total, "failed"),
			StateClass: "failure",
			StateIcon:  "x-circle-fill",
		}
	case pending > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(pending, total, "pending"),
			StateClass: "pending",
			StateIcon:  "dot-fill",
		}
	case cancelled > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(cancelled, total, "cancelled"),
			StateClass: "cancelled",
			StateIcon:  "stop",
		}
	case successful > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(successful, total, "successful"),
			StateClass: "success",
			StateIcon:  "check-circle-fill",
		}
	case skipped > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(skipped, total, "skipped"),
			StateClass: "skipped",
			StateIcon:  "dash",
		}
	case neutral > 0:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(neutral, total, "neutral"),
			StateClass: "neutral",
			StateIcon:  "dot-fill",
		}
	default:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(total, total, "neutral"),
			StateClass: "neutral",
			StateIcon:  "dot-fill",
		}
	}
}

func checkSummaryLabel(count, total int, state string) string {
	checkWord := "checks"
	if total == 1 {
		checkWord = "check"
	}
	if count == total {
		return fmt.Sprintf("%d %s %s", total, checkWord, state)
	}
	return fmt.Sprintf("%d of %d checks %s", count, total, state)
}

func codeCheckSummaryHref(owner, repoName string, runs []checksdb.CheckRun) string {
	for _, run := range runs {
		if href := sameRepoLocalDetailsHref(owner, repoName, run.DetailsUrl); href != "" {
			return href
		}
	}
	return fmt.Sprintf("/%s/%s/actions", owner, repoName)
}

func uniqueHeadSHAs(headSHAs []string) []string {
	seen := make(map[string]struct{}, len(headSHAs))
	out := make([]string, 0, len(headSHAs))
	for _, sha := range headSHAs {
		sha = strings.TrimSpace(sha)
		if sha == "" {
			continue
		}
		if _, ok := seen[sha]; ok {
			continue
		}
		seen[sha] = struct{}{}
		out = append(out, sha)
	}
	return out
}
