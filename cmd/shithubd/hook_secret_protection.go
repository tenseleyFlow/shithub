// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/secretscan"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
)

const (
	preReceiveSecretScanMaxFileBytes = 256 * 1024
	preReceiveSecretScanMaxFindings  = 10
)

type secretPushFinding struct {
	Commit  string
	Path    string
	Pattern string
	Line    int
}

type errHookSecretProtection struct {
	Findings   []secretPushFinding
	ReviewPath string
}

func (e errHookSecretProtection) Error() string {
	return "shithub-hook: secret push protection denied"
}

func (e errHookSecretProtection) Friendly() string {
	var b strings.Builder
	b.WriteString("shithub: push rejected because secret scanning found supported credential patterns.\n")
	for _, f := range e.Findings {
		fmt.Fprintf(&b, "shithub:   - %s:%d %s (%s)\n", f.Path, f.Line, f.Pattern, shortObjectID(f.Commit))
	}
	b.WriteString("shithub: Remove the secret and rotate the credential.")
	if e.ReviewPath != "" {
		fmt.Fprintf(&b, "\nshithub: Bypass requests were recorded for owner review: %s", e.ReviewPath)
	} else {
		b.WriteString(" If this is a false positive, allowlist the pattern and path from the repository security page before pushing again.")
	}
	return b.String()
}

func enforcePreReceiveSecretProtection(ctx context.Context, h *hookCtx, repo reposdb.Repo, gitDir string, refs []refUpdate) error {
	if !pushMayAddReachableObjects(refs) {
		return nil
	}
	enabled, err := preReceiveSecretProtectionEnabled(ctx, h, repo)
	if err != nil {
		return fmt.Errorf("secret push protection: entitlement: %w", err)
	}
	if !enabled {
		return nil
	}
	findings, err := scanPreReceiveSecrets(ctx, h.pool, repo, gitDir, refs)
	if err != nil {
		return fmt.Errorf("secret push protection: %w", err)
	}
	if len(findings) > 0 {
		reviewPath, err := recordPreReceiveSecretBypassRequests(ctx, h, repo, findings)
		if err != nil {
			return fmt.Errorf("secret push protection: bypass requests: %w", err)
		}
		return errHookSecretProtection{Findings: findings, ReviewPath: reviewPath}
	}
	return nil
}

func preReceiveSecretProtectionEnabled(ctx context.Context, h *hookCtx, repo reposdb.Repo) (bool, error) {
	if policy.NewRepoRefFromRepo(repo).IsPublic() {
		return true, nil
	}
	if repo.OwnerOrgID.Valid {
		decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: h.pool}, repo.OwnerOrgID.Int64, entitlements.FeatureSecretPushProtection)
		if err != nil {
			return false, err
		}
		return decision.Allowed, nil
	}
	// Personal private repos keep push-time blocking as a baseline
	// protection. Historical scans remain the Pro-gated surface.
	return true, nil
}

