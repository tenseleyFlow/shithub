// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tenseleyFlow/shithub/internal/actions/expr"
	runnerexec "github.com/tenseleyFlow/shithub/internal/runner/exec"
	"github.com/tenseleyFlow/shithub/internal/runner/scrub"
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
	Binary           string
	DefaultImage     string
	Network          string
	Memory           string
	CPUs             string
	LogChunkBytes    int
	LogFlushInterval time.Duration
	StepLogLimit     int64
	Stdout           io.Writer
	Stderr           io.Writer
	Runner           CommandRunner
	MaskValues       []string
}

type Docker struct {
	cfg       DockerConfig
	streams   map[int64]chan LogChunk
	eventSubs map[int64]chan Event
	mu        sync.Mutex
}

func NewDocker(cfg DockerConfig) *Docker {
	if cfg.Binary == "" {
		cfg.Binary = "docker"
	}
	if cfg.LogChunkBytes <= 0 {
		cfg.LogChunkBytes = 4 * 1024
	}
	if cfg.LogFlushInterval <= 0 {
		cfg.LogFlushInterval = time.Second
	}
	if cfg.StepLogLimit <= 0 {
		cfg.StepLogLimit = 10 * 1024 * 1024
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
	return &Docker{cfg: cfg, streams: make(map[int64]chan LogChunk), eventSubs: make(map[int64]chan Event)}
}

func (d *Docker) Execute(ctx context.Context, job Job) (Outcome, error) {
	started := time.Now().UTC()
	outcome := Outcome{Conclusion: ConclusionSuccess, StartedAt: started}
	defer d.closeStream(job.ID)
	defer d.closeEventStream(job.ID)
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
		stepStarted := time.Now().UTC()
		if err := d.executeStep(ctx, job, step); err != nil {
			stepCompleted := time.Now().UTC()
			stepOutcome := StepOutcome{
				StepID:      step.ID,
				Status:      "completed",
				Conclusion:  conclusionForError(err),
				StartedAt:   stepStarted,
				CompletedAt: stepCompleted,
			}
			outcome.StepOutcomes = append(outcome.StepOutcomes, stepOutcome)
			if emitErr := d.emitStepOutcome(ctx, job.ID, stepOutcome); emitErr != nil {
				outcome.Conclusion = conclusionForError(emitErr)
				outcome.CompletedAt = time.Now().UTC()
				return outcome, emitErr
			}
			if step.ContinueOnError {
				continue
			}
			outcome.Conclusion = conclusionForError(err)
			outcome.CompletedAt = stepCompleted
			return outcome, err
		}
		stepOutcome := StepOutcome{
			StepID:      step.ID,
			Status:      "completed",
			Conclusion:  ConclusionSuccess,
			StartedAt:   stepStarted,
			CompletedAt: time.Now().UTC(),
		}
		outcome.StepOutcomes = append(outcome.StepOutcomes, stepOutcome)
		if err := d.emitStepOutcome(ctx, job.ID, stepOutcome); err != nil {
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
	writer := d.newStepLogWriter(ctx, job.ID, step.ID, job.MaskValues)
	out := io.MultiWriter(d.cfg.Stdout, writer)
	errOut := io.MultiWriter(d.cfg.Stderr, writer)
	if err := d.cfg.Runner.Run(ctx, d.cfg.Binary, args, out, errOut); err != nil {
		if closeErr := writer.Close(); closeErr != nil {
			return fmt.Errorf("runner engine: step %q failed: %w", stepLabel(step), errors.Join(err, closeErr))
		}
		return fmt.Errorf("runner engine: step %q failed: %w", stepLabel(step), err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("runner engine: flush step %q logs: %w", stepLabel(step), err)
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
	rendered, err := runnerexec.RenderStep(runnerexec.StepInput{
		Run:     step.Run,
		JobEnv:  job.Env,
		StepEnv: step.Env,
		Context: expressionContext(job),
	})
	if err != nil {
		return nil, fmt.Errorf("runner engine: render step %q: %w", stepLabel(step), err)
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
	env, err := validateEnv(rendered.Env)
	if err != nil {
		return nil, err
	}
	for _, key := range sortedKeys(env) {
		args = append(args, "-e", key+"="+env[key])
	}
	args = append(args, image, "bash", "-c", rendered.Run)
	return args, nil
}

func expressionContext(job Job) expr.Context {
	event := job.EventPayload
	if len(event) == 0 && strings.TrimSpace(job.Event) != "" && json.Valid([]byte(job.Event)) {
		_ = json.Unmarshal([]byte(job.Event), &event)
	}
	return expr.Context{
		Shithub: expr.ShithubContext{
			Event: event,
			RunID: fmt.Sprintf("%d", job.RunID),
			SHA:   job.HeadSHA,
			Ref:   job.HeadRef,
		},
		Untrusted: expr.DefaultUntrusted(),
	}
}

func (d *Docker) StreamLogs(_ context.Context, jobID int64) (<-chan LogChunk, error) {
	return d.ensureStream(jobID), nil
}

func (d *Docker) StreamEvents(_ context.Context, jobID int64) (<-chan Event, error) {
	return d.ensureEventStream(jobID), nil
}

func (d *Docker) Cancel(_ context.Context, _ int64) error {
	return ErrUnsupported
}

func (d *Docker) ensureStream(jobID int64) chan LogChunk {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.streams[jobID]; ok {
		return ch
	}
	ch := make(chan LogChunk, 128)
	d.streams[jobID] = ch
	return ch
}

func (d *Docker) closeStream(jobID int64) {
	d.mu.Lock()
	ch, ok := d.streams[jobID]
	if ok {
		delete(d.streams, jobID)
	}
	d.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (d *Docker) logStream(jobID int64) chan LogChunk {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streams[jobID]
}

func (d *Docker) ensureEventStream(jobID int64) chan Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.eventSubs[jobID]; ok {
		return ch
	}
	ch := make(chan Event, 128)
	d.eventSubs[jobID] = ch
	return ch
}

func (d *Docker) closeEventStream(jobID int64) {
	d.mu.Lock()
	ch, ok := d.eventSubs[jobID]
	if ok {
		delete(d.eventSubs, jobID)
	}
	d.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (d *Docker) eventStream(jobID int64) chan Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.eventSubs[jobID]
}

func (d *Docker) emitStepOutcome(ctx context.Context, jobID int64, step StepOutcome) error {
	ch := d.eventStream(jobID)
	if ch == nil {
		return nil
	}
	copied := step
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- Event{Step: &copied}:
		return nil
	}
}

func (d *Docker) newStepLogWriter(ctx context.Context, jobID, stepID int64, jobMasks []string) *stepLogWriter {
	w := &stepLogWriter{
		ctx:      ctx,
		ch:       d.logStream(jobID),
		events:   d.eventStream(jobID),
		jobID:    jobID,
		stepID:   stepID,
		maxChunk: d.cfg.LogChunkBytes,
		interval: d.cfg.LogFlushInterval,
		limit:    d.cfg.StepLogLimit,
		masker:   scrub.New(append(append([]string{}, d.cfg.MaskValues...), jobMasks...)),
		done:     make(chan struct{}),
	}
	go w.flushLoop()
	return w
}

type stepLogWriter struct {
	ctx       context.Context
	ch        chan<- LogChunk
	events    chan<- Event
	jobID     int64
	stepID    int64
	seq       int32
	maxChunk  int
	interval  time.Duration
	limit     int64
	written   int64
	truncated bool
	masker    *scrub.Scrubber
	buf       []byte
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	closed    bool
}

func (w *stepLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	w.appendWithinLimit(p)
	for len(w.buf) >= w.maxChunk {
		if err := w.emitLocked(w.buf[:w.maxChunk]); err != nil {
			return 0, err
		}
		w.buf = w.buf[w.maxChunk:]
	}
	return len(p), nil
}

func (w *stepLogWriter) Close() error {
	w.once.Do(func() {
		close(w.done)
		w.mu.Lock()
		defer w.mu.Unlock()
		_ = w.flushLocked()
		_ = w.flushMaskerLocked()
		w.closed = true
	})
	return nil
}

func (w *stepLogWriter) appendWithinLimit(p []byte) {
	if w.limit <= 0 {
		w.buf = append(w.buf, p...)
		return
	}
	remaining := w.limit - w.written
	if remaining > 0 {
		if int64(len(p)) <= remaining {
			w.buf = append(w.buf, p...)
			w.written += int64(len(p))
			return
		}
		w.buf = append(w.buf, p[:int(remaining)]...)
		w.written += remaining
	}
	if !w.truncated {
		w.buf = append(w.buf, []byte("\n[shithub-runner: step log truncated after 10 MiB]\n")...)
		w.truncated = true
	}
}

func (w *stepLogWriter) flushLoop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			_ = w.flushLocked()
			w.mu.Unlock()
		}
	}
}

func (w *stepLogWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	if err := w.emitLocked(w.buf); err != nil {
		return err
	}
	w.buf = nil
	return nil
}

func (w *stepLogWriter) emitLocked(chunk []byte) error {
	if w.masker != nil {
		chunk = w.masker.Scrub(chunk)
		if len(chunk) == 0 {
			return nil
		}
	}
	return w.emitChunkLocked(chunk)
}

func (w *stepLogWriter) flushMaskerLocked() error {
	if w.masker == nil {
		return nil
	}
	chunk := w.masker.Flush()
	if len(chunk) == 0 {
		return nil
	}
	return w.emitChunkLocked(chunk)
}

func (w *stepLogWriter) emitChunkLocked(chunk []byte) error {
	copied := LogChunk{JobID: w.jobID, StepID: w.stepID, Seq: w.seq, Chunk: append([]byte(nil), chunk...)}
	if w.ch != nil {
		select {
		case <-w.ctx.Done():
			return w.ctx.Err()
		case w.ch <- copied:
		}
	}
	if w.events != nil {
		eventChunk := copied
		select {
		case <-w.ctx.Done():
			return w.ctx.Err()
		case w.events <- Event{Log: &eventChunk}:
		}
	}
	w.seq++
	return nil
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

func validateEnv(env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
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
