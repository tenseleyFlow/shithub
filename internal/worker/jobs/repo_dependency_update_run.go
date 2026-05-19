// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	"github.com/tenseleyFlow/shithub/internal/repos/dependencies"
	"github.com/tenseleyFlow/shithub/internal/repos/dependencyupdates"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/webedit"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const dependencyUpdateResolverTimeout = 20 * time.Second

type RepoDependencyUpdateRunDeps struct {
	Pool            *pgxpool.Pool
	RepoFS          *storage.RepoFS
	Logger          *slog.Logger
	VersionResolver DependencyVersionResolver
	Now             func() time.Time
}

type DependencyVersionResolver interface {
	LatestVersion(ctx context.Context, ecosystem string, packageName string, currentVersion string) (string, error)
}

type DefaultDependencyVersionResolver struct {
	HTTPClient *http.Client
	GoCommand  string
	Timeout    time.Duration
}

type dependencyUpdateRunSummary struct {
	Status              string                                  `json:"status"`
	Message             string                                  `json:"message,omitempty"`
	BaseSHA             string                                  `json:"base_sha,omitempty"`
	HeadSHA             string                                  `json:"head_sha,omitempty"`
	CandidateCount      int                                     `json:"candidate_count,omitempty"`
	PullRequestCount    int                                     `json:"pull_request_count,omitempty"`
	SkippedCount        int                                     `json:"skipped_count,omitempty"`
	Skipped             []dependencyUpdateSkippedCandidate      `json:"skipped,omitempty"`
	PullRequests        []dependencyUpdateCreatedPullRequest    `json:"pull_requests,omitempty"`
	UnsupportedWarnings []dependencyUpdateUnsupportedResolution `json:"unsupported_warnings,omitempty"`
}

type dependencyUpdateCreatedPullRequest struct {
	IssueID    int64                       `json:"issue_id"`
	BranchName string                      `json:"branch_name"`
	UpdateKind string                      `json:"update_kind"`
	Packages   []dependencyUpdatePackageIO `json:"packages"`
}

type dependencyUpdatePackageIO struct {
	Ecosystem      string `json:"ecosystem"`
	PackageName    string `json:"package_name"`
	ManifestPath   string `json:"manifest_path"`
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	UpdateKind     string `json:"update_kind"`
	Group          string `json:"group,omitempty"`
	AdvisoryID     int64  `json:"advisory_id,omitempty"`
}

type dependencyUpdateSkippedCandidate struct {
	Ecosystem    string `json:"ecosystem,omitempty"`
	PackageName  string `json:"package_name,omitempty"`
	ManifestPath string `json:"manifest_path,omitempty"`
	Reason       string `json:"reason"`
}

type dependencyUpdateUnsupportedResolution struct {
	PackageName    string `json:"package_name"`
	CurrentVersion string `json:"current_version"`
	Error          string `json:"error"`
}

type dependencyUpdateCandidate struct {
	Ecosystem       string
	PackageName     string
	ManifestPath    string
	CurrentVersion  string
	TargetVersion   string
	Scope           string
	Direct          bool
	Source          string
	PackageManager  string
	UpdateKind      string
	UpdateType      string
	Group           string
	AdvisoryID      int64
	AdvisorySummary string
}

type dependencyUpdateBatch struct {
	Key        string
	BranchName string
	UpdateKind string
	Group      string
	Candidates []dependencyUpdateCandidate
}

type dependencyUpdateRules struct {
	Allow  []dependencyupdates.AllowRule
	Ignore []dependencyupdates.IgnoreRule
	Groups map[string]dependencyupdates.GroupRule
}

