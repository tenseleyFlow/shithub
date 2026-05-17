// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/url"
	"strconv"
	"strings"
)

func sameRepoLocalDetailsHref(owner, repoName, href string) string {
	href = strings.TrimSpace(href)
	if !sameRepoLocalPath(owner, repoName, href) {
		return ""
	}
	return href
}

func sameRepoLocalPath(owner, repoName, href string) bool {
	if !safeLocalPath(href) {
		return false
	}
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	parts := localPathParts(u.Path)
	if len(parts) < 2 {
		return false
	}
	return strings.EqualFold(parts[0], owner) && strings.EqualFold(parts[1], repoName)
}

func localActionsRunRerunHref(owner, repoName, href string) (string, bool) {
	if !sameRepoLocalPath(owner, repoName, href) {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", false
	}
	parts := localPathParts(u.Path)
	if len(parts) != 5 || parts[2] != "actions" || parts[3] != "runs" {
		return "", false
	}
	if _, err := strconv.ParseInt(parts[4], 10, 64); err != nil {
		return "", false
	}
	return "/" + owner + "/" + repoName + "/actions/runs/" + parts[4] + "/rerun", true
}

func localPathParts(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
