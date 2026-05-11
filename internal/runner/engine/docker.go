// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrUnsupportedUses = errors.New("runner engine: unsupported uses step")
	ErrUnsupported     = errors.New("runner engine: unsupported operation")
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type DockerConfig struct {
	Binary       string
	DefaultImage string
	Network      string
	Memory       string
	CPUs         string
	Stdout       io.Writer
	Stderr       io.Writer
	Runner       CommandRunner
}

type Docker struct {
	cfg DockerConfig
}

func NewDocker(cfg DockerConfig) *Docker {
	if cfg.Binary == "" {
		cfg.Binary = "docker"
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.Runner == nil {
		cfg.Runner = ExecRunner{}
	}
	return &Docker{cfg: cfg}
}

func (d *Docker) Execute(ctx context.Context, job Job) (Outcome, error) {
	started := time.Now().UTC()
	outcome := Outcome{Conclusion: ConclusionSuccess, StartedAt: started}
	if job.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(job.TimeoutMinutes)*time.Minute)
		defer cancel()
	}
	if err := os.MkdirAll(job.WorkspaceDir, 0o700); err != nil {
		outcome.Conclusion = ConclusionFailure
		outcome.CompletedAt = time.Now().UTC()
		return outcome, fmt.Errorf("runner engine: prepare workspace: %w", err)
	}
	for _, step := range job.Steps {
		if err := d.executeStep(ctx, job, step); err != nil {
			if step.ContinueOnError {
				continue
			}
			outcome.Conclusion = conclusionForError(err)
			outcome.CompletedAt = time.Now().UTC()
			return outcome, err
		}
	}
	outcome.CompletedAt = time.Now().UTC()
	return outcome, nil
}

func (d *Docker) executeStep(ctx context.Context, job Job, step Step) error {
	if strings.TrimSpace(step.Uses) != "" {
		return fmt.Errorf("%w: %s is not executable until checkout/artifact support lands", ErrUnsupportedUses, step.Uses)
	}
	if strings.TrimSpace(step.Run) == "" {
		return nil
	}
	args, err := d.dockerArgs(job, step)
	if err != nil {
		return err
	}
	if err := d.cfg.Runner.Run(ctx, d.cfg.Binary, args, d.cfg.Stdout, d.cfg.Stderr); err != nil {
		return fmt.Errorf("runner engine: step %q failed: %w", stepLabel(step), err)
	}
	return nil
}

func (d *Docker) dockerArgs(job Job, step Step) ([]string, error) {
	workdir, err := containerWorkdir(step.WorkingDirectory)
	if err != nil {
		return nil, err
	}
	image := strings.TrimSpace(job.Image)
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return nil, errors.New("runner engine: image is required")
	}
	args := []string{
		"run",
		"--rm",
		"--network=" + d.cfg.Network,
		"--memory=" + d.cfg.Memory,
		"--cpus=" + d.cfg.CPUs,
		"--workdir=" + workdir,
		"-v", job.WorkspaceDir + ":/workspace",
	}
	env, err := mergeEnv(job.Env, step.Env)
	if err != nil {
		return nil, err
	}
	for _, key := range sortedKeys(env) {
		args = append(args, "-e", key+"="+env[key])
	}
	args = append(args, image, "bash", "-c", step.Run)
	return args, nil
}

func (d *Docker) StreamLogs(_ context.Context, _ int64) (<-chan LogChunk, error) {
	ch := make(chan LogChunk)
	close(ch)
	return ch, nil
}

func (d *Docker) Cancel(_ context.Context, _ int64) error {
	return ErrUnsupported
}

func conclusionForError(err error) string {
	if errors.Is(err, context.Canceled) {
		return ConclusionCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ConclusionTimedOut
	}
	return ConclusionFailure
}

func containerWorkdir(wd string) (string, error) {
	wd = strings.TrimSpace(wd)
	if wd == "" {
		return "/workspace", nil
	}
	if strings.HasPrefix(wd, "/") {
		return "", fmt.Errorf("runner engine: working-directory must be relative, got %q", wd)
	}
	clean := path.Clean("/workspace/" + wd)
	if clean != "/workspace" && !strings.HasPrefix(clean, "/workspace/") {
		return "", fmt.Errorf("runner engine: working-directory escapes workspace: %q", wd)
	}
	return clean, nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mergeEnv(jobEnv, stepEnv map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(jobEnv)+len(stepEnv))
	for k, v := range jobEnv {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("runner engine: invalid env name %q", k)
		}
		out[k] = v
	}
	for k, v := range stepEnv {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("runner engine: invalid env name %q", k)
		}
		out[k] = v
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stepLabel(step Step) string {
	if step.Name != "" {
		return step.Name
	}
	if step.StepID != "" {
		return step.StepID
	}
	return fmt.Sprintf("#%d", step.Index)
}
