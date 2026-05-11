// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FetchRemoteHeadsAndTags imports public heads and tags from remoteURL into
// gitDir without forcing local refs. It is intended for mirror/backfill flows:
// if a local branch or tag has diverged, git rejects the update instead of
// overwriting local history.
func FetchRemoteHeadsAndTags(ctx context.Context, gitDir, remoteURL string) error {
	return fetchRemoteHeadsAndTags(ctx, gitDir, remoteURL, "")
}

// FetchRemoteHeadsAndTagsWithToken is the authenticated variant used by
// GitHub imports for private repositories. The token is supplied through a
// short-lived askpass helper, not embedded in the remote URL or git argv.
func FetchRemoteHeadsAndTagsWithToken(ctx context.Context, gitDir, remoteURL, token string) error {
	return fetchRemoteHeadsAndTags(ctx, gitDir, remoteURL, strings.TrimSpace(token))
}

func fetchRemoteHeadsAndTags(ctx context.Context, gitDir, remoteURL, token string) error {
	if gitDir == "" {
		return errors.New("git fetch: gitDir is required")
	}
	if strings.TrimSpace(remoteURL) == "" {
		return errors.New("git fetch: remoteURL is required")
	}
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_XDG=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	if token != "" {
		askpass, cleanup, err := writeAskpass(token)
		if err != nil {
			return err
		}
		defer cleanup()
		env = append(env, "GIT_ASKPASS="+askpass)
	}
	//nolint:gosec // G204: gitDir is RepoFS-derived at call sites; remoteURL is caller-allowlisted and passed as argv, not shell.
	cmd := exec.CommandContext(ctx, "git",
		"-c", "protocol.ext.allow=never",
		"-C", gitDir,
		"fetch",
		"--quiet",
		"--no-recurse-submodules",
		remoteURL,
		"refs/heads/*:refs/heads/*",
		"refs/tags/*:refs/tags/*",
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch remote refs: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeAskpass(token string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "shithub-git-askpass-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("git fetch: askpass tempdir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "askpass.sh")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"*Username*) printf '%s\\n' 'x-access-token' ;;\n" +
		"*Password*) printf '%s\\n' " + shellQuote(token) + " ;;\n" +
		"*) printf '\\n' ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git fetch: askpass write: %w", err)
	}
	return path, cleanup, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