// RepoDependencyUpdateRun consumes queued dependency_update_jobs rows and
// creates bounded dependency update pull requests for supported Go/npm
// manifests.
func RepoDependencyUpdateRun(deps RepoDependencyUpdateRunDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("dependency update run: missing pool")
		}
		if deps.RepoFS == nil {
			return errors.New("dependency update run: missing repo fs")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		resolver := deps.VersionResolver
		if resolver == nil {
			resolver = DefaultDependencyVersionResolver{}
		}
		now := deps.Now
		if now == nil {
			now = time.Now
		}

		var p RepoDependencyUpdateRunPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.JobID == 0 {
			return worker.PoisonError(errors.New("missing job_id"))
		}

		rq := reposdb.New()
		job, err := rq.MarkQueuedDependencyUpdateJobRunning(ctx, deps.Pool, p.JobID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("claim dependency update job: %w", err)
		}
		if job.JobKind != "version_update" && job.JobKind != "security_update" {
			msg := "unsupported dependency update job kind " + job.JobKind
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "failed", "", "", dependencyUpdateRunSummary{
				Status:  "failed",
				Message: msg,
			}, msg)
		}
		if !job.ConfigID.Valid {
			msg := "dependency update job has no config_id"
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "failed", "", "", dependencyUpdateRunSummary{
				Status:  "failed",
				Message: msg,
			}, msg)
		}

		cfg, err := rq.GetDependencyUpdateConfig(ctx, deps.Pool, job.ConfigID.Int64)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				msg := "dependency update config is missing"
				return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "failed", "", "", dependencyUpdateRunSummary{
					Status:  "failed",
					Message: msg,
				}, msg)
			}
			return fmt.Errorf("load dependency update config: %w", err)
		}
		if !cfg.Enabled {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", "", "", dependencyUpdateRunSummary{
				Status:  "skipped",
				Message: "dependency update config is disabled",
			}, "")
		}

		repo, err := rq.GetRepoByID(ctx, deps.Pool, job.RepoID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return worker.PoisonError(fmt.Errorf("repo %d not found", job.RepoID))
			}
			return fmt.Errorf("load repo: %w", err)
		}
		if repo.DeletedAt.Valid || repo.IsArchived || repo.IsPaused {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", "", "", dependencyUpdateRunSummary{
				Status:  "skipped",
				Message: "repository is not eligible for dependency updates",
			}, "")
		}
		if !repo.OwnerOrgID.Valid {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", "", "", dependencyUpdateRunSummary{
				Status:  "skipped",
				Message: "repository is not org-owned",
			}, "")
		}

		feature := entitlements.FeatureDependabotVersionUpdates
		if job.JobKind == "security_update" {
			feature = entitlements.FeatureDependabotSecurityUpdates
		}
		decision, err := entitlements.CheckPrincipalFeature(ctx,
			entitlements.Deps{Pool: deps.Pool},
			billing.PrincipalForOrg(repo.OwnerOrgID.Int64),
			feature)
		if err != nil {
			return fmt.Errorf("dependency update entitlement: %w", err)
		}
		if !decision.Allowed {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", "", "", dependencyUpdateRunSummary{
				Status:  "skipped",
				Message: "Team dependency update entitlement denied",
			}, "")
		}

		owner, err := rq.GetRepoOwnerUsernameByID(ctx, deps.Pool, repo.ID)
		if err != nil {
			return fmt.Errorf("load repo owner: %w", err)
		}
		ownerSlug, err := ownerSlugString(owner.OwnerUsername)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo owner slug: %w", err))
		}
		gitDir, err := deps.RepoFS.RepoPath(ownerSlug, repo.Name)
		if err != nil {
			return worker.PoisonError(fmt.Errorf("repo path: %w", err))
		}
		actorID, err := dependencyUpdateActorID(ctx, deps.Pool, repo)
		if err != nil {
			return fmt.Errorf("dependency update actor: %w", err)
		}

		baseRef := strings.TrimSpace(cfg.TargetBranch)
		if baseRef == "" {
			baseRef = repo.DefaultBranch
		}
		baseSHA, err := repogit.ResolveRefOID(ctx, gitDir, baseRef)
		if err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", "", "", dependencyUpdateRunSummary{
					Status:  "skipped",
					Message: "target branch is missing",
				}, "")
			}
			return fmt.Errorf("resolve target branch: %w", err)
		}

		snapshot, err := dependencies.Build(ctx, gitDir, dependencies.BuildOptions{Ref: baseRef})
		if err != nil {
			return fmt.Errorf("build dependency inventory: %w", err)
		}
		alerts, err := rq.ListOpenDependencyAlertsForRepo(ctx, deps.Pool, repo.ID)
		if err != nil {
			return fmt.Errorf("list dependency alerts: %w", err)
		}

		rules, err := dependencyUpdateRulesFromConfig(cfg)
		if err != nil {
			msg := err.Error()
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "failed", baseSHA, "", dependencyUpdateRunSummary{
				Status:  "failed",
				BaseSHA: baseSHA,
				Message: msg,
			}, msg)
		}
		plan, err := planDependencyUpdateRun(ctx, cfg, job.JobKind, snapshot, alerts, resolver, rules)
		if err != nil {
			return fmt.Errorf("plan dependency updates: %w", err)
		}
		if len(plan.Candidates) == 0 {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", baseSHA, baseSHA, dependencyUpdateRunSummary{
				Status:              "skipped",
				Message:             "no supported dependency updates found",
				BaseSHA:             baseSHA,
				HeadSHA:             baseSHA,
				SkippedCount:        len(plan.Skipped),
				Skipped:             plan.Skipped,
				UnsupportedWarnings: plan.Warnings,
			}, "")
		}

		openPRs, err := rq.ListDependencyUpdatePRsForRepo(ctx, deps.Pool, repo.ID)
		if err != nil {
			return fmt.Errorf("list dependency update prs: %w", err)
		}
		openCount := 0
		openBranches := map[string]struct{}{}
		for _, pr := range openPRs {
			if pr.Status == "open" {
				openCount++
				openBranches[pr.BranchName] = struct{}{}
			}
		}
		limit := int(cfg.OpenPullRequestLimit)
		if limit < 0 {
			limit = 0
		}
		remaining := limit - openCount
		if remaining <= 0 {
			return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "completed", baseSHA, baseSHA, dependencyUpdateRunSummary{
				Status:         "skipped",
				Message:        "open dependency update pull request limit reached",
				BaseSHA:        baseSHA,
				HeadSHA:        baseSHA,
				CandidateCount: len(plan.Candidates),
			}, "")
		}

		batches := dependencyUpdateBatches(plan.Candidates, limit)
		created := make([]dependencyUpdateCreatedPullRequest, 0, len(batches))
		skipped := append([]dependencyUpdateSkippedCandidate{}, plan.Skipped...)
		headSHA := baseSHA
		for _, batch := range batches {
			if len(created) >= remaining {
				skipped = append(skipped, dependencyUpdateSkippedCandidate{Reason: "open pull request limit reached"})
				break
			}
			if _, ok := openBranches[batch.BranchName]; ok {
				skipped = append(skipped, dependencyUpdateSkippedCandidate{
					Reason:       "dependency update branch already has an open PR",
					PackageName:  batch.Candidates[0].PackageName,
					Ecosystem:    batch.Candidates[0].Ecosystem,
					ManifestPath: batch.Candidates[0].ManifestPath,
				})
				continue
			}
			if _, err := repogit.ResolveRefOID(ctx, gitDir, "refs/heads/"+batch.BranchName); err == nil {
				skipped = append(skipped, dependencyUpdateSkippedCandidate{
					Reason:       "dependency update branch already exists",
					PackageName:  batch.Candidates[0].PackageName,
					Ecosystem:    batch.Candidates[0].Ecosystem,
					ManifestPath: batch.Candidates[0].ManifestPath,
				})
				continue
			} else if !errors.Is(err, repogit.ErrRefNotFound) {
				return fmt.Errorf("check dependency update branch: %w", err)
			}

			pr, branchHead, err := createDependencyUpdatePR(ctx, deps, repo, gitDir, baseRef, baseSHA, actorID, batch, now().UTC())
			if err != nil {
				msg := err.Error()
				return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, "failed", baseSHA, headSHA, dependencyUpdateRunSummary{
					Status:         "failed",
					Message:        msg,
					BaseSHA:        baseSHA,
					HeadSHA:        headSHA,
					CandidateCount: len(plan.Candidates),
					PullRequests:   created,
					Skipped:        skipped,
				}, msg)
			}
			packages := dependencyUpdatePackageSet(batch.Candidates)
			body, _ := json.Marshal(packages)
			if _, err := rq.UpsertDependencyUpdatePR(ctx, deps.Pool, reposdb.UpsertDependencyUpdatePRParams{
				RepoID:        repo.ID,
				BranchName:    batch.BranchName,
				UpdateKind:    batch.UpdateKind,
				Status:        "open",
				JobID:         pgtype.Int8{Int64: job.ID, Valid: true},
				PullRequestID: pgtype.Int8{Int64: pr.PullRequest.IssueID, Valid: true},
				PackageSet:    body,
			}); err != nil {
				return fmt.Errorf("record dependency update pr: %w", err)
			}
			openBranches[batch.BranchName] = struct{}{}
			headSHA = branchHead
			created = append(created, dependencyUpdateCreatedPullRequest{
				IssueID:    pr.PullRequest.IssueID,
				BranchName: batch.BranchName,
				UpdateKind: batch.UpdateKind,
				Packages:   packages,
			})
		}

		status := "completed"
		msg := "dependency update run completed"
		if len(created) == 0 {
			msg = "no dependency update pull requests created"
		}
		logger.InfoContext(ctx, "dependency update run complete",
			"job_id", job.ID, "repo_id", repo.ID, "candidates", len(plan.Candidates), "prs", len(created))
		return completeDependencyUpdateRun(ctx, rq, deps.Pool, job.ID, status, baseSHA, headSHA, dependencyUpdateRunSummary{
			Status:              status,
			Message:             msg,
			BaseSHA:             baseSHA,
			HeadSHA:             headSHA,
			CandidateCount:      len(plan.Candidates),
			PullRequestCount:    len(created),
			SkippedCount:        len(skipped),
			Skipped:             skipped,
			PullRequests:        created,
			UnsupportedWarnings: plan.Warnings,
		}, "")
	}
}

