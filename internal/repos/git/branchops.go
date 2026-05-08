// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// AheadBehind returns the number of commits unique to head (ahead)
// and unique to base (behind), computed via
// `git rev-list --left-right --count base...head`. Output shape is
// "<behind>\t<ahead>" — the left side is base, right is head.
//
// When base or head doesn't exist on the repo we surface the typed
// ErrRefNotFound so callers can render "—" instead of a number.
func AheadBehind(ctx context.Context, gitDir, base, head string) (ahead, behind int, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"rev-list", "--left-right", "--count", base+"..."+head)
	out, runErr := cmd.Output()
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			stderr := string(ee.Stderr)
			if strings.Contains(stderr, "unknown revision") || strings.Contains(stderr, "ambiguous argument") {
				return 0, 0, ErrRefNotFound
			}
		}
		return 0, 0, wrapExecErr(runErr)
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("rev-list: unexpected output %q", out)
	}
	behind, _ = strconv.Atoi(parts[0])
	ahead, _ = strconv.Atoi(parts[1])
	return ahead, behind, nil
}

// CommitsBetween returns the commits unique to head (the right side
// of the symmetric range). Used by the compare view's commits list.
func CommitsBetween(ctx context.Context, gitDir, base, head string, max int) ([]Commit, error) {
	if max <= 0 {
		max = 250
	}
	const sep = "\x1f"
	const recordEnd = "\x1e"
	format := strings.Join([]string{"%H", "%h", "%an", "%ae", "%at", "%s"}, sep) + sep + "%b" + recordEnd
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"log",
		"--max-count="+strconv.Itoa(max),
		"--format="+format,
		base+".."+head,
	)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "unknown revision") {
			return nil, ErrRefNotFound
		}
		return nil, wrapExecErr(err)
	}
	return parseLogOutput(out)
}

// IsAncestor reports whether commit a is an ancestor of commit b.
// Used by the pre-receive force-push detector: a fast-forward is
// `IsAncestor(old, new)`.
func IsAncestor(ctx context.Context, gitDir, a, b string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"merge-base", "--is-ancestor", a, b)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// Exit 1 = not an ancestor. Anything else = real error.
		if ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, wrapExecErr(err)
}

// SetSymbolicRef updates HEAD (or any other symbolic ref) atomically.
// Used by the default-branch change to point HEAD at the new branch.
func SetSymbolicRef(ctx context.Context, gitDir, ref, target string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"symbolic-ref", ref, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("symbolic-ref %s -> %s: %w (%s)", ref, target, err, out)
	}
	return nil
}

// ErrRefNotFound is returned when git can't resolve a ref or commit.
// Distinguished from generic exec failures so handlers can render a
// 404-leaning response.
var ErrRefNotFound = errors.New("git: ref not found")
