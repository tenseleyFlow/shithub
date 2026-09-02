// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"os/exec"
	"sync/atomic"
	"time"
)

// Every git subprocess this package spawns goes through gitCmd so we
// have exactly one place that (a) counts forks and (b) can grow
// process-wide policy later (nice level, env scrubbing, tracing).
//
// The counter is the measurement lever for the code-tab work: a page
// that forks git O(entries) times shows up as a linear ForkCount
// delta, and the tests assert the delta is constant instead.
var forkCount atomic.Uint64

// ForkCount reports the cumulative number of git subprocesses this
// process has started through this package. It only ever increases;
// callers that want a per-operation number read it before and after
// and subtract. Exported for tests and the /metrics surface.
//
// Note the pack/transport paths (internal/git/protocol,
// internal/web/handlers/githttp) run their own `git upload-pack` /
// `receive-pack` processes and are deliberately NOT counted here —
// they are long-lived streams, not the short read forks this counter
// is about.
func ForkCount() uint64 { return forkCount.Load() }

// ReadTimeout bounds a single read-only git invocation on a request
// path. Reads on this side of the package are local-disk object
// lookups: `ls-tree`, `rev-list --count`, `log`, `cat-file`. On the
// production box the slowest of these is a full recursive `ls-tree`
// on the largest repo, measured in tens of milliseconds; 30s is three
// orders of magnitude of headroom and exists purely so a wedged or
// pathological invocation cannot pin a request goroutine (and its
// git subprocess) forever when a crawler is walking every SHA.
//
// Blob streaming (StreamBlob) is deliberately excluded: its duration
// is bounded by the client's download speed, not by git.
const ReadTimeout = 30 * time.Second

// gitCmd builds a git subprocess rooted at args and bumps ForkCount.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	forkCount.Add(1)
	// gitDir is RepoFS-validated at every call site and every
	// user-controlled value is an argv element, never a shell string.
	return exec.CommandContext(ctx, "git", args...)
}

// readCtx derives a deadline-bounded context for a read-only git
// invocation. context.WithTimeout keeps the earlier of the two
// deadlines, so a caller with a tighter request deadline still wins.
// Callers must defer the returned cancel.
func readCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ReadTimeout)
}