func dependencyUpdateActorID(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo) (int64, error) {
	if repo.OwnerUserID.Valid {
		return repo.OwnerUserID.Int64, nil
	}
	if repo.OwnerOrgID.Valid {
		org, err := orgsdb.New().GetOrgByID(ctx, pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return 0, err
		}
		if org.CreatedByUserID.Valid {
			return org.CreatedByUserID.Int64, nil
		}
	}
	return 0, errors.New("no dependency update actor available")
}

func completeDependencyUpdateRun(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, jobID int64, status string, base, head string, summary dependencyUpdateRunSummary, lastErr string) error {
	body, err := json.Marshal(summary)
	if err != nil {
		body = []byte(`{"status":"failed","message":"could not marshal summary"}`)
		status = "failed"
		lastErr = "could not marshal summary"
	}
	_, err = q.CompleteDependencyUpdateJob(ctx, db, reposdb.CompleteDependencyUpdateJobParams{
		Status:        status,
		BaseSha:       base,
		HeadSha:       head,
		ResultSummary: body,
		LastError:     trimDependencyUpdateError(lastErr),
		ID:            jobID,
	})
	return err
}

func trimDependencyUpdateError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= 2000 {
		return msg
	}
	return msg[:2000]
}

type dependencyUpdatePlan struct {
	Candidates []dependencyUpdateCandidate
	Skipped    []dependencyUpdateSkippedCandidate
	Warnings   []dependencyUpdateUnsupportedResolution
}

