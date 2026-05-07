# Contributing to shithub

shithub is currently **pre-launch and closed to external contributions**. Once we cross v1.0.0 and the project's own source migrates to a self-hosted shithub instance, this document will be expanded with the full contribution workflow.

In the meantime, if you find a security issue, please follow [SECURITY.md](SECURITY.md).

## Post-launch (planned)

- Code style: `gofumpt` + `goimports`. Lint via `golangci-lint`.
- Commit messages: terse, imperative, single-line unless the change requires elaboration.
- DCO sign-off on each commit (`git commit -s`).
- Tests: every change ships with tests proportional to its scope. Integration tests hit a real Postgres; we do not mock the DB seam.
- One change per pull request; small PRs are easier to review.

## Development setup

```sh
git clone https://github.com/tenseleyFlow/shithub.git
cd shithub
make dev
```

The full development guide will land in `docs/internal/contributing.md` post-launch.