func scanPreReceiveSecrets(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo, gitDir string, refs []refUpdate) ([]secretPushFinding, error) {
	allowSet, err := loadPreReceiveSecretAllowlist(ctx, pool, repo.ID)
	if err != nil {
		return nil, err
	}
	bypassSet, err := loadPreReceiveSecretBypassSet(ctx, pool, repo.ID)
	if err != nil {
		return nil, err
	}
	scanPatterns, err := loadPreReceiveSecretPatterns(ctx, pool, repo)
	if err != nil {
		return nil, err
	}
	commits, err := preReceiveNewCommits(ctx, gitDir, refs)
	if err != nil {
		return nil, err
	}
	out := make([]secretPushFinding, 0)
	for _, commit := range commits {
		paths, err := preReceiveChangedPaths(ctx, gitDir, commit)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if shouldSkipPreReceiveSecretPath(path) {
				continue
			}
			blob, err := repogit.ReadBlobBytes(ctx, gitDir, commit, path, preReceiveSecretScanMaxFileBytes+1)
			if err != nil || len(blob) > preReceiveSecretScanMaxFileBytes || !isTextForPreReceiveSecretScan(blob) {
				continue
			}
			for _, finding := range secretscan.Scan(blob, secretscan.ScanOptions{Patterns: scanPatterns, MaxBytes: preReceiveSecretScanMaxFileBytes}) {
				if _, ok := allowSet[preReceiveAllowlistKey{Pattern: finding.Pattern, Path: path}]; ok {
					continue
				}
				if _, ok := bypassSet[preReceiveBypassKey{Pattern: finding.Pattern, Path: path, Commit: commit, Line: finding.Line}]; ok {
					continue
				}
				out = append(out, secretPushFinding{
					Commit:  commit,
					Path:    path,
					Pattern: finding.Pattern,
					Line:    finding.Line,
				})
				if len(out) >= preReceiveSecretScanMaxFindings {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

func recordPreReceiveSecretBypassRequests(ctx context.Context, h *hookCtx, repo reposdb.Repo, findings []secretPushFinding) (string, error) {
	owner, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, h.pool, repo.ID)
	if err != nil {
		return "", fmt.Errorf("load repo owner: %w", err)
	}
	reviewPath := "/" + url.PathEscape(ownerNameString(owner.OwnerUsername)) + "/" + url.PathEscape(owner.RepoName) + "/security/secret-scanning#secret-bypass-requests"
	sq := secretscandb.New()
	recorder := audit.NewRecorder()
	for _, finding := range findings {
		row, err := sq.UpsertSecretScanBypassRequest(ctx, h.pool, secretscandb.UpsertSecretScanBypassRequestParams{
			RepoID:        repo.ID,
			Pattern:       finding.Pattern,
			Path:          finding.Path,
			CommitOid:     finding.Commit,
			LineNo:        int32(finding.Line),
			RequestedBy:   pgtype.Int8{Int64: h.userID, Valid: h.userID != 0},
			RequestReason: "push protection false-positive review",
		})
		if err != nil {
			return "", err
		}
		if err := recorder.Record(ctx, h.pool, h.userID, audit.ActionSecretBypassRequested, audit.TargetRepo, repo.ID, map[string]any{
			"bypass_request_id": row.ID,
			"pattern":           row.Pattern,
			"path":              row.Path,
			"line_no":           row.LineNo,
			"commit_oid":        row.CommitOid,
			"status":            row.Status,
		}); err != nil {
			if h.logger != nil {
				h.logger.WarnContext(ctx, "secret push protection: audit bypass request", "repo_id", repo.ID, "request_id", row.ID, "error", err)
			}
		}
	}
	return reviewPath, nil
}

func loadPreReceiveSecretPatterns(ctx context.Context, pool *pgxpool.Pool, repo reposdb.Repo) ([]secretscan.Pattern, error) {
	if !repo.OwnerOrgID.Valid {
		return secretscan.Patterns, nil
	}
	decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: pool}, repo.OwnerOrgID.Int64, entitlements.FeatureSecretCustomPatterns)
	if err != nil {
		return nil, fmt.Errorf("custom pattern entitlement: %w", err)
	}
	if !decision.Allowed {
		return secretscan.Patterns, nil
	}
	rows, err := secretscandb.New().ListEnabledSecretScanCustomPatternsForOrg(ctx, pool, repo.OwnerOrgID.Int64)
	if err != nil {
		return nil, fmt.Errorf("load custom patterns: %w", err)
	}
	custom := make([]secretscan.Pattern, 0, len(rows))
	for _, row := range rows {
		p, err := secretscan.CompileCustomPattern(secretscan.CustomPatternSpec{
			Name:        row.Name,
			Description: row.Description,
			Pattern:     row.Pattern,
			MinMatchLen: int(row.MinMatchLen),
		})
		if err != nil {
			continue
		}
		custom = append(custom, p)
	}
	return secretscan.PatternsWithCustom(custom), nil
}

func preReceiveNewCommits(ctx context.Context, gitDir string, refs []refUpdate) ([]string, error) {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, rf := range refs {
		if isZeroObjectID(rf.after) {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-list", "--reverse", rf.after, "--not", "--all")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(raw))
			if strings.Contains(msg, "object is a blob") || strings.Contains(msg, "object is a tree") {
				continue
			}
			return nil, fmt.Errorf("git rev-list: %w (%s)", err, msg)
		}
		for _, commit := range strings.Fields(string(raw)) {
			if _, ok := seen[commit]; ok {
				continue
			}
			seen[commit] = struct{}{}
			out = append(out, commit)
		}
	}
	return out, nil
}

func preReceiveChangedPaths(ctx context.Context, gitDir, commit string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "diff-tree", "--root", "--no-commit-id", "-r", "-m", "--name-only", "-z", "--diff-filter=ACMRT", commit)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff-tree: %w", err)
	}
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		path := string(part)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out, nil
}

type preReceiveAllowlistKey struct {
	Pattern string
	Path    string
}

type preReceiveBypassKey struct {
	Pattern string
	Path    string
	Commit  string
	Line    int
}

func loadPreReceiveSecretAllowlist(ctx context.Context, pool *pgxpool.Pool, repoID int64) (map[preReceiveAllowlistKey]struct{}, error) {
	rows, err := secretscandb.New().ListSecretScanAllowlistForRepo(ctx, pool, repoID)
	if err != nil {
		return nil, fmt.Errorf("load allowlist: %w", err)
	}
	out := make(map[preReceiveAllowlistKey]struct{}, len(rows))
	for _, row := range rows {
		out[preReceiveAllowlistKey{Pattern: row.Pattern, Path: row.Path}] = struct{}{}
	}
	return out, nil
}

func loadPreReceiveSecretBypassSet(ctx context.Context, pool *pgxpool.Pool, repoID int64) (map[preReceiveBypassKey]struct{}, error) {
	rows, err := secretscandb.New().ListApprovedSecretScanBypassesForRepo(ctx, pool, repoID)
	if err != nil {
		return nil, fmt.Errorf("load bypass requests: %w", err)
	}
	out := make(map[preReceiveBypassKey]struct{}, len(rows))
	for _, row := range rows {
		out[preReceiveBypassKey{
			Pattern: row.Pattern,
			Path:    row.Path,
			Commit:  row.CommitOid,
			Line:    int(row.LineNo),
		}] = struct{}{}
	}
	return out, nil
}

func shouldSkipPreReceiveSecretPath(path string) bool {
	if strings.HasPrefix(path, ".git") {
		return true
	}
	for _, prefix := range []string{"vendor/", "node_modules/", "dist/"} {
		if strings.HasPrefix(path, prefix) || strings.Contains(path, "/"+prefix) {
			return true
		}
	}
	return false
}

func isTextForPreReceiveSecretScan(body []byte) bool {
	limit := len(body)
	if limit > 8192 {
		limit = 8192
	}
	return bytes.IndexByte(body[:limit], 0) < 0
}

func shortObjectID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

func ownerNameString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprint(s)
	}
}