func planDependencyUpdateRun(ctx context.Context, cfg reposdb.DependencyUpdateConfig, jobKind string, snapshot dependencies.Snapshot, alerts []reposdb.ListOpenDependencyAlertsForRepoRow, resolver DependencyVersionResolver, rules dependencyUpdateRules) (dependencyUpdatePlan, error) {
	var plan dependencyUpdatePlan
	if jobKind == "security_update" {
		security, skipped := securityUpdateCandidates(cfg, alerts, rules)
		plan.Candidates = append(plan.Candidates, security...)
		plan.Skipped = append(plan.Skipped, skipped...)
		return plan, nil
	}

	deps := directDependenciesForConfig(cfg, snapshot)
	for _, dep := range deps {
		latest, err := resolver.LatestVersion(ctx, dep.Ecosystem, dep.PackageName, dep.PackageVersion)
		if err != nil {
			plan.Warnings = append(plan.Warnings, dependencyUpdateUnsupportedResolution{
				PackageName:    dep.PackageName,
				CurrentVersion: dep.PackageVersion,
				Error:          err.Error(),
			})
			continue
		}
		latest = strings.TrimSpace(latest)
		if latest == "" || normalizedVersionEqual(dep.PackageVersion, latest) {
			continue
		}
		target := dependencyTargetSpec(dep.PackageVersion, latest)
		c := dependencyUpdateCandidate{
			Ecosystem:      dep.Ecosystem,
			PackageName:    dep.PackageName,
			ManifestPath:   dep.ManifestPath,
			CurrentVersion: dep.PackageVersion,
			TargetVersion:  target,
			Scope:          dep.Scope,
			Direct:         dep.Direct,
			Source:         dep.Source,
			PackageManager: dep.PackageManager,
			UpdateKind:     "version",
			UpdateType:     semverUpdateType(dep.PackageVersion, latest),
		}
		if !candidateAllowed(c, rules) {
			continue
		}
		if ignoredCandidate(c, rules) {
			continue
		}
		c.Group = candidateGroup(c, rules)
		plan.Candidates = append(plan.Candidates, c)
	}
	sortDependencyUpdateCandidates(plan.Candidates)
	return plan, nil
}

func directDependenciesForConfig(cfg reposdb.DependencyUpdateConfig, snapshot dependencies.Snapshot) []dependencies.Dependency {
	out := make([]dependencies.Dependency, 0, len(snapshot.Dependencies))
	for _, dep := range snapshot.Dependencies {
		if dep.Ecosystem != cfg.Ecosystem || !manifestInDependencyUpdateDirectory(dep.ManifestPath, cfg.Directory) {
			continue
		}
		if dep.Source != "go.mod" && dep.Source != "package.json" {
			continue
		}
		if !dep.Direct {
			continue
		}
		out = append(out, dep)
	}
	return out
}

func securityUpdateCandidates(cfg reposdb.DependencyUpdateConfig, alerts []reposdb.ListOpenDependencyAlertsForRepoRow, rules dependencyUpdateRules) ([]dependencyUpdateCandidate, []dependencyUpdateSkippedCandidate) {
	var out []dependencyUpdateCandidate
	var skipped []dependencyUpdateSkippedCandidate
	for _, alert := range alerts {
		if alert.Ecosystem != cfg.Ecosystem || !manifestInDependencyUpdateDirectory(alert.ManifestPath, cfg.Directory) {
			continue
		}
		if path.Base(alert.ManifestPath) != "go.mod" && path.Base(alert.ManifestPath) != "package.json" {
			skipped = append(skipped, dependencyUpdateSkippedCandidate{
				Ecosystem:    alert.Ecosystem,
				PackageName:  alert.PackageName,
				ManifestPath: alert.ManifestPath,
				Reason:       "transitive or lockfile-only security update requires a package-manager adapter",
			})
			continue
		}
		target := dependencyTargetSpec(alert.PackageVersion, firstPatchedVersion(alert.PatchedVersions))
		if strings.TrimSpace(target) == "" {
			skipped = append(skipped, dependencyUpdateSkippedCandidate{
				Ecosystem:    alert.Ecosystem,
				PackageName:  alert.PackageName,
				ManifestPath: alert.ManifestPath,
				Reason:       "advisory does not provide an exact patched version",
			})
			continue
		}
		c := dependencyUpdateCandidate{
			Ecosystem:       alert.Ecosystem,
			PackageName:     alert.PackageName,
			ManifestPath:    alert.ManifestPath,
			CurrentVersion:  alert.PackageVersion,
			TargetVersion:   target,
			Source:          path.Base(alert.ManifestPath),
			PackageManager:  cfg.PackageManager,
			UpdateKind:      "security",
			UpdateType:      "security",
			AdvisoryID:      alert.ID,
			AdvisorySummary: alert.Summary,
		}
		if !candidateAllowed(c, rules) || ignoredCandidate(c, rules) {
			continue
		}
		c.Group = candidateGroup(c, rules)
		out = append(out, c)
	}
	sortDependencyUpdateCandidates(out)
	return out, skipped
}

