// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// stderrCap bounds the in-memory stderr drainer's buffer so a chatty
// subprocess can't OOM us. Truncated stderr is fine — we only surface
// it on error and a few KB is enough to identify the failure.
const stderrCap = 16 * 1024

// Service identifies which git plumbing program we're driving.
type Service string

const (
	UploadPack  Service = "git-upload-pack"
	ReceivePack Service = "git-receive-pack"
)

// Cmd builds an *exec.Cmd that drives the requested service against the
// bare repo at gitDir. AdvertiseRefs flips on the --advertise-refs flag
// used by the info/refs response. extraEnv is appended to os.Environ()
// — the caller uses it to thread SHITHUB_USER_ID/etc. through git's
// environment so post-receive hooks can read them.
//
// Subprocess lifecycle:
//   - Killed on ctx cancel (Go ≥ 1.20: cmd.Cancel default kills with
//     SIGKILL; we tighten to a 250ms WaitDelay so a stuck subprocess
//     doesn't pin a worker forever).
//   - Stderr is drained by the helper Drain() so the OS pipe buffer
//     doesn't fill and deadlock the caller's stdout copy. Captured
//     stderr is exposed via Stderr() on the returned Process.
func Cmd(ctx context.Context, svc Service, gitDir string, advertiseRefs bool, extraEnv []string) *exec.Cmd {
	args := []string{"--stateless-rpc"}
	if advertiseRefs {
		args = append(args, "--advertise-refs")
	}
	args = append(args, gitDir)
	//nolint:gosec // G204: svc is an enum from a fixed set; gitDir is constructed via storage.RepoFS path validation.
	cmd := exec.CommandContext(ctx, string(svc), args...)
	// Append, don't replace. The pre-receive / post-receive hooks
	// re-invoke shithubd, which reads SHITHUB_DATABASE_URL etc. from
	// the environment via config.Load. Without inheriting os.Environ
	// the hook fails with "DB URL not set" on every push. Later
	// entries win in Go's exec.Cmd, so the SHITHUB_USER_ID / etc.
	// values in extraEnv override any conflicting parent-env keys.
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd
}

// DrainStderr starts a goroutine that copies stderr into a bounded ring
// buffer; returns a function the caller invokes after Wait() to get the
// captured bytes. The cap is `stderrCap`; further writes are silently
// discarded.
//
// Always call this BEFORE Start() and call the returned closure AFTER
// Wait() to avoid the deadlock where a verbose stderr fills the OS
// pipe.
func DrainStderr(cmd *exec.Cmd) func() []byte {
	pipe, err := cmd.StderrPipe()
	if err != nil {
		// Stderr already wired (or another wiring error). Fall back to
		// a no-op drain so callers don't have to special-case.
		return func() []byte { return nil }
	}
	buf := &cappedBuffer{cap: stderrCap}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(buf, pipe)
	}()
	return func() []byte {
		<-done
		return buf.Bytes()
	}
}

// cappedBuffer is a bytes.Buffer that drops writes after the cap.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	free := c.cap - c.buf.Len()
	if free <= 0 {
		return len(p), nil // accept-and-drop
	}
	if len(p) > free {
		_, _ = c.buf.Write(p[:free])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out
}

// ErrSubprocessFailed is returned by Run when the git subprocess exits
// non-zero. The captured stderr is wrapped via %s so the operator sees
// it in logs without it being escaped through the chain.
var ErrSubprocessFailed = errors.New("git subprocess exited non-zero")
