// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type compareMenuTarget string

const (
	compareMenuTargetCompare compareMenuTarget = "compare"
	compareMenuTargetPullNew compareMenuTarget = "pull_new"
)

type compareRefOption struct {
	Name      string
	Href      string
	Current   bool
	IsDefault bool
}

type compareRefMenu struct {
	ID      string
	Label   string
	Title   string
	Current string

	Branches []compareRefOption
	Tags     []compareRefOption
}

type compareExample struct {
	Name string
	Href string
}

type compareStats struct {
	CommitCount      int
	FileCount        int
	ContributorCount int
}

type compareMergeState struct {
	State       string
	Label       string
	Description string
}

type compareState struct {
	Base         string
	Head         string
	HasSelection bool
	SameRef      bool
	NotFound     bool
	CommitsErr   bool
	NoCommits    bool
	Ahead        int
	Behind       int

	Commits     []repogit.Commit
	CommitRows  []compareCommitRow
	DiffHTML    string
	Stats       compareStats
	MergeState  compareMergeState
	CanOpenPull bool
	PullNewHref string

	BaseMenu compareRefMenu
	HeadMenu compareRefMenu
	Examples []compareExample
}

type compareCommitRow struct {
	Commit repogit.Commit
	Checks codeCommitCheckSummary
}

func (h *Handlers) repoPageChrome(r *http.Request, owner string, row reposdb.Repo, activeSubnav string) map[string]any {
	return map[string]any{
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"Owner":        owner,
		"Repo":         row,
		"RepoActions":  h.repoActions(r, row.ID),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": activeSubnav,
	}
}

func mergePageData(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (h *Handlers) buildCompareState(r *http.Request, owner string, row reposdb.Repo, gitDir, base, head string, hasSelection bool, target compareMenuTarget) compareState {
	if strings.TrimSpace(base) == "" {
		base = row.DefaultBranch
	}
	if strings.TrimSpace(head) == "" {
		head = row.DefaultBranch
	}

	refs, _ := repogit.ListRefs(r.Context(), gitDir)
	state := compareState{
		Base:         base,
		Head:         head,
		HasSelection: hasSelection,
		SameRef:      base == head,
		PullNewHref:  pullNewURL(owner, row.Name, base, head),
		MergeState: compareMergeState{
			State:       "pending",
			Label:       "Checking mergeability...",
			Description: "You can still create the pull request while shithub checks these branches.",
		},
	}
	state.BaseMenu, state.HeadMenu = buildCompareMenus(owner, row.Name, row.DefaultBranch, base, head, refs, target)
	state.Examples = buildCompareExamples(owner, row.Name, row.DefaultBranch, refs)

	if !hasSelection || base == "" || head == "" {
		state.MergeState = compareMergeState{}
		return state
	}

	commits, cerr := repogit.CommitsBetween(r.Context(), gitDir, base, head, 250)
	if cerr != nil {
		state.CommitsErr = true
	}
	state.Commits = commits
	state.CommitRows = h.compareCommitRows(r.Context(), owner, row.Name, row.ID, commits)

	ahead, behind, abErr := repogit.AheadBehind(r.Context(), gitDir, base, head)
	if abErr != nil {
		state.NotFound = true
		state.MergeState = compareMergeState{
			State:       "missing",
			Label:       "There was a problem comparing these refs.",
			Description: "One or both refs were not found in this repository.",
		}
		return state
	}
	state.Ahead = ahead
	state.Behind = behind
	state.NoCommits = ahead <= 0
	state.Stats.CommitCount = len(commits)
	state.Stats.ContributorCount = countCommitContributors(commits)

	if state.SameRef {
		state.MergeState = compareMergeState{}
		return state
	}
	if state.NoCommits {
		state.MergeState = compareMergeState{
			State:       "empty",
			Label:       "There isn't anything to compare.",
			Description: head + " is up to date with " + base + ".",
		}
		return state
	}

	patch, perr := compareSourceMergeBase(r, gitDir, base, head)
	if perr == nil {
		state.DiffHTML = renderCompareDiff(patch)
		state.Stats.FileCount = countPatchFiles(patch)
	}
	state.CanOpenPull = true
	state.MergeState = probeCompareMerge(r.Context(), gitDir, base, head)
	return state
}

func (h *Handlers) compareCommitRows(ctx context.Context, owner, repoName string, repoID int64, commits []repogit.Commit) []compareCommitRow {
	rows := make([]compareCommitRow, 0, len(commits))
	if len(commits) == 0 {
		return rows
	}
	oids := make([]string, 0, len(commits))
	for _, commit := range commits {
		oids = append(oids, commit.OID)
	}
	checkSummaries := h.codeCommitCheckSummaries(ctx, owner, repoName, repoID, oids)
	for _, commit := range commits {
		rows = append(rows, compareCommitRow{
			Commit: commit,
			Checks: checkSummaries[commit.OID],
		})
	}
	return rows
}

func buildCompareMenus(owner, repo, defaultBranch, base, head string, refs repogit.RefListing, target compareMenuTarget) (compareRefMenu, compareRefMenu) {
	baseMenu := compareRefMenu{
		ID:      "base",
		Label:   "base:",
		Title:   "Choose a base ref",
		Current: base,
	}
	headMenu := compareRefMenu{
		ID:      "head",
		Label:   "compare:",
		Title:   "Choose a head ref",
		Current: head,
	}

	baseMenu.Branches = compareRefOptions(owner, repo, defaultBranch, base, head, base, refs.Branches, target, true)
	headMenu.Branches = compareRefOptions(owner, repo, defaultBranch, base, head, head, refs.Branches, target, false)
	baseMenu.Tags = compareRefOptions(owner, repo, defaultBranch, base, head, base, refs.Tags, target, true)
	headMenu.Tags = compareRefOptions(owner, repo, defaultBranch, base, head, head, refs.Tags, target, false)

	baseMenu.Branches = ensureCompareRefOption(baseMenu.Branches, owner, repo, defaultBranch, base, head, base, target, true)
	headMenu.Branches = ensureCompareRefOption(headMenu.Branches, owner, repo, defaultBranch, base, head, head, target, false)
	return baseMenu, headMenu
}

func compareRefOptions(owner, repo, defaultBranch, base, head, current string, refs []repogit.RefEntry, target compareMenuTarget, changingBase bool) []compareRefOption {
	options := make([]compareRefOption, 0, len(refs))
	for _, ref := range refs {
		options = append(options, compareRefOption{
			Name:      ref.Name,
			Href:      compareRefHref(owner, repo, base, head, ref.Name, target, changingBase),
			Current:   ref.Name == current,
			IsDefault: ref.Name == defaultBranch,
		})
	}
	return options
}

func ensureCompareRefOption(options []compareRefOption, owner, repo, defaultBranch, base, head, current string, target compareMenuTarget, changingBase bool) []compareRefOption {
	if current == "" {
		return options
	}
	for _, option := range options {
		if option.Name == current {
			return options
		}
	}
	return append([]compareRefOption{{
		Name:      current,
		Href:      compareRefHref(owner, repo, base, head, current, target, changingBase),
		Current:   true,
		IsDefault: current == defaultBranch,
	}}, options...)
}

func compareRefHref(owner, repo, base, head, ref string, target compareMenuTarget, changingBase bool) string {
	if changingBase {
		base = ref
	} else {
		head = ref
	}
	if target == compareMenuTargetPullNew {
		return pullNewURL(owner, repo, base, head)
	}
	return compareURL(owner, repo, base, head)
}

func buildCompareExamples(owner, repo, defaultBranch string, refs repogit.RefListing) []compareExample {
	examples := make([]compareExample, 0, 5)
	for _, branch := range refs.Branches {
		if branch.Name == defaultBranch {
			continue
		}
		examples = append(examples, compareExample{
			Name: branch.Name,
			Href: compareURL(owner, repo, defaultBranch, branch.Name),
		})
		if len(examples) == 5 {
			break
		}
	}
	return examples
}

func compareURL(owner, repo, base, head string) string {
	return "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/compare/" + escapePathSegments(base) + "..." + escapePathSegments(head)
}

func pullNewURL(owner, repo, base, head string) string {
	q := url.Values{}
	q.Set("base", base)
	q.Set("head", head)
	return "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/new?" + q.Encode()
}

func countPatchFiles(patch []byte) int {
	if len(patch) == 0 {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			count++
		}
	}
	return count
}