func dependencyUpdateRulesFromConfig(cfg reposdb.DependencyUpdateConfig) (dependencyUpdateRules, error) {
	var rules dependencyUpdateRules
	if len(cfg.AllowRules) > 0 {
		if err := json.Unmarshal(cfg.AllowRules, &rules.Allow); err != nil {
			return rules, fmt.Errorf("decode allow rules: %w", err)
		}
	}
	if len(cfg.IgnoreRules) > 0 {
		if err := json.Unmarshal(cfg.IgnoreRules, &rules.Ignore); err != nil {
			return rules, fmt.Errorf("decode ignore rules: %w", err)
		}
	}
	if len(cfg.Groups) > 0 {
		if err := json.Unmarshal(cfg.Groups, &rules.Groups); err != nil {
			return rules, fmt.Errorf("decode group rules: %w", err)
		}
	}
	if rules.Groups == nil {
		rules.Groups = map[string]dependencyupdates.GroupRule{}
	}
	return rules, nil
}

func candidateAllowed(c dependencyUpdateCandidate, rules dependencyUpdateRules) bool {
	if len(rules.Allow) == 0 {
		return true
	}
	for _, rule := range rules.Allow {
		if matchDependencyRule(rule.DependencyName, rule.DependencyType, rule.UpdateTypes, c) {
			return true
		}
	}
	return false
}

func ignoredCandidate(c dependencyUpdateCandidate, rules dependencyUpdateRules) bool {
	for _, rule := range rules.Ignore {
		if matchDependencyRule(rule.DependencyName, "", rule.UpdateTypes, c) {
			if len(rule.Versions) == 0 || matchAnyPattern(c.TargetVersion, rule.Versions) {
				return true
			}
		}
	}
	return false
}

func matchDependencyRule(namePattern, depType string, updateTypes []string, c dependencyUpdateCandidate) bool {
	if strings.TrimSpace(namePattern) != "" && !globMatch(namePattern, c.PackageName) {
		return false
	}
	if strings.TrimSpace(depType) != "" && !dependencyTypeMatches(depType, c) {
		return false
	}
	if len(updateTypes) > 0 && !updateTypeMatches(updateTypes, c.UpdateType) {
		return false
	}
	return true
}

func dependencyTypeMatches(depType string, c dependencyUpdateCandidate) bool {
	switch strings.ToLower(strings.TrimSpace(depType)) {
	case "", "all":
		return true
	case "direct":
		return c.Direct
	case "indirect", "transitive":
		return !c.Direct
	case "production", "runtime":
		return c.Scope == "runtime"
	case "development":
		return c.Scope == "development"
	default:
		return false
	}
}

func updateTypeMatches(values []string, got string) bool {
	got = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(got)), "version-update:")
	for _, v := range values {
		v = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "version-update:")
		if v == got || v == "security" && got == "security" {
			return true
		}
	}
	return false
}

func candidateGroup(c dependencyUpdateCandidate, rules dependencyUpdateRules) string {
	names := make([]string, 0, len(rules.Groups))
	for name := range rules.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		group := rules.Groups[name]
		if group.AppliesTo == "security-updates" && c.UpdateKind != "security" {
			continue
		}
		if group.AppliesTo == "version-updates" && c.UpdateKind != "version" {
			continue
		}
		if strings.TrimSpace(group.DependencyType) != "" && !dependencyTypeMatches(group.DependencyType, c) {
			continue
		}
		if len(group.UpdateTypes) > 0 && !updateTypeMatches(group.UpdateTypes, c.UpdateType) {
			continue
		}
		if len(group.ExcludePatterns) > 0 && matchAnyPattern(c.PackageName, group.ExcludePatterns) {
			continue
		}
		if len(group.Patterns) == 0 || matchAnyPattern(c.PackageName, group.Patterns) {
			return name
		}
	}
	return ""
}

