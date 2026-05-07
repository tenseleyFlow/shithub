// SPDX-License-Identifier: AGPL-3.0-or-later

package hooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/git/hooks"
)

func TestInstall_WritesExecutableShim(t *testing.T) {
	t.Parallel()
	gitDir := t.TempDir()
	bin := "/usr/local/bin/shithubd"

	if err := hooks.Install(gitDir, bin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, name := range hooks.SupportedHooks {
		path := filepath.Join(gitDir, "hooks", name)
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s: not executable, mode = %v", path, st.Mode())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		s := string(body)
		if !strings.Contains(s, "#!/bin/sh") {
			t.Errorf("%s: missing shebang", path)
		}
		if !strings.Contains(s, "hook "+name) {
			t.Errorf("%s: missing 'hook %s' subcommand reference", path, name)
		}
		if !strings.Contains(s, bin) {
			t.Errorf("%s: missing binary path %q", path, bin)
		}
	}
}

func TestInstall_Idempotent(t *testing.T) {
	t.Parallel()
	gitDir := t.TempDir()
	bin := "/usr/local/bin/shithubd"

	if err := hooks.Install(gitDir, bin); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := hooks.Install(gitDir, bin); err != nil {
		t.Fatalf("second Install: %v", err)
	}
}

func TestInstall_OverwritesStale(t *testing.T) {
	t.Parallel()
	gitDir := t.TempDir()
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(hooksDir, "pre-receive")
	if err := os.WriteFile(stale, []byte("#!/bin/sh\nold contents\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := hooks.Install(gitDir, "/usr/local/bin/shithubd-new"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "shithubd-new") {
		t.Errorf("stale hook not overwritten: %q", string(body))
	}
}