func countCommitContributors(commits []repogit.Commit) int {
	if len(commits) == 0 {
		return 0
	}
	seen := map[string]struct{}{}
	for _, commit := range commits {
		key := strings.ToLower(strings.TrimSpace(commit.AuthorEmail))
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(commit.AuthorName))
		}
		if key != "" {
			seen[key] = struct{}{}
		}
	}
	return len(seen)
}

func defaultPullTitle(head string, commits []repogit.Commit) string {
	if len(commits) == 1 && strings.TrimSpace(commits[0].Subject) != "" {
		return commits[0].Subject
	}
	if strings.TrimSpace(head) == "" {
		return ""
	}
	return head
}

func probeCompareMerge(ctx context.Context, gitDir, base, head string) compareMergeState {
	baseOID, berr := repogit.ResolveRefOID(ctx, gitDir, base)
	headOID, herr := repogit.ResolveRefOID(ctx, gitDir, head)
	if berr != nil || herr != nil {
		return compareMergeState{
			State:       "missing",
			Label:       "Unable to check mergeability.",
			Description: "One or both refs could not be resolved.",
		}
	}
	result, err := repogit.ProbeMerge(ctx, gitDir, baseOID, headOID)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return compareMergeState{
				State:       "missing",
				Label:       "Unable to check mergeability.",
				Description: "One or both refs could not be resolved.",
			}
		}
		return compareMergeState{
			State:       "unknown",
			Label:       "Mergeability could not be checked.",
			Description: "You can still create the pull request and shithub will retry the check.",
		}
	}
	if result.HasConflict {
		return compareMergeState{
			State:       "conflict",
			Label:       "Cannot automatically merge.",
			Description: "These branches have conflicts that must be resolved.",
		}
	}
	return compareMergeState{
		State:       "clean",
		Label:       "Able to merge.",
		Description: "These branches can be automatically merged.",
	}
}