func dependencyUpdateBatches(candidates []dependencyUpdateCandidate, limit int) []dependencyUpdateBatch {
	grouped := map[string][]dependencyUpdateCandidate{}
	var single []dependencyUpdateCandidate
	for _, c := range candidates {
		if c.Group != "" {
			grouped[c.Group] = append(grouped[c.Group], c)
			continue
		}
		single = append(single, c)
	}
	var out []dependencyUpdateBatch
	groupNames := make([]string, 0, len(grouped))
	for name := range grouped {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		items := grouped[name]
		sortDependencyUpdateCandidates(items)
		out = append(out, dependencyUpdateBatch{
			Key:        name,
			BranchName: dependencyUpdateBranchName(items[0].Ecosystem, name, items),
			UpdateKind: "grouped",
			Group:      name,
			Candidates: items,
		})
	}
	for _, c := range single {
		out = append(out, dependencyUpdateBatch{
			Key:        c.PackageName,
			BranchName: dependencyUpdateBranchName(c.Ecosystem, c.PackageName, []dependencyUpdateCandidate{c}),
			UpdateKind: c.UpdateKind,
			Candidates: []dependencyUpdateCandidate{c},
		})
	}
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func createDependencyUpdatePR(ctx context.Context, deps RepoDependencyUpdateRunDeps, repo reposdb.Repo, gitDir, baseRef, baseSHA string, actorID int64, batch dependencyUpdateBatch, now time.Time) (pulls.CreateResult, string, error) {
	if err := createDependencyUpdateBranch(ctx, gitDir, batch.BranchName, baseSHA); err != nil {
		return pulls.CreateResult{}, "", err
	}
	changed := 0
	for _, candidate := range batch.Candidates {
		didChange, err := applyDependencyUpdateCandidate(ctx, deps, repo, gitDir, batch.BranchName, actorID, candidate, now)
		if err != nil {
			_ = repogit.DeleteBranch(ctx, gitDir, batch.BranchName, "")
			return pulls.CreateResult{}, "", err
		}
		if didChange {
			changed++
		}
	}
	if changed == 0 {
		_ = repogit.DeleteBranch(ctx, gitDir, batch.BranchName, "")
		return pulls.CreateResult{}, "", errors.New("dependency update produced no file changes")
	}
	headSHA, err := repogit.ResolveRefOID(ctx, gitDir, "refs/heads/"+batch.BranchName)
	if err != nil {
		return pulls.CreateResult{}, "", fmt.Errorf("resolve dependency update branch: %w", err)
	}
	pr, err := pulls.Create(ctx, pulls.Deps{Pool: deps.Pool, Logger: deps.Logger}, pulls.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: actorID,
		Title:        dependencyUpdatePRTitle(batch),
		Body:         dependencyUpdatePRBody(batch),
		BaseRef:      baseRef,
		HeadRef:      batch.BranchName,
		GitDir:       gitDir,
	})
	if err != nil {
		return pulls.CreateResult{}, headSHA, fmt.Errorf("create dependency update pull request: %w", err)
	}
	return pr, headSHA, nil
}

func createDependencyUpdateBranch(ctx context.Context, gitDir, branchName, baseSHA string) error {
	check := exec.CommandContext(ctx, "git", "-C", gitDir, "check-ref-format", "--branch", branchName)
	if out, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("check dependency update branch name: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	ref := "refs/heads/" + branchName
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "update-ref", ref, baseSHA)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create dependency update branch: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func applyDependencyUpdateCandidate(ctx context.Context, deps RepoDependencyUpdateRunDeps, repo reposdb.Repo, gitDir, branch string, actorID int64, c dependencyUpdateCandidate, now time.Time) (bool, error) {
	current, err := repogit.ReadBlobBytes(ctx, gitDir, branch, c.ManifestPath, webedit.MaxTextBytes)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", c.ManifestPath, err)
	}
	next, err := updateDependencyManifest(current, c)
	if err != nil {
		return false, err
	}
	if string(next) == string(current) {
		return false, nil
	}
	baseOID, err := repogit.ResolveRefOID(ctx, gitDir, "refs/heads/"+branch)
	if err != nil {
		return false, fmt.Errorf("resolve branch before edit: %w", err)
	}
	message := fmt.Sprintf("Bump %s from %s to %s", c.PackageName, c.CurrentVersion, c.TargetVersion)
	if c.UpdateKind == "security" {
		message = fmt.Sprintf("Bump vulnerable %s from %s to %s", c.PackageName, c.CurrentVersion, c.TargetVersion)
	}
	_, err = webedit.Commit(ctx, webedit.Deps{Pool: deps.Pool, Logger: deps.Logger, Now: func() time.Time { return now }}, webedit.Params{
		GitDir:      gitDir,
		Repo:        repo,
		Branch:      branch,
		BaseOID:     baseOID,
		ActorUserID: actorID,
		Op:          webedit.OpEdit,
		SourcePath:  c.ManifestPath,
		TargetPath:  c.ManifestPath,
		Content:     next,
		Message:     message,
		Description: "Automated dependency update.",
	})
	if err != nil {
		return false, fmt.Errorf("commit dependency update for %s: %w", c.PackageName, err)
	}
	return true, nil
}

