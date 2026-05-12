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
	cmd := exec.CommandContext(
		ctx, "git", "-C", gitDir,
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

// UpdateRefCAS performs an atomic compare-and-swap on a ref: only
// succeeds if the ref currently points at oldOID. Returns
// ErrRefRaced when the ref moved underneath us (a concurrent push
// is the canonical case). Used by S27's sync-fork to fast-forward
// the fork's default branch only when nothing else has touched it.
//
// The git update-ref `<oldvalue>` argument is what enforces the CAS;
// passing it gives git's exact-match semantics (no off-by-one
// races even when oldvalue happens to equal newvalue).
func UpdateRefCAS(ctx context.Context, gitDir, ref, newOID, oldOID string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"update-ref", ref, newOID, oldOID)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// git update-ref's "ref-changed-from-under-us" failure mode is
	// signalled via stderr text rather than a distinct exit code.
	// The two phrasings we care about: "old value is %s, but expected"
	// and "cannot lock ref" — both indicate the CAS lost the race.
	s := string(out)
	if strings.Contains(s, "old value") || strings.Contains(s, "cannot lock ref") {
		return ErrRefRaced
	}
	return fmt.Errorf("update-ref %s %s..%s: %w (%s)", ref, oldOID, newOID, err, strings.TrimSpace(s))
}

// ErrRefRaced is the typed sentinel UpdateRefCAS returns when the
// ref moved between our read and our update.
var ErrRefRaced = errors.New("repogit: ref moved concurrently")

// DeleteBranch removes refs/heads/<branch>. When oldOID is non-empty,
// git's update-ref compare-and-delete guard is used so a branch that
// moved after the caller rendered the page is not deleted accidentally.
func DeleteBranch(ctx context.Context, gitDir, branch, oldOID string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || strings.HasPrefix(branch, "-") {
		return ErrRefNotFound
	}
	check := exec.CommandContext(ctx, "git", "-C", gitDir, "check-ref-format", "--branch", branch)
	if out, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("check-ref-format %s: %w (%s)", branch, err, strings.TrimSpace(string(out)))
	}
	ref := "refs/heads/" + branch
	args := []string{"-C", gitDir, "update-ref", "-d", ref}
	if strings.TrimSpace(oldOID) != "" {
		args = append(args, oldOID)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	switch {
	case strings.Contains(msg, "unable to resolve reference"), strings.Contains(msg, "reference does not exist"):
		return ErrRefNotFound
	case strings.Contains(msg, "cannot lock ref"), strings.Contains(msg, "expected"):
		return ErrRefRaced
	default:
		return fmt.Errorf("delete-ref %s: %w (%s)", ref, err, msg)
	}
}

// FetchIntoNamespace fetches a single ref from `srcRepoDir` into
// `dstRepoDir` under the supplied refspec. The dst-side ref name is
// the second half of the refspec. Used by S27 cross-fork PR support
// to pull a fork's head branch into the base repo's
// `refs/shithub-pr/<pr_id>/head` namespace (private — never
// advertised via `info/refs`).
//
// Idempotent at the git layer; calling repeatedly with the same
// refspec just updates the dst ref.
func FetchIntoNamespace(ctx context.Context, dstRepoDir, srcRepoDir, srcRef, dstRef string) error {
	refspec := srcRef + ":" + dstRef
	cmd := exec.CommandContext(ctx, "git", "-C", dstRepoDir,
		"fetch", "--quiet", "--no-tags", srcRepoDir, refspec)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch %s into %s: %w (%s)", srcRef, dstRef, err, strings.TrimSpace(string(out)))
	}
	return nil
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
