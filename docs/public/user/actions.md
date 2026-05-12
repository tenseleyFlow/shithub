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

## What works today

- `push`, `pull_request`, `schedule`, and `workflow_dispatch` triggers
- `run:` steps executed in the operator-configured runner image
- `runs-on:` label matching against registered runners
- workflow, job, and step `env:`
- `${{ secrets.NAME }}`, `${{ vars.NAME }}`, `${{ env.NAME }}`, and
  `${{ shithub.* }}` expressions
- `needs:`, `if:`, `timeout-minutes:`, and concurrency groups
- live step logs, cancel, re-run, check-run sync, and the Actions Atom feed

`runs-on: ubuntu-latest` is a runner label, not a promise that shithub downloads
a hosted Ubuntu image for you. The site operator decides which image a matching
runner uses. On shithub.sh, use the labels published by the instance operator.

## Current limit

Use `run:` steps for now. The parser accepts these reserved aliases:

- `actions/checkout@v4`
- `shithub/upload-artifact@v1`
- `shithub/download-artifact@v1`

The runner does not execute them yet. A workflow containing those `uses:` steps
will fail until checkout and artifact execution land. If you need repository
files in a smoke workflow today, keep the command self-contained or fetch what
you need explicitly inside a `run:` step.

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
2. Replace `uses:` actions with equivalent `run:` commands.
3. Confirm `runs-on:` matches a label registered by your shithub operator.

Marketplace actions, Docker actions, composite actions, hosted runner images,
matrix expansion, service containers, and built-in checkout are not part of the
current v1 runner.
