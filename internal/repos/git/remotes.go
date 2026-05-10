// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// FetchRemoteHeadsAndTags imports public heads and tags from remoteURL into
// gitDir without forcing local refs. It is intended for mirror/backfill flows:
// if a local branch or tag has diverged, git rejects the update instead of
// overwriting local history.
func FetchRemoteHeadsAndTags(ctx context.Context, gitDir, remoteURL string) error {
	if gitDir == "" {
		return errors.New("git fetch: gitDir is required")
	}
	if strings.TrimSpace(remoteURL) == "" {
		return errors.New("git fetch: remoteURL is required")
	}
	//nolint:gosec // G204: gitDir is RepoFS-derived at call sites; remoteURL is caller-allowlisted and passed as argv, not shell.
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"fetch",
		"--quiet",
		"--no-recurse-submodules",
		remoteURL,
		"refs/heads/*:refs/heads/*",
		"refs/tags/*:refs/tags/*",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch remote refs: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
