// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
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

const (
	defaultSeccompProfile = "/etc/shithubd-runner/seccomp.json"
	defaultContainerUser  = "65534:65534"
	defaultPidsLimit      = 512
	defaultNofileLimit    = "4096:4096"
	defaultNprocLimit     = "512:512"

	// rootPermissionKey is an intentionally shithub-specific escape hatch.
	// It requires an explicit per-job permissions entry rather than treating
	// broad write-all permissions as permission to run the container as root.
	rootPermissionKey = "shithub-runner-root"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, env []string, stdout, stderr io.Writer) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type DockerConfig struct {
	Binary           string
	GitBinary        string
	DefaultImage     string
	Network          string
	Memory           string
	CPUs             string
	SeccompProfile   string
	User             string
	PidsLimit        int
	DNSServers       []string
	LogChunkBytes    int
	LogFlushInterval time.Duration
	StepLogLimit     int64
	TimeoutMinute    time.Duration
	Stdout           io.Writer
	Stderr           io.Writer
	Runner           CommandRunner
	MaskValues       []string
	AllowRoot        bool
	Logger           *slog.Logger
}

type Docker struct {
	cfg       DockerConfig
	streams   map[int64]chan LogChunk
	eventSubs map[int64]chan Event
	active    map[int64]string
	mu        sync.Mutex
}

