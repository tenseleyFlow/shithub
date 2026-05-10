// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"strings"
	"testing"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// TestBuildSSHEnv_InheritsParentEnv pins the contract that
// buildSSHEnv extends os.Environ() so SHITHUB_DATABASE_URL (and any
// other config var sourced by the git-shell-commands wrapper from
// /etc/shithub/web.env) propagates to the exec'd git binary, which
// in turn forks pre-receive / post-receive hooks that need it.
//
// Pre-fix: the function returned a minimal explicit list, so a
// `git push` over SSH made it through auth + dispatch + perms but
// crashed at the pre-receive hook with "DB URL not set". The HTTPS
// path didn't see this because shithubd-web's systemd unit sources
// web.env and receive-pack inherits it directly.
//
// This test does NOT need a database — buildSSHEnv has no side
// effects beyond constructing an env slice.
func TestBuildSSHEnv_InheritsParentEnv(t *testing.T) {
	t.Setenv("SHITHUB_DATABASE_URL", "postgres://test-marker/x")
	t.Setenv("SHITHUB_STORAGE__REPOS_ROOT", "/tmp/test-marker-repos")

	user := usersdb.User{ID: 7, Username: "alice"}
	repo := reposdb.Repo{ID: 42, Name: "rcal"}
	env := buildSSHEnv(user, "tenseleyflow", repo, "127.0.0.1", "req-deadbeef")
	blob := strings.Join(env, "\n")

	want := []string{
		// Inherited config (the regression):
		"SHITHUB_DATABASE_URL=postgres://test-marker/x",
		"SHITHUB_STORAGE__REPOS_ROOT=/tmp/test-marker-repos",
		// Push-event metadata appended after inheritance:
		"SHITHUB_USER_ID=7",
		"SHITHUB_USERNAME=alice",
		"SHITHUB_REPO_ID=42",
		"SHITHUB_REPO_FULL_NAME=tenseleyflow/rcal",
		"SHITHUB_PROTOCOL=ssh",
		"SHITHUB_REMOTE_IP=127.0.0.1",
		"SHITHUB_REQUEST_ID=req-deadbeef",
		// safe.directory plumbing intact:
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	}
	for _, w := range want {
		if !strings.Contains(blob, w) {
			t.Errorf("buildSSHEnv missing %q in:\n%s", w, blob)
		}
	}
}

// TestBuildSSHEnv_ExplicitOverridesInheritance pins that push-event
// metadata wins over an inherited variable of the same name. This
// matters if the wrapping process happened to set, e.g.,
// SHITHUB_REQUEST_ID — the dispatcher's value (per-push, generated
// here) should take precedence so logs aren't cross-contaminated.
func TestBuildSSHEnv_ExplicitOverridesInheritance(t *testing.T) {
	// Inject a stale SHITHUB_REQUEST_ID into the parent — buildSSHEnv
	// must append the real one AFTER os.Environ so Go's last-wins
	// behavior on duplicate env keys gives us the real value.
	t.Setenv("SHITHUB_REQUEST_ID", "stale-from-parent")

	user := usersdb.User{ID: 7, Username: "alice"}
	repo := reposdb.Repo{ID: 42, Name: "rcal"}
	env := buildSSHEnv(user, "tenseleyflow", repo, "127.0.0.1", "real-req-id")

	// Find both the stale and real entries; the real one MUST come
	// after the stale one (last-wins semantics in os/exec).
	staleAt, realAt := -1, -1
	for i, kv := range env {
		switch kv {
		case "SHITHUB_REQUEST_ID=stale-from-parent":
			staleAt = i
		case "SHITHUB_REQUEST_ID=real-req-id":
			realAt = i
		}
	}
	if realAt < 0 {
		t.Fatalf("buildSSHEnv missing the real SHITHUB_REQUEST_ID")
	}
	if staleAt >= 0 && realAt < staleAt {
		t.Fatalf("real SHITHUB_REQUEST_ID at idx %d but stale at %d — explicit must win", realAt, staleAt)
	}
}
