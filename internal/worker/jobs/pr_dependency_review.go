// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/checks"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/advisorymatch"
	"github.com/tenseleyFlow/shithub/internal/repos/dependencies"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const (
	DependencyReviewCheckName = "Dependency review"
	dependencyReviewAppSlug   = "shithub-security"
)

type PRDependencyReviewPayload struct {
	PRID int64 `json:"pr_id"`
}

// PRDependencyReview compares dependency snapshots for a PR base/head pair,
// persists the review, and publishes the GitHub-shaped "Dependency review"
// check run that branch protection can require.
func PRDependencyReview(deps PRJobsDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("pr dependency review: missing pool")
		}
		if deps.RepoFS == nil {
			return errors.New("pr dependency review: missing repo fs")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}

		var p PRDependencyReviewPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.PRID == 0 {
			return worker.PoisonError(errors.New("missing pr_id"))
		}

		pq := pullsdb.New()
		pr, err := pq.GetPullRequestByIssueID(ctx, deps.Pool, p.PRID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("pr %d not found", p.PRID))
			}
			return fmt.Errorf("load pr: %w", err)
		}
		if strings.TrimSpace(pr.HeadOid) == "" || strings.TrimSpace(pr.BaseOid) == "" {
			return worker.PoisonError(fmt.Errorf("pr %d missing base/head oid", p.PRID))
		}
		issue, err := issuesdb.New().GetIssueByID(ctx, deps.Pool, pr.IssueID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("pr issue %d not found", pr.IssueID))
			}
			return fmt.Errorf("load issue: %w", err)
		}
		repo, err := reposdb.New().GetRepoByID(ctx, deps.Pool, issue.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("repo %d not found", issue.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}
		if repo.DeletedAt.Valid {
			return nil
		}
		if !repo.OwnerOrgID.Valid {
			return nil
		}

		detailsURL, err := pullDetailsURL(ctx, deps.Pool, repo.ID, issue.Number)
		if err != nil {
			return err
		}
		externalID := dependencyReviewExternalID(pr.IssueID, pr.BaseOid, pr.HeadOid)

		decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: deps.Pool}, repo.OwnerOrgID.Int64, entitlements.FeatureDependencyReview)
		if err != nil {
			return fmt.Errorf("dependency review entitlement: %w", err)
		}
		if !decision.Allowed {
			if err := upsertDependencyReviewCheck(ctx, deps, dependencyReviewCheckInput{
				RepoID:     repo.ID,
				HeadSHA:    pr.HeadOid,
				DetailsURL: detailsURL,
				ExternalID: externalID,
				Conclusion: "action_required",
				Output: checks.Output{
					Title:   "Dependency review requires Team",
					Summary: "Upgrade this organization to Team to evaluate dependency changes before merge.",
					Text:    "shithub does not expose private dependency names or advisory details for Free organizations. Upgrade to Team to run dependency review on this pull request and make this check satisfy branch protection.",
				},
			}); err != nil {
				return err
			}
			enqueuePRMergeability(ctx, deps, p.PRID, "dependency_review_gate")
			return nil
		}

		gitDir, err := resolveGitDirForPR(ctx, deps.Pool, deps.RepoFS, p.PRID)
		if err != nil {
			return err
		}
		base, err := dependencies.Build(ctx, gitDir, dependencies.BuildOptions{Ref: pr.BaseOid})
		if err != nil {
			return fmt.Errorf("build base dependency snapshot: %w", err)
		}
		head, err := dependencies.Build(ctx, gitDir, dependencies.BuildOptions{Ref: pr.HeadOid})
		if err != nil {
			return fmt.Errorf("build head dependency snapshot: %w", err)
		}
		changes := dependencies.Diff(base, head)
		counts, err := persistPullDependencyReview(ctx, deps.Pool, pr, repo.ID, head, changes)
		if err != nil {
			return err
		}

		conclusion := dependencyReviewConclusion(counts.Vulnerable, len(changes))
		if err := upsertDependencyReviewCheck(ctx, deps, dependencyReviewCheckInput{
			RepoID:     repo.ID,
			HeadSHA:    pr.HeadOid,
			DetailsURL: detailsURL,
			ExternalID: externalID,
			Conclusion: conclusion,
			Output:     dependencyReviewOutput(conclusion, counts),
		}); err != nil {
			return err
		}
		enqueuePRMergeability(ctx, deps, p.PRID, "dependency_review_complete")

		logger.InfoContext(ctx, "pr dependency review complete",
			"pr_id", p.PRID, "repo_id", repo.ID, "head_sha", pr.HeadOid,
			"changes", len(changes), "vulnerable_changes", counts.Vulnerable)
		return nil
	}
}

type dependencyReviewCounts struct {
	Manifests  int
	Changes    int
	Added      int
	Removed    int
	Changed    int
	Vulnerable int
}

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