func NewDocker(cfg DockerConfig) *Docker {
	if cfg.Binary == "" {
		cfg.Binary = "docker"
	}
	if cfg.GitBinary == "" {
		cfg.GitBinary = "git"
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
	if cfg.TimeoutMinute <= 0 {
		cfg.TimeoutMinute = time.Minute
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
	if cfg.SeccompProfile == "" {
		cfg.SeccompProfile = defaultSeccompProfile
	}
	if cfg.User == "" {
		cfg.User = defaultContainerUser
	}
	if cfg.PidsLimit <= 0 {
		cfg.PidsLimit = defaultPidsLimit
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Docker{
		cfg:       cfg,
		streams:   make(map[int64]chan LogChunk),
		eventSubs: make(map[int64]chan Event),
		active:    make(map[int64]string),
	}
}

func (d *Docker) Execute(ctx context.Context, job Job) (Outcome, error) {
	started := time.Now().UTC()
	outcome := Outcome{Conclusion: ConclusionSuccess, StartedAt: started}
	defer d.closeStream(job.ID)
	defer d.closeEventStream(job.ID)
	if job.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, time.Duration(job.TimeoutMinutes)*d.cfg.TimeoutMinute, ErrJobTimedOut)
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
			if emitErr := d.emitStepOutcomeAfterRun(ctx, job.ID, stepOutcome); emitErr != nil {
				outcome.Conclusion = conclusionForError(emitErr)
				outcome.CompletedAt = time.Now().UTC()
				return outcome, emitErr
			}
			if step.ContinueOnError && !errors.Is(err, ErrJobTimedOut) {
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
		if err := d.emitStepOutcomeAfterRun(ctx, job.ID, stepOutcome); err != nil {
			outcome.Conclusion = conclusionForError(err)
			outcome.CompletedAt = time.Now().UTC()
			return outcome, err
		}
	}
	outcome.CompletedAt = time.Now().UTC()
	return outcome, nil
}

func (d *Docker) executeStep(ctx context.Context, job Job, step Step) error {
	if uses := strings.TrimSpace(step.Uses); uses != "" {
		switch uses {
		case "actions/checkout@v4":
			return d.executeCheckout(ctx, job, step)
		default:
			return fmt.Errorf("%w: %s is not executable until that alias lands", ErrUnsupportedUses, uses)
		}
	}
	if strings.TrimSpace(step.Run) == "" {
		return nil
	}
	invocation, err := d.dockerInvocation(job, step)
	if err != nil {
		return err
	}
	d.setActiveContainer(job.ID, invocation.containerName)
	defer d.clearActiveContainer(job.ID, invocation.containerName)
	d.logStep(ctx, "runner step starting", job, step, invocation, "")
	writer := d.newStepLogWriter(ctx, job.ID, step.ID, job.MaskValues)
	out := io.MultiWriter(d.cfg.Stdout, writer)
	errOut := io.MultiWriter(d.cfg.Stderr, writer)
	if err := d.cfg.Runner.Run(ctx, d.cfg.Binary, invocation.args, invocation.env, out, errOut); err != nil {
		if isJobTimeout(ctx, err) {
			killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			killErr := d.killContainer(killCtx, invocation.containerName)
			cancel()
			if killErr != nil {
				err = errors.Join(err, killErr)
			}
			err = fmt.Errorf("%w: %w", ErrJobTimedOut, err)
		}
		d.logStep(ctx, "runner step completed", job, step, invocation, conclusionForError(err))
		if closeErr := writer.Close(); closeErr != nil {
			return fmt.Errorf("runner engine: step %q failed: %w", stepLabel(step), errors.Join(err, closeErr))
		}
		return fmt.Errorf("runner engine: step %q failed: %w", stepLabel(step), err)
	}
	d.logStep(ctx, "runner step completed", job, step, invocation, ConclusionSuccess)
	if err := writer.Close(); err != nil {
		return fmt.Errorf("runner engine: flush step %q logs: %w", stepLabel(step), err)
	}
	return nil
}

func (d *Docker) executeCheckout(ctx context.Context, job Job, step Step) error {
	checkoutURL, err := validateCheckoutURL(job.CheckoutURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(job.CheckoutToken) == "" {
		return errors.New("runner engine: checkout token is required")
	}
	headSHA := strings.TrimSpace(job.HeadSHA)
	if !gitObjectIDRE.MatchString(headSHA) {
		return fmt.Errorf("runner engine: invalid checkout sha %q", headSHA)
	}
	if err := os.MkdirAll(job.WorkspaceDir, 0o700); err != nil {
		return fmt.Errorf("runner engine: prepare checkout workspace: %w", err)
	}
	depthArgs, err := checkoutDepthArgs(step.With)
	if err != nil {
		return err
	}
	fetchTarget := headSHA

	writer := d.newStepLogWriter(ctx, job.ID, step.ID, job.MaskValues)
	out := io.MultiWriter(d.cfg.Stdout, writer)
	errOut := io.MultiWriter(d.cfg.Stderr, writer)
	closeWriter := func(runErr error) error {
		if closeErr := writer.Close(); closeErr != nil {
			if runErr != nil {
				return errors.Join(runErr, closeErr)
			}
			return closeErr
		}
		return runErr
	}

	fmt.Fprintf(out, "Checking out %s at %s\n", checkoutURL, shortObjectID(headSHA))
	gitEnv := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SHITHUB_CHECKOUT_TOKEN=" + job.CheckoutToken,
	}
	//nolint:gosec // G101: this helper reads the token from env; it does not hard-code or argv-expose a secret.
	credentialHelper := `credential.helper=!f() { echo username=shithub-actions; echo password=$SHITHUB_CHECKOUT_TOKEN; }; f`
	runGit := func(args ...string) error {
		if err := d.cfg.Runner.Run(ctx, d.cfg.GitBinary, args, gitEnv, out, errOut); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(redactCheckoutArgs(args), " "), err)
		}
		return nil
	}

	if err := runGit("-C", job.WorkspaceDir, "init"); err != nil {
		return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: %w", stepLabel(step), err))
	}
	if err := runGit("-C", job.WorkspaceDir, "remote", "add", "origin", checkoutURL); err != nil {
		return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: %w", stepLabel(step), err))
	}
	fetchArgs := []string{"-C", job.WorkspaceDir, "-c", credentialHelper, "fetch", "--no-tags"}
	fetchArgs = append(fetchArgs, depthArgs...)
	fetchArgs = append(fetchArgs, "origin", fetchTarget)
	if err := runGit(fetchArgs...); err != nil {
		if ref, ok := checkoutFallbackRef(job.HeadRef); ok {
			fmt.Fprintf(out, "Exact SHA fetch failed; fetching %s to resolve queued commit\n", ref)
			fallbackArgs := []string{"-C", job.WorkspaceDir, "-c", credentialHelper, "fetch", "--no-tags", "origin", ref}
			if fallbackErr := runGit(fallbackArgs...); fallbackErr != nil {
				return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: %w", stepLabel(step), errors.Join(err, fallbackErr)))
			}
		} else {
			return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: %w", stepLabel(step), err))
		}
	}
	if err := runGit("-C", job.WorkspaceDir, "checkout", "--force", "--detach", headSHA); err != nil {
		return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: %w", stepLabel(step), err))
	}

	var rev bytes.Buffer
	if err := d.cfg.Runner.Run(ctx, d.cfg.GitBinary,
		[]string{"-C", job.WorkspaceDir, "rev-parse", "HEAD"},
		gitEnv, io.MultiWriter(out, &rev), errOut); err != nil {
		return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: git rev-parse HEAD: %w", stepLabel(step), err))
	}
	if got := strings.TrimSpace(rev.String()); !strings.EqualFold(got, headSHA) {
		return closeWriter(fmt.Errorf("runner engine: checkout step %q failed: HEAD %s != expected %s", stepLabel(step), got, headSHA))
	}
	fmt.Fprintf(out, "Checked out %s\n", shortObjectID(headSHA))
	return closeWriter(nil)
}