func updateDependencyManifest(body []byte, c dependencyUpdateCandidate) ([]byte, error) {
	switch path.Base(c.ManifestPath) {
	case "go.mod":
		return updateGoModDependency(body, c)
	case "package.json":
		return updatePackageJSONDependency(body, c)
	default:
		return nil, fmt.Errorf("unsupported dependency update manifest %q", c.ManifestPath)
	}
}

func updateGoModDependency(body []byte, c dependencyUpdateCandidate) ([]byte, error) {
	lines := strings.SplitAfter(string(body), "\n")
	changed := false
	for i, line := range lines {
		if !strings.Contains(line, c.PackageName) || !strings.Contains(line, c.CurrentVersion) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "require" && len(fields) >= 3 && fields[1] == c.PackageName {
			lines[i] = strings.Replace(line, c.CurrentVersion, c.TargetVersion, 1)
			changed = true
			continue
		}
		if fields[0] == c.PackageName {
			lines[i] = strings.Replace(line, c.CurrentVersion, c.TargetVersion, 1)
			changed = true
		}
	}
	if !changed {
		return nil, fmt.Errorf("could not update %s in %s", c.PackageName, c.ManifestPath)
	}
	return []byte(strings.Join(lines, "")), nil
}

func updatePackageJSONDependency(body []byte, c dependencyUpdateCandidate) ([]byte, error) {
	name := regexp.QuoteMeta(`"` + c.PackageName + `"`)
	re := regexp.MustCompile(`(?m)(` + name + `\s*:\s*")([^"]*)(")`)
	matches := re.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("could not find %s in %s", c.PackageName, c.ManifestPath)
	}
	var out []byte
	changed := false
	last := 0
	for _, m := range matches {
		old := string(body[m[4]:m[5]])
		if old != c.CurrentVersion {
			continue
		}
		out = append(out, body[last:m[4]]...)
		out = append(out, c.TargetVersion...)
		last = m[5]
		changed = true
		break
	}
	if !changed {
		return nil, fmt.Errorf("could not update %s in %s", c.PackageName, c.ManifestPath)
	}
	out = append(out, body[last:]...)
	return out, nil
}

func dependencyUpdatePRTitle(batch dependencyUpdateBatch) string {
	if batch.Group != "" {
		return fmt.Sprintf("Bump %s dependency group", batch.Group)
	}
	c := batch.Candidates[0]
	return fmt.Sprintf("Bump %s from %s to %s", c.PackageName, c.CurrentVersion, c.TargetVersion)
}

func dependencyUpdatePRBody(batch dependencyUpdateBatch) string {
	var b strings.Builder
	switch {
	case batch.Group != "":
		fmt.Fprintf(&b, "Updates the `%s` dependency group.\n\n", batch.Group)
	case batch.Candidates[0].UpdateKind == "security":
		b.WriteString("Updates a vulnerable dependency to a patched version.\n\n")
	default:
		b.WriteString("Updates a dependency according to the repository dependency update configuration.\n\n")
	}
	b.WriteString("| Package | From | To | Manifest |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, c := range batch.Candidates {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | `%s` |\n", c.PackageName, c.CurrentVersion, c.TargetVersion, c.ManifestPath)
	}
	b.WriteString("\nGenerated by shithub dependency updates.")
	return b.String()
}

func dependencyUpdatePackageSet(candidates []dependencyUpdateCandidate) []dependencyUpdatePackageIO {
	out := make([]dependencyUpdatePackageIO, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, dependencyUpdatePackageIO{
			Ecosystem:      c.Ecosystem,
			PackageName:    c.PackageName,
			ManifestPath:   c.ManifestPath,
			CurrentVersion: c.CurrentVersion,
			TargetVersion:  c.TargetVersion,
			UpdateKind:     c.UpdateKind,
			Group:          c.Group,
			AdvisoryID:     c.AdvisoryID,
		})
	}
	return out
}

