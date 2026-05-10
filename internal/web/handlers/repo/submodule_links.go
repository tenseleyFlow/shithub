// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/url"
	pathpkg "path"
	"strings"
)

type submoduleRouteConfig struct {
	owner    string
	repoName string
	baseURL  string
	sshHost  string
}

func (h *Handlers) submoduleTreeURL(cc *codeContext, remoteURL, oid string) string {
	return submoduleRouteURL(submoduleRouteConfig{
		owner:    cc.owner,
		repoName: cc.row.Name,
		baseURL:  h.d.CloneURLs.BaseURL,
		sshHost:  h.d.CloneURLs.SSHHost,
	}, remoteURL, oid)
}

func submoduleRouteURL(cfg submoduleRouteConfig, remoteURL, oid string) string {
	owner, repoName, ok := submoduleRepoTarget(cfg, remoteURL)
	if !ok || oid == "" {
		return ""
	}
	return "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/tree/" + url.PathEscape(oid)
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
