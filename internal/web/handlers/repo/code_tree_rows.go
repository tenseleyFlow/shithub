// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/url"
	"strings"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/treecache"
)

type codeTreeEntryRow struct {
	Entry      repogit.TreeEntry
	FullPath   string
	URL        string
	LastCommit repogit.Commit
	LastFound  bool
}

// codeTreeEntryRows builds the tree listing's rows, including the
// last-commit column.
//
// The column used to cost one `git log -1 -- <path>` per entry, run
// serially: a 100-entry directory forked git 100 times for one
// anonymous page view. It is now one cached `git log --name-only`
// walk for the whole directory (see repogit.EntryLastCommits), with
// the per-path query kept only as the fallback for entries the walk
// could not resolve inside its commit bound.
//
// commitOID is the resolved OID of the commit being rendered and is
// the cache key's invalidation lever; pass "" (unresolvable ref) to
// bypass the cache.
func (h *Handlers) codeTreeEntryRows(
	ctx context.Context, cc *codeContext, entries []repogit.TreeEntry, commitOID string,
) []codeTreeEntryRow {
	submodules := map[string]repogit.Submodule{}
	if hasSubmoduleEntries(entries) {
		var err error
		submodules, err = repogit.Submodules(ctx, cc.gitDir, cc.ref)
		if err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: submodules", "error", err)
			submodules = map[string]repogit.Submodule{}
		}
	}

	lastCommits := h.entryLastCommits(ctx, cc, commitOID, entries)

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
		if commit, ok := lastCommits[e.Name]; ok {
			row.LastCommit = commit
			row.LastFound = true
		} else if commit, ok := h.entryLastCommitFallback(ctx, cc, fullPath); ok {
			row.LastCommit = commit
			row.LastFound = true
		}
		rows = append(rows, row)
	}
	return rows
}

// entryLastCommits resolves the whole directory's last-commit column
// in one walk, memoized on (repo, commit OID, subpath). A failure
// returns an empty map, which degrades to the old one-fork-per-entry
// behavior rather than dropping the column.
func (h *Handlers) entryLastCommits(
	ctx context.Context, cc *codeContext, commitOID string, entries []repogit.TreeEntry,
) map[string]repogit.Commit {
	if len(entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	// Walk from the resolved OID when we have one so the walk and the
	// cache key describe the same commit even if the ref moves
	// mid-request.
	walkRef := commitOID
	if walkRef == "" {
		walkRef = cc.ref
	}
	key := treecache.EntryKey{RepoID: cc.row.ID, CommitOID: commitOID, Subpath: cc.subpath}
	out, err := h.d.TreeCache.LastCommits(ctx, key, func(ctx context.Context) (map[string]repogit.Commit, error) {
		return repogit.EntryLastCommits(ctx, cc.gitDir, repogit.LastCommitOptions{
			Ref:   walkRef,
			Dir:   cc.subpath,
			Names: names,
		})
	})
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: entry last commits", "error", err, "path", cc.subpath)
		}
		return nil
	}
	return out
}

// entryLastCommitFallback is the per-path `git log -1` the walk
// replaces. It runs only for entries the walk left unresolved —
// entries untouched within the walk bound, or paths git had to quote.
func (h *Handlers) entryLastCommitFallback(
	ctx context.Context, cc *codeContext, fullPath string,
) (repogit.Commit, bool) {
	commits, err := repogit.Log(ctx, cc.gitDir, repogit.LogOptions{
		Ref:      cc.ref,
		MaxCount: 1,
		Path:     fullPath,
	})
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "code: row history", "error", err, "path", fullPath)
		}
		return repogit.Commit{}, false
	}
	if len(commits) == 0 {
		return repogit.Commit{}, false
	}
	return commits[0], true
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
