// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import (
	"context"
	"errors"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/repos/git"
)

// WorkflowFile is one parseable file discovered in the repo's
// `.shithub/workflows/` directory at a specific SHA.
type WorkflowFile struct {
	// Path is the repo-relative blob path
	// (e.g., ".shithub/workflows/ci.yml").
	Path string
	// Bytes is the file contents capped at workflow.MaxWorkflowFileBytes.
	// The trigger handler hands these straight to workflow.Parse.
	Bytes []byte
}

// DiscoverSkip captures a file that was found but skipped before
// parsing — typically because it exceeded the size cap. The trigger
// handler logs these so operators can spot a workflow that's grown
// out of bounds.
type DiscoverSkip struct {
	Path   string
	Reason string
}

// Discover walks `.shithub/workflows/` at the given SHA and returns
// every `.yml`/`.yaml` file's bytes (capped at MaxWorkflowFileBytes).
// Files that exceed the cap appear in the returned skips list, not
// in files — they need operator attention but shouldn't block other
// workflows on the same commit from triggering.
//
// A repo with no `.shithub/workflows/` directory returns
// (nil, nil, nil) — common case for repos that haven't adopted CI yet,
// not an error.
//
// The function does not parse. The trigger handler parses each
// returned file and handles parse-time diagnostics there; keeping
// parse separate from discover lets the handler log + skip per-file
// without unwinding the discovery work.
func Discover(ctx context.Context, gitDir, sha string) (files []WorkflowFile, skips []DiscoverSkip, err error) {
	entries, err := git.LsTree(ctx, gitDir, sha, ".shithub/workflows")
	if err != nil {
		// Missing directory is the common case (repo without CI).
		// Surface a typed nil/nil/nil so callers don't need an error
		// branch for the no-workflows path.
		if errors.Is(err, git.ErrNotATree) {
			return nil, nil, nil
		}
		// `Not a valid object name` is what `git ls-tree` says when
		// the path doesn't exist at the given SHA. Same outcome.
		if isMissingPath(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	for _, e := range entries {
		if !isWorkflowYAMLName(e.Name) {
			continue
		}
		path := ".shithub/workflows/" + e.Name
		body, err := git.ReadBlobBytes(ctx, gitDir, sha, path, int64(workflow.MaxWorkflowFileBytes))
		switch {
		case err == nil:
			files = append(files, WorkflowFile{Path: path, Bytes: body})
		case errors.Is(err, git.ErrBlobTooLarge):
			skips = append(skips, DiscoverSkip{
				Path:   path,
				Reason: "exceeds " + sizeStr(workflow.MaxWorkflowFileBytes) + " size cap",
			})
		default:
			// Read errors on individual blobs shouldn't tank the whole
			// discovery — record + continue. The handler logs skips so
			// operators see them.
			skips = append(skips, DiscoverSkip{Path: path, Reason: "read: " + err.Error()})
		}
	}
	return files, skips, nil
}

// isWorkflowYAMLName accepts files ending in `.yml` or `.yaml`,
// case-insensitive on the suffix. Other files in the directory
// (READMEs, partials authors might drop in) are silently ignored.
func isWorkflowYAMLName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")
}

// isMissingPath checks for the "path doesn't exist at this SHA" shape
// that `git ls-tree` surfaces. The treeops layer already maps the
// "not a tree" case to ErrNotATree, but a path that simply isn't in
// the tree comes back as a generic exec error — we recognize it by
// its stderr text rather than introducing a typed error in treeops.
func isMissingPath(err error) bool {
	s := err.Error()
	return strings.Contains(s, "not a valid object name") ||
		strings.Contains(s, "Not a valid object name") ||
		strings.Contains(s, "exists on disk, but not in")
}

func sizeStr(n int) string {
	switch {
	case n >= 1024*1024:
		return formatInt(n/(1024*1024)) + " MiB"
	case n >= 1024:
		return formatInt(n/1024) + " KiB"
	}
	return formatInt(n) + " B"
}

func formatInt(n int) string {
	// Trivial inline to avoid an strconv import for one call site.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
