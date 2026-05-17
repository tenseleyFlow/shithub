# Actions

shithub Actions runs CI workflows from `.shithub/workflows/*.yml`.
The workflow format intentionally follows the parts of GitHub Actions that are
useful for ordinary repository CI, while keeping the runner surface small enough
to secure.

## Minimal workflow

```yaml
name: smoke
on: [push, workflow_dispatch]
jobs:
  hello:
    runs-on: ubuntu-latest
    env:
      RUN_ID: ${{ shithub.run_id }}
    steps:
      - run: echo "hello from shithub actions"
      - run: test -n "$RUN_ID"
```

Commit that file as `.shithub/workflows/smoke.yml` and push to the repository.
The run appears under the repository's Actions tab and its job also appears as
a check run on matching pull requests.

## Copy-paste smoke workflow

Use this workflow to confirm a normal repository can use the shared Linux pool.
It runs on every push to `trunk` while Actions are enabled for the repository
and a runner advertising `ubuntu-latest` is online.

```yaml
name: Smoke
on:
  push:
    branches: [trunk]
jobs:
  green:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Verify checkout
        run: test -f README.md || test -f readme.md || pwd
      - name: Smoke
        run: printf 'shithub actions smoke passed\n'
```

The same file should work in any repository that is allowed by the site, org,
and repo Actions policies. It should not need a repo-specific runner label.

## Creating a workflow in the web UI

Repository writers can open **Actions** and choose **New workflow** to create a
workflow file under `.shithub/workflows/`. The picker offers supported starter
templates for minimal shell smoke tests, checkout verification, scheduled
smoke runs, and manual `workflow_dispatch` runs.

The form validates the path, target branch, file collision, and workflow syntax
before committing. Common GitHub templates that need setup/cache/matrix/service
features are shown as unavailable instead of being offered as broken workflows.

## Pinning workflows

Signed-in users can pin workflows from the Actions sidebar. Pins are personal
to the viewer and repository: they do not change anyone else's sidebar.
Pinned workflows appear before unpinned workflows across Actions pages, and
the small sidebar controls can move pinned workflows up or down.

## What works today

- `push`, `pull_request`, `schedule`, and `workflow_dispatch` triggers
- `actions/checkout@v4` for repository checkout
- `run:` steps executed in the operator-configured runner image
- `runs-on:` label matching against registered runners
- workflow, job, and step `env:`
- `${{ secrets.NAME }}`, `${{ vars.NAME }}`, `${{ env.NAME }}`, and
  `${{ shithub.* }}` expressions
- `needs:`, `if:`, `timeout-minutes:`, and concurrency groups
- live step logs, grouped completed logs, cancel, re-run, check-run sync, and
  the Actions Atom feed

Completed step-log pages recognize `::group::<title>` and `::endgroup::`
markers as collapsible sections. Line numbers are stable anchors; click a line
number or copy icon to copy a permalink, and shift-click a second line to copy a
line-range link. Raw log downloads stay unchanged and include the original
group marker lines.

`runs-on: ubuntu-latest` is a runner label, not a promise that shithub downloads
a hosted Ubuntu image for you. The site operator decides which image a matching
runner uses. On shithub.sh, the shared Linux pool advertises
`self-hosted`, `linux`, `ubuntu-latest`, and `x64`.

If a run stays queued, the run page shows the requested label set, for example
`Waiting for runner with labels: windows-latest`. That means no currently
registered runner can claim the job.

## Current limit

The runner executes `actions/checkout@v4` and `run:` steps. Checkout accepts
the default shallow fetch and `with.fetch-depth`; use `fetch-depth: 0` when a
workflow needs full history:

```yaml
steps:
  - uses: actions/checkout@v4
    with:
      fetch-depth: "0"
  - run: git describe --tags --always
```

The parser also accepts these artifact aliases:

- `shithub/upload-artifact@v1`
- `shithub/download-artifact@v1`

The runner does not execute artifact aliases yet. A workflow containing those
artifact `uses:` steps will fail until artifact execution lands. Checkout
inputs such as `path`, submodules, LFS, and persisted credentials are not
implemented yet.

## Expressions

Use the shithub namespace:

```yaml
env:
  REF: ${{ shithub.ref }}
  SHA: ${{ shithub.sha }}
  RUN_ID: ${{ shithub.run_id }}
```

The `github.*` namespace is accepted as a compatibility alias for the fields
shithub exposes, but new workflows should use `shithub.*`.

Event payload values such as `${{ shithub.event.pull_request.title }}` are
treated as untrusted. The runner passes them through temporary environment
bindings instead of splicing them directly into shell command text.

## Secrets and variables

Repository and organization settings expose Actions secrets and variables.
Secrets are encrypted at rest and are redacted from logs. Variables are
plaintext configuration and are suitable for non-secret values such as tool
versions or feature flags.

Repo-scoped values shadow organization-scoped values with the same name.

## Migrating from GitHub Actions

Most simple CI files need three edits:

1. Move the workflow file from `.github/workflows/` to `.shithub/workflows/`.
2. Keep `actions/checkout@v4`, but replace marketplace and artifact `uses:`
   actions with equivalent `run:` commands for now.
3. Confirm `runs-on:` matches a label registered by your shithub operator.
   The default shithub.sh shared label for ordinary Linux CI is
   `ubuntu-latest`.

Marketplace actions, Docker actions, composite actions, hosted runner images,
matrix expansion, service containers, submodules, LFS, and artifact transfer
are not part of the current v1 runner.
