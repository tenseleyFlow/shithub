// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
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
