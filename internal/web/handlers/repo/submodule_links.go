// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/url"
	pathpkg "path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type submoduleRouteConfig struct {
	owner    string
	repoName string
	baseURL  string
	sshHost  string
}

type submoduleRoute struct {
	Owner    string
	RepoName string
	TreeURL  string
	RepoURL  string
}

func (h *Handlers) submoduleTreeURL(ctx context.Context, cc *codeContext, remoteURL, oid string) string {
	route, ok := submoduleRouteForRemote(submoduleRouteConfig{
		owner:    cc.owner,
		repoName: cc.row.Name,
		baseURL:  h.d.CloneURLs.BaseURL,
		sshHost:  h.d.CloneURLs.SSHHost,
	}, remoteURL, oid)
	if !ok {
		return ""
	}
	gitDir, exists, err := h.localSubmoduleRepo(route.Owner, route.RepoName)
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule repo lookup", "error", err, "owner", route.Owner, "repo", route.RepoName)
		}
		return ""
	}
	if !exists {
		return ""
	}
	existsAtCommit, err := repogit.CommitExists(ctx, gitDir, oid)
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule commit lookup", "error", err, "owner", route.Owner, "repo", route.RepoName, "oid", oid)
		}
		return route.RepoURL
	}
	if !existsAtCommit {
		if fetchURL, ok := githubSubmoduleFetchURL(remoteURL); ok {
			backfilled, backfillErr := h.backfillSubmoduleCommit(ctx, gitDir, fetchURL, oid)
			if backfillErr != nil {
				if h.d.Logger != nil {
					h.d.Logger.WarnContext(ctx, "code: submodule backfill fetch", "error", backfillErr, "owner", route.Owner, "repo", route.RepoName, "oid", oid, "remote", fetchURL)
				}
			} else if backfilled {
				h.recordSubmoduleBackfill(ctx, route.Owner, route.RepoName, gitDir)
				return route.TreeURL
			}
		}
		return route.RepoURL
	}
	return route.TreeURL
}

func (h *Handlers) backfillSubmoduleCommit(ctx context.Context, gitDir, fetchURL, oid string) (bool, error) {
	key := gitDir + "\x00" + fetchURL + "\x00" + oid
	v, err, _ := h.submoduleBackfills.Do(key, func() (any, error) {
		if exists, err := repogit.CommitExists(ctx, gitDir, oid); err != nil || exists {
			return exists, err
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := repogit.FetchRemoteHeadsAndTags(fetchCtx, gitDir, fetchURL); err != nil {
			if exists, existsErr := repogit.CommitExists(ctx, gitDir, oid); existsErr == nil && exists {
				return true, nil
			}
			return false, err
		}
		return repogit.CommitExists(ctx, gitDir, oid)
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

func (h *Handlers) recordSubmoduleBackfill(ctx context.Context, owner, repoName, gitDir string) {
	row, ok := h.lookupSubmoduleRepoRow(ctx, owner, repoName)
	if !ok {
		return
	}
	if head, found, err := repogit.HeadOf(ctx, gitDir, row.DefaultBranch); err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill default ref", "error", err, "repo_id", row.ID)
		}
	} else if found && (!row.DefaultBranchOid.Valid || row.DefaultBranchOid.String != head.OID) {
		if err := h.rq.UpdateRepoDefaultBranchOID(ctx, h.d.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
			ID:               row.ID,
			DefaultBranchOid: pgtype.Text{String: head.OID, Valid: true},
		}); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill default oid", "error", err, "repo_id", row.ID)
		}
		if _, err := worker.Enqueue(ctx, h.d.Pool, worker.KindRepoIndexCode, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill enqueue index", "error", err, "repo_id", row.ID)
		}
	}
	if _, err := worker.Enqueue(ctx, h.d.Pool, worker.KindRepoSizeRecalc, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "code: submodule backfill enqueue size", "error", err, "repo_id", row.ID)
	}
	_ = worker.Notify(ctx, h.d.Pool)
}

func (h *Handlers) lookupSubmoduleRepoRow(ctx context.Context, owner, repoName string) (reposdb.Repo, bool) {
	repoName = strings.ToLower(strings.TrimSpace(repoName))
	if user, err := h.uq.GetUserByUsername(ctx, h.d.Pool, owner); err == nil {
		row, repoErr := h.rq.GetRepoByOwnerUserAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: user.ID, Valid: true},
			Name:        repoName,
		})
		if repoErr == nil {
			return row, true
		}
		if !errors.Is(repoErr, pgx.ErrNoRows) && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill user repo lookup", "error", repoErr, "owner", owner, "repo", repoName)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "code: submodule backfill user lookup", "error", err, "owner", owner)
	}

	org, err := orgsdb.New().GetOrgBySlug(ctx, h.d.Pool, owner)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill org lookup", "error", err, "owner", owner)
		}
		return reposdb.Repo{}, false
	}
	row, err := h.rq.GetRepoByOwnerOrgAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerOrgAndNameParams{
		OwnerOrgID: pgtype.Int8{Int64: org.ID, Valid: true},
		Name:       repoName,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodule backfill org repo lookup", "error", err, "owner", owner, "repo", repoName)
		}
		return reposdb.Repo{}, false
	}
	return row, true
}

func submoduleRouteURL(cfg submoduleRouteConfig, remoteURL, oid string) string {
	route, ok := submoduleRouteForRemote(cfg, remoteURL, oid)
	if !ok {
		return ""
	}
	return route.TreeURL
}