func persistPullDependencyReview(ctx context.Context, db txBeginner, pr pullsdb.PullRequest, repoID int64, head dependencies.Snapshot, changes []dependencies.Change) (dependencyReviewCounts, error) {
	counts := dependencyReviewCounts{
		Manifests: len(head.Manifests),
		Changes:   len(changes),
	}
	for _, change := range changes {
		switch change.Kind {
		case dependencies.ChangeAdded:
			counts.Added++
		case dependencies.ChangeRemoved:
			counts.Removed++
		case dependencies.ChangeChanged:
			counts.Changed++
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return dependencyReviewCounts{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := pullsdb.New()
	review, err := q.UpsertPullDependencyReview(ctx, tx, pullsdb.UpsertPullDependencyReviewParams{
		PrID:          pr.IssueID,
		RepoID:        repoID,
		BaseSha:       pr.BaseOid,
		HeadSha:       pr.HeadOid,
		Conclusion:    "neutral",
		ManifestCount: clampInt32(counts.Manifests),
		ChangeCount:   clampInt32(counts.Changes),
		AddedCount:    clampInt32(counts.Added),
		RemovedCount:  clampInt32(counts.Removed),
		ChangedCount:  clampInt32(counts.Changed),
	})
	if err != nil {
		return dependencyReviewCounts{}, fmt.Errorf("upsert dependency review: %w", err)
	}
	if err := q.DeletePullDependencyReviewItems(ctx, tx, review.ID); err != nil {
		return dependencyReviewCounts{}, fmt.Errorf("clear dependency review items: %w", err)
	}

	for _, change := range changes {
		advisories, err := matchingReviewAdvisories(ctx, q, tx, change)
		if err != nil {
			return dependencyReviewCounts{}, err
		}
		if len(advisories) > 0 {
			counts.Vulnerable++
		}
		if len(advisories) == 0 {
			if _, err := q.InsertPullDependencyReviewItem(ctx, tx, reviewItemParams(review.ID, change, pullsdb.DependencyAdvisory{})); err != nil {
				return dependencyReviewCounts{}, fmt.Errorf("insert dependency review item: %w", err)
			}
			continue
		}
		for _, advisory := range advisories {
			if _, err := q.InsertPullDependencyReviewItem(ctx, tx, reviewItemParams(review.ID, change, advisory)); err != nil {
				return dependencyReviewCounts{}, fmt.Errorf("insert dependency review item: %w", err)
			}
		}
	}

	conclusion := dependencyReviewConclusion(counts.Vulnerable, len(changes))
	if _, err := q.UpsertPullDependencyReview(ctx, tx, pullsdb.UpsertPullDependencyReviewParams{
		PrID:                  pr.IssueID,
		RepoID:                repoID,
		BaseSha:               pr.BaseOid,
		HeadSha:               pr.HeadOid,
		Conclusion:            conclusion,
		ManifestCount:         clampInt32(counts.Manifests),
		ChangeCount:           clampInt32(counts.Changes),
		AddedCount:            clampInt32(counts.Added),
		RemovedCount:          clampInt32(counts.Removed),
		ChangedCount:          clampInt32(counts.Changed),
		VulnerableChangeCount: clampInt32(counts.Vulnerable),
	}); err != nil {
		return dependencyReviewCounts{}, fmt.Errorf("finalize dependency review: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return dependencyReviewCounts{}, err
	}
	committed = true
	return counts, nil
}

type pullReviewQueries interface {
	ListDependencyReviewAdvisoryCandidates(ctx context.Context, db pullsdb.DBTX, arg pullsdb.ListDependencyReviewAdvisoryCandidatesParams) ([]pullsdb.ListDependencyReviewAdvisoryCandidatesRow, error)
}

func matchingReviewAdvisories(ctx context.Context, q pullReviewQueries, db pullsdb.DBTX, change dependencies.Change) ([]pullsdb.DependencyAdvisory, error) {
	if change.Kind == dependencies.ChangeRemoved {
		return nil, nil
	}
	version := change.NewVersion
	if strings.TrimSpace(version) == "" {
		return nil, nil
	}
	candidates, err := q.ListDependencyReviewAdvisoryCandidates(ctx, db, pullsdb.ListDependencyReviewAdvisoryCandidatesParams{
		Ecosystem:   change.Ecosystem,
		PackageName: change.PackageName,
	})
	if err != nil {
		return nil, fmt.Errorf("match dependency review advisories: %w", err)
	}
	advisories := make([]pullsdb.DependencyAdvisory, 0, len(candidates))
	for _, candidate := range candidates {
		if advisorymatch.MatchVersion(candidate.Ecosystem, version, candidate.AffectedRange) {
			advisories = append(advisories, dependencyReviewAdvisoryCandidate(candidate))
		}
	}
	return advisories, nil
}

func dependencyReviewAdvisoryCandidate(row pullsdb.ListDependencyReviewAdvisoryCandidatesRow) pullsdb.DependencyAdvisory {
	return pullsdb.DependencyAdvisory(row)
}

func reviewItemParams(reviewID int64, change dependencies.Change, advisory pullsdb.DependencyAdvisory) pullsdb.InsertPullDependencyReviewItemParams {
	params := pullsdb.InsertPullDependencyReviewItemParams{
		ReviewID:       reviewID,
		ChangeKind:     string(change.Kind),
		Ecosystem:      change.Ecosystem,
		PackageName:    change.PackageName,
		ManifestPath:   change.ManifestPath,
		LockfilePath:   change.LockfilePath,
		OldVersion:     change.OldVersion,
		NewVersion:     change.NewVersion,
		Scope:          change.Scope,
		Direct:         change.Direct,
		PackageManager: change.PackageManager,
		Source:         change.Source,
	}
	if advisory.ID != 0 {
		params.AdvisoryID = pgtype.Int8{Int64: advisory.ID, Valid: true}
		params.Severity = advisory.Severity
		params.AdvisorySource = advisory.Source
		params.AdvisoryExternalID = advisory.ExternalID
		params.AdvisorySummary = advisory.Summary
		params.PatchedVersions = advisory.PatchedVersions
		params.Recommendation = dependencyReviewRecommendation(advisory)
	}
	return params
}

func dependencyReviewRecommendation(advisory pullsdb.DependencyAdvisory) string {
	patched := strings.TrimSpace(advisory.PatchedVersions)
	if patched == "" {
		return "Review the advisory and update to a non-vulnerable version when one is available."
	}
	return "Update to " + patched + " or another non-vulnerable version."
}

func dependencyReviewConclusion(vulnerableChanges, changes int) string {
	if vulnerableChanges > 0 {
		return "failure"
	}
	if changes == 0 {
		return "neutral"
	}
	return "success"
}

func dependencyReviewOutput(conclusion string, counts dependencyReviewCounts) checks.Output {
	switch conclusion {
	case "failure":
		return checks.Output{
			Title:   "Vulnerable dependency changes detected",
			Summary: fmt.Sprintf("%d vulnerable dependency change(s) found across %d total dependency change(s).", counts.Vulnerable, counts.Changes),
			Text:    "Review the dependency review panel on this pull request before merging. shithub matched these results against the local advisory catalog for supported Go and npm manifests.",
		}
	case "neutral":
		return checks.Output{
			Title:   "No dependency changes detected",
			Summary: "No supported dependency manifests changed in this pull request.",
			Text:    "Dependency review currently covers supported Go and npm manifests only.",
		}
	default:
		return checks.Output{
			Title:   "Dependency review passed",
			Summary: fmt.Sprintf("%d dependency change(s) reviewed with no local advisory matches.", counts.Changes),
			Text:    "Dependency review currently covers supported Go and npm manifests only.",
		}
	}
}

type dependencyReviewCheckInput struct {
	RepoID     int64
	HeadSHA    string
	DetailsURL string
	ExternalID string
	Conclusion string
	Output     checks.Output
}

func upsertDependencyReviewCheck(ctx context.Context, deps PRJobsDeps, in dependencyReviewCheckInput) error {
	now := time.Now()
	cdeps := checks.Deps{Pool: deps.Pool, Logger: deps.Logger}
	run, err := checks.Create(ctx, cdeps, checks.CreateParams{
		RepoID:      in.RepoID,
		HeadSHA:     in.HeadSHA,
		AppSlug:     dependencyReviewAppSlug,
		Name:        DependencyReviewCheckName,
		Status:      "completed",
		Conclusion:  in.Conclusion,
		CompletedAt: now,
		DetailsURL:  in.DetailsURL,
		Output:      in.Output,
		ExternalID:  in.ExternalID,
	})
	if err != nil {
		return fmt.Errorf("create dependency review check: %w", err)
	}
	if _, err := checks.Update(ctx, cdeps, checks.UpdateParams{
		RunID:          run.ID,
		Status:         "completed",
		Conclusion:     in.Conclusion,
		CompletedAt:    now,
		DetailsURL:     in.DetailsURL,
		Output:         in.Output,
		HasStatus:      true,
		HasConclusion:  true,
		HasCompletedAt: true,
		HasDetailsURL:  true,
		HasOutput:      true,
	}); err != nil {
		return fmt.Errorf("update dependency review check: %w", err)
	}
	return nil
}

func dependencyReviewExternalID(prID int64, baseSHA, headSHA string) string {
	return fmt.Sprintf("dependency-review:%d:%s:%s", prID, baseSHA, headSHA)
}

func pullDetailsURL(ctx context.Context, db pullsdb.DBTX, repoID, number int64) (string, error) {
	row, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, db, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", worker.PoisonError(fmt.Errorf("repo %d owner not found", repoID))
		}
		return "", fmt.Errorf("load repo owner: %w", err)
	}
	ownerSlug, err := ownerSlugString(row.OwnerUsername)
	if err != nil {
		return "", worker.PoisonError(fmt.Errorf("repo %d owner slug: %w", repoID, err))
	}
	return "/" + ownerSlug + "/" + row.RepoName + "/pulls/" + strconv.FormatInt(number, 10), nil
}

func enqueuePRMergeability(ctx context.Context, deps PRJobsDeps, prID int64, reason string) {
	if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindPRMergeability,
		map[string]any{"pr_id": prID}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "pr dependency review: enqueue mergeability",
			"pr_id", prID, "reason", reason, "error", err)
	}
}