func manifestInDependencyUpdateDirectory(manifestPath, directory string) bool {
	dir := strings.TrimSpace(directory)
	if dir == "" || dir == "/" {
		return true
	}
	dir = strings.TrimPrefix(path.Clean(dir), "/")
	if dir == "." {
		return true
	}
	p := path.Clean(manifestPath)
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func firstPatchedVersion(value string) string {
	re := regexp.MustCompile(`v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)
	return re.FindString(value)
}

func dependencyTargetSpec(current, latest string) string {
	latest = strings.TrimSpace(latest)
	if latest == "" {
		return ""
	}
	prefix := ""
	for _, p := range []string{"^", "~", ">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(strings.TrimSpace(current), p) {
			prefix = p
			break
		}
	}
	return prefix + latest
}

func normalizedVersionEqual(a, b string) bool {
	return strings.TrimPrefix(strings.TrimLeft(strings.TrimSpace(a), "^~<>= "), "v") ==
		strings.TrimPrefix(strings.TrimLeft(strings.TrimSpace(b), "^~<>= "), "v")
}

func semverUpdateType(current, latest string) string {
	c := semverParts(current)
	l := semverParts(latest)
	if c == nil || l == nil {
		return "unknown"
	}
	switch {
	case l[0] != c[0]:
		return "semver-major"
	case l[1] != c[1]:
		return "semver-minor"
	case l[2] != c[2]:
		return "semver-patch"
	default:
		return "unknown"
	}
}

func semverParts(value string) []int {
	value = strings.TrimLeft(strings.TrimSpace(value), "v^~<>= ")
	re := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)
	m := re.FindStringSubmatch(value)
	if len(m) != 4 {
		return nil
	}
	out := make([]int, 3)
	for i := 1; i <= 3; i++ {
		var n int
		for _, ch := range m[i] {
			n = n*10 + int(ch-'0')
		}
		out[i-1] = n
	}
	return out
}

func sortDependencyUpdateCandidates(candidates []dependencyUpdateCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if !strings.EqualFold(a.PackageName, b.PackageName) {
			return strings.ToLower(a.PackageName) < strings.ToLower(b.PackageName)
		}
		return a.ManifestPath < b.ManifestPath
	})
}

func dependencyUpdateBranchName(ecosystem, key string, candidates []dependencyUpdateCandidate) string {
	hashBase := ecosystem + "\x00" + key
	for _, c := range candidates {
		hashBase += "\x00" + c.PackageName + "\x00" + c.ManifestPath + "\x00" + c.TargetVersion
	}
	sum := dependencyUpdateShortHash(hashBase)
	slug := sanitizeDependencyUpdateBranchPart(key)
	if slug == "" {
		slug = "updates"
	}
	return "shithub/dependency-updates/" + sanitizeDependencyUpdateBranchPart(ecosystem) + "/" + slug + "-" + sum
}

func dependencyUpdateShortHash(value string) string {
	const alphabet = "0123456789abcdef"
	var h uint64 = 1469598103934665603
	for i := 0; i < len(value); i++ {
		h ^= uint64(value[i])
		h *= 1099511628211
	}
	var b [10]byte
	for i := range b {
		shift := uint((9 - i) * 4)
		b[i] = alphabet[(h>>shift)&0xf]
	}
	return string(b[:])
}

var dependencyUpdateBranchPartRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeDependencyUpdateBranchPart(value string) string {
	value = strings.Trim(strings.ToLower(value), " \t\r\n")
	value = strings.Trim(value, "@")
	value = dependencyUpdateBranchPartRe.ReplaceAllString(value, "-")
	value = strings.Trim(value, ".-_/")
	if len(value) > 80 {
		value = strings.Trim(value[:80], ".-_/")
	}
	return value
}

func matchAnyPattern(value string, patterns []string) bool {
	for _, p := range patterns {
		if globMatch(p, value) {
			return true
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	ok, err := regexp.MatchString(b.String(), value)
	return err == nil && ok
}

func (r DefaultDependencyVersionResolver) LatestVersion(ctx context.Context, ecosystem string, packageName string, currentVersion string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = dependencyUpdateResolverTimeout
	}
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch ecosystem {
	case "go":
		return r.latestGoVersion(child, packageName)
	case "npm":
		return r.latestNPMVersion(child, packageName)
	default:
		return "", fmt.Errorf("unsupported ecosystem %q", ecosystem)
	}
}

func (r DefaultDependencyVersionResolver) latestGoVersion(ctx context.Context, packageName string) (string, error) {
	goCmd := strings.TrimSpace(r.GoCommand)
	if goCmd == "" {
		goCmd = "go"
	}
	cmd := exec.CommandContext(ctx, goCmd, "list", "-m", "-json", "-versions", packageName)
	cmd.Env = append(os.Environ(), "GONOSUMDB=*", "GONOPROXY=")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go version resolver: %w", err)
	}
	var doc struct {
		Versions []string `json:"Versions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", fmt.Errorf("go version resolver decode: %w", err)
	}
	if len(doc.Versions) == 0 {
		return "", errors.New("go version resolver returned no versions")
	}
	return doc.Versions[len(doc.Versions)-1], nil
}

func (r DefaultDependencyVersionResolver) latestNPMVersion(ctx context.Context, packageName string) (string, error) {
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: dependencyUpdateResolverTimeout}
	}
	endpoint := "https://registry.npmjs.org/" + url.PathEscape(packageName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("npm version resolver: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("npm version resolver status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("npm version resolver decode: %w", err)
	}
	latest := strings.TrimSpace(doc.DistTags["latest"])
	if latest == "" {
		return "", errors.New("npm version resolver returned no latest dist-tag")
	}
	return latest, nil
}