func submoduleRouteForRemote(cfg submoduleRouteConfig, remoteURL, oid string) (submoduleRoute, bool) {
	owner, repoName, ok := submoduleRepoTarget(cfg, remoteURL)
	if !ok || oid == "" {
		return submoduleRoute{}, false
	}
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName)
	return submoduleRoute{
		Owner:    owner,
		RepoName: repoName,
		TreeURL:  base + "/tree/" + url.PathEscape(oid),
		RepoURL:  base,
	}, true
}

func (h *Handlers) localSubmoduleRepo(owner, repoName string) (string, bool, error) {
	gitDir, err := h.d.RepoFS.RepoPath(owner, repoName)
	if err != nil {
		return "", false, err
	}
	exists, err := h.d.RepoFS.Exists(gitDir)
	if err != nil {
		return "", false, err
	}
	return gitDir, exists, nil
}

func submoduleRepoTarget(cfg submoduleRouteConfig, remoteURL string) (owner, repoName string, ok bool) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", "", false
	}
	if owner, repoName, ok := submoduleRepoFromRelativeURL(cfg, remoteURL); ok {
		return owner, repoName, true
	}
	if host, repoPath, ok := scpLikeRemote(remoteURL); ok {
		if !isLocalSubmoduleHost(host, cfg) {
			return "", "", false
		}
		return ownerRepoFromRemotePath(repoPath)
	}
	u, err := url.Parse(remoteURL)
	if err != nil || u.Scheme == "" {
		return "", "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ssh", "git":
	default:
		return "", "", false
	}
	if !isLocalSubmoduleHost(u.Hostname(), cfg) {
		return "", "", false
	}
	return ownerRepoFromRemotePath(strings.TrimPrefix(u.EscapedPath(), "/"))
}

func githubSubmoduleFetchURL(remoteURL string) (string, bool) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", false
	}
	var repoPath string
	if host, path, ok := scpLikeRemote(remoteURL); ok {
		if normalizeRemoteHost(host) != "github.com" {
			return "", false
		}
		repoPath = path
	} else {
		u, err := url.Parse(remoteURL)
		if err != nil || u.Scheme == "" {
			return "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "ssh", "git":
		default:
			return "", false
		}
		if normalizeRemoteHost(u.Hostname()) != "github.com" {
			return "", false
		}
		repoPath = strings.TrimPrefix(u.EscapedPath(), "/")
	}
	owner, repoName, ok := ownerRepoFromRemotePath(repoPath)
	if !ok {
		return "", false
	}
	return "https://github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + ".git", true
}

func submoduleRepoFromRelativeURL(cfg submoduleRouteConfig, remoteURL string) (owner, repoName string, ok bool) {
	if pathpkg.IsAbs(remoteURL) || strings.Contains(remoteURL, "://") {
		return "", "", false
	}
	if _, _, ok := scpLikeRemote(remoteURL); ok {
		return "", "", false
	}
	basePath := cfg.owner + "/" + cfg.repoName + ".git"
	cleaned := pathpkg.Clean(pathpkg.Join(basePath, remoteURL))
	if cleaned == "." || cleaned == ".." || pathpkg.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") {
		return "", "", false
	}
	return ownerRepoFromRemotePath(cleaned)
}

func scpLikeRemote(remoteURL string) (host, repoPath string, ok bool) {
	if strings.Contains(remoteURL, "://") {
		return "", "", false
	}
	colon := strings.IndexByte(remoteURL, ':')
	if colon <= 0 {
		return "", "", false
	}
	if slash := strings.IndexByte(remoteURL, '/'); slash >= 0 && slash < colon {
		return "", "", false
	}
	host = strings.TrimSpace(remoteURL[:colon])
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	repoPath = strings.TrimSpace(remoteURL[colon+1:])
	return host, repoPath, host != "" && repoPath != ""
}

func ownerRepoFromRemotePath(repoPath string) (owner, repoName string, ok bool) {
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	repoPath = strings.TrimSuffix(repoPath, "/")
	parts := strings.Split(repoPath, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner, ok = cleanRemotePathSegment(parts[0])
	if !ok {
		return "", "", false
	}
	repoSegment := strings.TrimSuffix(parts[1], ".git")
	repoName, ok = cleanRemotePathSegment(repoSegment)
	if !ok {
		return "", "", false
	}
	return owner, repoName, true
}

func cleanRemotePathSegment(segment string) (string, bool) {
	segment = strings.TrimSpace(segment)
	if segment == "" || segment == "." || segment == ".." {
		return "", false
	}
	unescaped, err := url.PathUnescape(segment)
	if err != nil {
		return "", false
	}
	if unescaped == "" || unescaped == "." || unescaped == ".." || strings.Contains(unescaped, "/") {
		return "", false
	}
	return unescaped, true
}

func isLocalSubmoduleHost(host string, cfg submoduleRouteConfig) bool {
	host = normalizeRemoteHost(host)
	if host == "" {
		return false
	}
	if host == "github.com" {
		return true
	}
	if baseHost := configuredBaseHostname(cfg.baseURL); baseHost != "" && host == baseHost {
		return true
	}
	if sshHost := configuredSSHHostname(cfg.sshHost); sshHost != "" && host == sshHost {
		return true
	}
	return false
}

func configuredBaseHostname(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return normalizeRemoteHost(u.Hostname())
}

func configuredSSHHostname(sshHost string) string {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return ""
	}
	if u, err := url.Parse(sshHost); err == nil && u.Hostname() != "" {
		return normalizeRemoteHost(u.Hostname())
	}
	if at := strings.LastIndexByte(sshHost, '@'); at >= 0 {
		sshHost = sshHost[at+1:]
	}
	if colon := strings.IndexByte(sshHost, ':'); colon >= 0 {
		sshHost = sshHost[:colon]
	}
	return normalizeRemoteHost(sshHost)
}

func normalizeRemoteHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimSuffix(host, ".")
	return host
}
