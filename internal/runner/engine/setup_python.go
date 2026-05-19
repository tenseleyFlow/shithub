// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	setupPythonAlias       = "actions/setup-python@v5"
	setupPythonDefaultPath = "/bin:/usr/bin:/usr/local/bin"
)

var setupPythonVersionRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+|x))?$`)

type setupPythonPlan struct {
	Requested       string
	Family          string
	Exact           string
	Executable      string
	HostBinDir      string
	ContainerBinDir string
}

func (d *Docker) executeSetupPython(ctx context.Context, job *Job, step Step) error {
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

	plan, err := planSetupPython(job.WorkspaceDir, step.With)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return closeWriter(fmt.Errorf("runner engine: setup-python step %q failed: %w", stepLabel(step), err))
	}
	if err := prepareSetupPythonShims(plan); err != nil {
		_, _ = fmt.Fprintf(errOut, "prepare Python shims: %v\n", err)
		return closeWriter(fmt.Errorf("runner engine: setup-python step %q failed: %w", stepLabel(step), err))
	}
	invocation, err := d.setupPythonInvocation(*job, step, plan)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return closeWriter(fmt.Errorf("runner engine: setup-python step %q failed: %w", stepLabel(step), err))
	}

	d.setActiveContainer(job.ID, invocation.containerName)
	defer d.clearActiveContainer(job.ID, invocation.containerName)
	d.logStep(ctx, "runner first-party setup-python starting", *job, step, invocation, "")

	_, _ = fmt.Fprintf(out, "Setting up Python %s with %s\n", plan.Requested, setupPythonAlias)
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
		d.logStep(ctx, "runner first-party setup-python completed", *job, step, invocation, conclusionForError(err))
		return closeWriter(fmt.Errorf("runner engine: setup-python step %q failed: %w", stepLabel(step), err))
	}

	prependSetupPythonPath(job, plan.ContainerBinDir)
	_, _ = fmt.Fprintf(out, "Added %s to PATH for subsequent steps\n", plan.ContainerBinDir)
	d.logStep(ctx, "runner first-party setup-python completed", *job, step, invocation, ConclusionSuccess)
	return closeWriter(nil)
}

func planSetupPython(workspace string, with map[string]string) (setupPythonPlan, error) {
	for key := range with {
		switch key {
		case "python-version":
		case "cache":
			return setupPythonPlan{}, errors.New("actions/setup-python@v5 input \"cache\" is not supported yet")
		default:
			return setupPythonPlan{}, fmt.Errorf("actions/setup-python@v5 input %q is not supported", key)
		}
	}
	requested := strings.TrimSpace(with["python-version"])
	if requested == "" {
		return setupPythonPlan{}, errors.New("actions/setup-python@v5 requires with.python-version; .python-version files are not supported yet")
	}
	if strings.Contains(requested, "${{") {
		return setupPythonPlan{}, errors.New("actions/setup-python@v5 python-version expressions are not supported yet")
	}
	m := setupPythonVersionRE.FindStringSubmatch(requested)
	if m == nil {
		return setupPythonPlan{}, fmt.Errorf("actions/setup-python@v5 unsupported python-version %q; supported versions: 3.12", requested)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major != 3 || minor != 12 {
		return setupPythonPlan{}, fmt.Errorf("actions/setup-python@v5 python-version %q is not available on this runner; supported versions: 3.12", requested)
	}
	family := "3.12"
	exact := ""
	if patch := m[3]; patch != "" && patch != "x" {
		exact = family + "." + patch
	}
	hostBinDir := filepath.Join(workspace, ".shithub", "setup-python", family, "bin")
	return setupPythonPlan{
		Requested:       requested,
		Family:          family,
		Exact:           exact,
		Executable:      "/bin/python3.12",
		HostBinDir:      hostBinDir,
		ContainerBinDir: filepath.ToSlash(filepath.Join("/workspace", ".shithub", "setup-python", family, "bin")),
	}, nil
}

func prepareSetupPythonShims(plan setupPythonPlan) error {
	if err := os.MkdirAll(plan.HostBinDir, 0o755); err != nil {
		return err
	}
	for name, target := range map[string]string{
		"python":     plan.Executable,
		"python3":    plan.Executable,
		"python3.12": plan.Executable,
		"pip":        "/bin/pip",
		"pip3":       "/bin/pip3",
	} {
		linkPath := filepath.Join(plan.HostBinDir, name)
		if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Symlink(target, linkPath); err != nil {
			return err
		}
	}
	return nil
}

func (d *Docker) setupPythonInvocation(job Job, step Step, plan setupPythonPlan) (dockerInvocation, error) {
	image := strings.TrimSpace(job.Image)
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return dockerInvocation{}, errors.New("runner engine: image is required")
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
		"--network=none",
		"--memory=" + d.cfg.Memory,
		"--cpus=" + d.cfg.CPUs,
		"--pids-limit=" + strconv.Itoa(d.cfg.PidsLimit),
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=1g",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--security-opt=seccomp=" + d.cfg.SeccompProfile,
		"--ulimit", "nofile=" + defaultNofileLimit,
		"--ulimit", "nproc=" + defaultNprocLimit,
		"--user", user,
		"--workdir=/workspace",
		"--mount", "type=bind,src=" + job.WorkspaceDir + ",dst=/workspace",
		image, "bash", "-c", setupPythonProbeScript(plan),
	}
	return dockerInvocation{
		args:           args,
		containerName:  containerName,
		image:          image,
		network:        "none",
		memory:         d.cfg.Memory,
		cpus:           d.cfg.CPUs,
		user:           user,
		seccompProfile: d.cfg.SeccompProfile,
		pidsLimit:      d.cfg.PidsLimit,
	}, nil
}

func setupPythonProbeScript(plan setupPythonPlan) string {
	exactCheck := ""
	if plan.Exact != "" {
		exactCheck = fmt.Sprintf(`
if [ "$actual" != %q ]; then
  echo "actions/setup-python@v5: requested Python %s but this runner image provides Python $actual" >&2
  exit 1
fi`, plan.Exact, plan.Exact)
	}
	return fmt.Sprintf(`set -euo pipefail
actual="$(%s - <<'PY'
import sys
print(".".join(str(part) for part in sys.version_info[:3]))
PY
)"
case "$actual" in
  %s.*) ;;
  *)
    echo "actions/setup-python@v5: requested Python %s but this runner image provides Python $actual" >&2
    exit 1
    ;;
esac%s
%s -m venv /tmp/shithub-setup-python-venv
echo "Resolved Python $actual from %s"
echo "Created validation venv with Python $actual"`,
		plan.Executable,
		plan.Family,
		plan.Requested,
		exactCheck,
		plan.Executable,
		plan.Executable,
	)
}

func prependSetupPythonPath(job *Job, binDir string) {
	if job.Env == nil {
		job.Env = map[string]string{}
	}
	current := strings.TrimSpace(job.Env["PATH"])
	if current == "" {
		current = setupPythonDefaultPath
	}
	job.Env["PATH"] = binDir + ":" + current
}
