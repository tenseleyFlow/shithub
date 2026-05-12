// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"fmt"

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
	runs, err := h.cq.ListCheckRunsForCommit(ctx, h.d.Pool, checksdb.ListCheckRunsForCommitParams{
		RepoID:  repoID,
		HeadSha: headSHA,
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "code: ListCheckRunsForCommit", "repo_id", repoID, "head_sha", headSHA, "error", err)
		return codeCommitCheckSummary{}
	}
	summary := summarizeCodeCommitChecks(runs)
	if !summary.Show {
		return summary
	}
	summary.Href = fmt.Sprintf("/%s/%s/actions", owner, repoName)
	return summary
}

func summarizeCodeCommitChecks(runs []checksdb.CheckRun) codeCommitCheckSummary {
	if len(runs) == 0 {
		return codeCommitCheckSummary{}
	}
	total := len(runs)
	pending := 0
	failing := 0
	for _, run := range runs {
		if run.Status != checksdb.CheckStatusCompleted {
			pending++
			continue
		}
		if !run.Conclusion.Valid {
			pending++
			continue
		}
		switch run.Conclusion.CheckConclusion {
		case checksdb.CheckConclusionSuccess, checksdb.CheckConclusionSkipped, checksdb.CheckConclusionNeutral:
		default:
			failing++
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
	default:
		return codeCommitCheckSummary{
			Show:       true,
			Label:      checkSummaryLabel(total, total, "successful"),
			StateClass: "success",
			StateIcon:  "check-circle-fill",
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
