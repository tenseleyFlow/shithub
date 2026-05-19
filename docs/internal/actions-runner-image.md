# Actions runner image

shithub's shared `ubuntu-latest` runner label maps to the operator-configured
Docker image in `engine.default_image`. The upstream default is the reproducible
Nix-built image in `deploy/runner-images/`:

```text
ghcr.io/tenseleyflow/shithub/runner-nix:1.1
```

## Contract

`runner-nix:1.1` is a small hosted Linux baseline. It is meant to cover common
`run:` steps and native compilation dogfood without turning the image into a
repo-specific kitchen sink.

Guaranteed baseline tools:

- shell and POSIX utilities: bash, coreutils, findutils, gawk, gnugrep, gnused,
  diffutils, procps, util-linux, which;
- source and archive tools: git, OpenSSH, curl, gnutar, gzip, xz, gnupg, CA
  certificates;
- native build tools: gcc, gfortran, gnumake;
- Python baseline: Python 3.12 with pip, setuptools, wheel, and virtualenv;
- shithub checkout plumbing: `shithub-shallow-checkout`.

The image does not promise `apt`, `sudo`, Homebrew, arbitrary language version
managers, or marketplace action execution.

## Setup Actions

GitHub-compatible language setup is implemented as explicit first-party action
shims where shithub supports the contract. `actions/setup-python@v5` currently
selects Python 3.12 from this image/toolcache, creates workspace-local
`python`/`python3` shims, and fails clearly when another version is requested.
The image should not silently switch versions or fetch toolchains during
arbitrary workflow steps.

## Versioning

Do not mutate published tags. Toolchain changes should bump the image tag and
then update:

- `deploy/runner-images/flake.nix`;
- `.github/workflows/runner-image.yml`;
- `internal/runner/config` defaults;
- `deploy/ansible/roles/shithubd-runner/defaults/main.yml`;
- `deploy/doctl/generate-actions-runner-inventory.sh`;
- operator docs and inventory examples.

Production runners should be redeployed only after the new image is published
and a tool-version smoke passes.

## Local Smoke

After building and loading the image:

```sh
nix build ./deploy/runner-images#runnerImage
docker load < result
docker run --rm ghcr.io/tenseleyflow/shithub/runner-nix:1.1 bash -lc '
  gfortran --version
  gcc --version
  make --version
  python3 --version
  python3 -m venv /tmp/venv
  awk "BEGIN {print 1}"
  diff --version
  which git
'
```
