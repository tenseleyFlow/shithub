// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/url"
	"strings"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

type codeTreeEntryRow struct {
	Entry      repogit.TreeEntry
	FullPath   string
	URL        string
	LastCommit repogit.Commit
	LastFound  bool
}

func (h *Handlers) codeTreeEntryRows(ctx context.Context, cc *codeContext, entries []repogit.TreeEntry) []codeTreeEntryRow {
	submodules := map[string]repogit.Submodule{}
	if hasSubmoduleEntries(entries) {
		var err error
		submodules, err = repogit.Submodules(ctx, cc.gitDir, cc.ref)
		if err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodules", "error", err)
			submodules = map[string]repogit.Submodule{}
		}
	}

	rows := make([]codeTreeEntryRow, 0, len(entries))
	for _, e := range entries {
		fullPath := joinPath(cc.subpath, e.Name)
		entryURL := treeEntryURL(cc.owner, cc.row.Name, cc.ref, e.Kind, fullPath)
		if e.Kind == repogit.EntrySubmod {
			entryURL = ""
			if sm, ok := submodules[fullPath]; ok {
				entryURL = h.submoduleTreeURL(ctx, cc, sm.URL, e.OID)
			}
		}
		row := codeTreeEntryRow{
			Entry:    e,
			FullPath: fullPath,
			URL:      entryURL,
		}
		if commits, err := repogit.Log(ctx, cc.gitDir, repogit.LogOptions{
			Ref:      cc.ref,
			MaxCount: 1,
			Path:     fullPath,
		}); err == nil && len(commits) > 0 {
			row.LastCommit = commits[0]
			row.LastFound = true
		} else if err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: row history", "error", err, "path", fullPath)
		}
		rows = append(rows, row)
	}
	return rows
}

func hasSubmoduleEntries(entries []repogit.TreeEntry) bool {
	for _, e := range entries {
		if e.Kind == repogit.EntrySubmod {
			return true
		}
	}
	return false
}

func treeEntryURL(owner, repoName, ref string, kind repogit.TreeEntryKind, fullPath string) string {
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName)
	refPath := escapePathSegments(ref)
	switch kind {
	case repogit.EntryTree:
		return base + "/tree/" + refPath + "/" + escapePathSegments(fullPath)
	case repogit.EntryBlob:
		return base + "/blob/" + refPath + "/" + escapePathSegments(fullPath)
	default:
		return ""
	}
}

func escapePathSegments(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