type dockerInvocation struct {
	args           []string
	env            []string
	containerName  string
	image          string
	network        string
	memory         string
	cpus           string
	user           string
	seccompProfile string
	pidsLimit      int
}

func (d *Docker) dockerInvocation(job Job, step Step) (dockerInvocation, error) {
	workdir, err := containerWorkdir(step.WorkingDirectory)
	if err != nil {
		return dockerInvocation{}, err
	}
	image := strings.TrimSpace(job.Image)
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return dockerInvocation{}, errors.New("runner engine: image is required")
	}
	rendered, err := runnerexec.RenderStep(runnerexec.StepInput{
		Run:     step.Run,
		JobEnv:  job.Env,
		StepEnv: step.Env,
		Context: expressionContext(job),
	})
	if err != nil {
		return dockerInvocation{}, fmt.Errorf("runner engine: render step %q: %w", stepLabel(step), err)
	}
	user := d.cfg.User
	if d.cfg.AllowRoot && permissionsRequestRoot(job.Permissions) {
		user = "0:0"
	}
	containerName := dockerContainerName(job, step)
	args := []string{
		"run",
		"--rm",
		"--name", containerName,
		"--network=" + d.cfg.Network,
		"--memory=" + d.cfg.Memory,
		"--cpus=" + d.cfg.CPUs,
		"--pids-limit=" + strconv.Itoa(d.cfg.PidsLimit),
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=1g",
		"--cap-drop=ALL",
		"--cap-add=DAC_OVERRIDE",
		"--cap-add=SETGID",
		"--cap-add=SETUID",
		"--security-opt=no-new-privileges",
		"--security-opt=seccomp=" + d.cfg.SeccompProfile,
		"--ulimit", "nofile=" + defaultNofileLimit,
		"--ulimit", "nproc=" + defaultNprocLimit,
		"--user", user,
		"--workdir=" + workdir,
		"--mount", "type=bind,src=" + job.WorkspaceDir + ",dst=/workspace",
	}
	for _, dns := range d.cfg.DNSServers {
		dns = strings.TrimSpace(dns)
		if dns != "" {
			args = append(args, "--dns", dns)
		}
	}
	env, err := validateEnv(rendered.Env)
	if err != nil {
		return dockerInvocation{}, err
	}
	processEnv := make([]string, 0, len(env))
	for _, key := range sortedKeys(env) {
		args = append(args, "--env", key)
		processEnv = append(processEnv, key+"="+env[key])
	}
	args = append(args, image, "bash", "-c", rendered.Run)
	return dockerInvocation{
		args:           args,
		env:            processEnv,
		containerName:  containerName,
		image:          image,
		network:        d.cfg.Network,
		memory:         d.cfg.Memory,
		cpus:           d.cfg.CPUs,
		user:           user,
		seccompProfile: d.cfg.SeccompProfile,
		pidsLimit:      d.cfg.PidsLimit,
	}, nil
}

func (d *Docker) logStep(ctx context.Context, msg string, job Job, step Step, invocation dockerInvocation, conclusion string) {
	attrs := []any{
		"run_id", job.RunID,
		"job_id", job.ID,
		"step_id", step.ID,
		"image", invocation.image,
		"network", invocation.network,
		"cpu_limit", invocation.cpus,
		"memory_limit", invocation.memory,
		"pids_limit", invocation.pidsLimit,
		"container_user", invocation.user,
		"seccomp_profile", invocation.seccompProfile,
	}
	if conclusion != "" {
		attrs = append(attrs, "conclusion", conclusion)
	}
	d.cfg.Logger.InfoContext(ctx, msg, attrs...)
}

func expressionContext(job Job) expr.Context {
	event := job.EventPayload
	if len(event) == 0 && strings.TrimSpace(job.Event) != "" && json.Valid([]byte(job.Event)) {
		_ = json.Unmarshal([]byte(job.Event), &event)
	}
	return expr.Context{
		Secrets: job.Secrets,
		Shithub: expr.ShithubContext{
			Event: event,
			RunID: fmt.Sprintf("%d", job.RunID),
			SHA:   job.HeadSHA,
			Ref:   job.HeadRef,
		},
		Untrusted: expr.DefaultUntrusted(),
	}
}

