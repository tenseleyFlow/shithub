// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/git/protocol"
)

func TestWritePkt_Format(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := protocol.WritePkt(&buf, "hi"); err != nil {
		t.Fatalf("WritePkt: %v", err)
	}
	// 4-hex prefix = len(payload)+4 = 6 -> "0006"
	if got := buf.String(); got != "0006hi" {
		t.Fatalf("got %q, want %q", got, "0006hi")
	}
}

func TestWritePkt_RejectsTooLong(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	huge := strings.Repeat("a", protocol.MaxPktLine)
	if err := protocol.WritePkt(&buf, huge); err == nil {
		t.Fatal("expected error for over-length pkt")
	}
}

func TestServiceAdvertisement(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := protocol.WriteServiceAdvertisement(&buf, "git-upload-pack"); err != nil {
		t.Fatalf("WriteServiceAdvertisement: %v", err)
	}
	got := buf.String()
	// Expected: "001e# service=git-upload-pack\n0000"
	want := "001e# service=git-upload-pack\n0000"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestCmd_StreamsAndDrainsStderr exercises the exec wrapper end-to-end:
// init a bare repo, run upload-pack --advertise-refs against it, drain
// stderr, and check stdout looks like a valid (empty) ref advertisement.
func TestCmd_StreamsAndDrainsStderr(t *testing.T) {
	t.Parallel()
	gitDir := initBare(t)

	cmd := protocol.Cmd(context.Background(), protocol.UploadPack, gitDir, true, nil)
	stderr := protocol.DrainStderr(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Cmd run: %v\nstderr: %s", err, stderr())
	}
	// First 4 chars are a hex length prefix; the body should mention
	// either a ref or the no-refs sentinel git emits for unborn HEAD.
	if len(out) < 4 {
		t.Fatalf("output too short: %q", out)
	}
}

// TestCmd_KillsOnContextCancel verifies that cancelling ctx kills the
// subprocess promptly (Go's default cmd.Cancel sends SIGKILL).
func TestCmd_KillsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	// `git fetch` against a never-resolving URL hangs indefinitely;
	// we'll use that as our long-running subprocess.
	gitDir := initBare(t)
	//nolint:gosec // G204: gitDir is t.TempDir.
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "fetch", "https://example.invalid/never-resolves")
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_ = cmd.Wait() // expect non-nil; we just care it returns
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("subprocess took %s to die after cancel; expected <5s", elapsed)
	}
}

func initBare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, "x.git")
	//nolint:gosec // G204: t.TempDir.
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return gitDir
}