func permissionsRequestRoot(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var shaped struct {
		Per map[string]string `json:"per"`
	}
	if err := json.Unmarshal(raw, &shaped); err == nil && strings.EqualFold(shaped.Per[rootPermissionKey], "write") {
		return true
	}
	var flat map[string]string
	if err := json.Unmarshal(raw, &flat); err != nil {
		return false
	}
	return strings.EqualFold(flat[rootPermissionKey], "write")
}

var gitObjectIDRE = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)

func validateCheckoutURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("runner engine: checkout url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("runner engine: invalid checkout url %q", raw)
	}
	if u.User != nil {
		return "", errors.New("runner engine: checkout url must not contain credentials")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("runner engine: checkout url scheme %q is not allowed", u.Scheme)
	}
	return u.String(), nil
}

func checkoutDepthArgs(with map[string]string) ([]string, error) {
	for key := range with {
		if key != "fetch-depth" {
			return nil, fmt.Errorf("runner engine: unsupported checkout input %q", key)
		}
	}
	raw := strings.TrimSpace(with["fetch-depth"])
	if raw == "" {
		return []string{"--depth=1"}, nil
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 0 || depth > 100000 {
		return nil, fmt.Errorf("runner engine: invalid checkout fetch-depth %q", raw)
	}
	if depth == 0 {
		return nil, nil
	}
	return []string{"--depth=" + strconv.Itoa(depth)}, nil
}

func checkoutFallbackRef(headRef string) (string, bool) {
	ref := strings.TrimSpace(headRef)
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
	case strings.HasPrefix(ref, "refs/tags/"):
	default:
		return "", false
	}
	if strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") ||
		strings.ContainsAny(ref, " \t\r\n:~^?*[\\") {
		return "", false
	}
	return ref, true
}

func shortObjectID(oid string) string {
	if len(oid) <= 12 {
		return oid
	}
	return oid[:12]
}

func redactCheckoutArgs(args []string) []string {
	out := append([]string{}, args...)
	for i, arg := range out {
		if strings.Contains(arg, "SHITHUB_CHECKOUT_TOKEN") {
			out[i] = "credential.helper=<redacted>"
		}
	}
	return out
}

func (d *Docker) StreamLogs(_ context.Context, jobID int64) (<-chan LogChunk, error) {
	return d.ensureStream(jobID), nil
}

func (d *Docker) StreamEvents(_ context.Context, jobID int64) (<-chan Event, error) {
	return d.ensureEventStream(jobID), nil
}

func (d *Docker) Cancel(ctx context.Context, jobID int64) error {
	name := d.activeContainer(jobID)
	if name == "" {
		return nil
	}
	killCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.killContainer(killCtx, name)
}

func (d *Docker) killContainer(ctx context.Context, name string) error {
	if err := d.cfg.Runner.Run(ctx, d.cfg.Binary, []string{"kill", name}, nil, d.cfg.Stdout, d.cfg.Stderr); err != nil {
		return fmt.Errorf("runner engine: kill container %s: %w", name, err)
	}
	return nil
}

func (d *Docker) setActiveContainer(jobID int64, name string) {
	if name == "" {
		return
	}
	d.mu.Lock()
	d.active[jobID] = name
	d.mu.Unlock()
}

func (d *Docker) clearActiveContainer(jobID int64, name string) {
	d.mu.Lock()
	if d.active[jobID] == name {
		delete(d.active, jobID)
	}
	d.mu.Unlock()
}

func (d *Docker) activeContainer(jobID int64) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active[jobID]
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

func (d *Docker) emitStepOutcomeAfterRun(ctx context.Context, jobID int64, step StepOutcome) error {
	if ctx.Err() == nil {
		return d.emitStepOutcome(ctx, jobID, step)
	}
	emitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return d.emitStepOutcome(emitCtx, jobID, step)
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
	if errors.Is(err, ErrJobTimedOut) {
		return ConclusionTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return ConclusionCancelled
	}
	return ConclusionFailure
}

func isJobTimeout(ctx context.Context, err error) bool {
	if errors.Is(err, ErrJobTimedOut) {
		return true
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(context.Cause(ctx), ErrJobTimedOut)
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

func dockerContainerName(job Job, step Step) string {
	stepID := step.ID
	if stepID == 0 {
		stepID = int64(step.Index)
	}
	return fmt.Sprintf("shithub-job-%d-step-%d", job.ID, stepID)
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnv(env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("runner engine: invalid env name %q", k)
		}
		if strings.ContainsRune(v, '\x00') {
			return nil, fmt.Errorf("runner engine: invalid env value for %q", k)
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
