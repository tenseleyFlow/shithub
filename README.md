# shithub

GitHub. Open source. Without Copilot.

shithub is a feature-complete, self-hostable, AGPL-licensed clone of GitHub. The full experience — repos, issues, pull requests, organizations, actions, social feed, search, settings, ssh & https git protocols — minus the AI integrations.

> Status: pre-launch. Not yet hosted. The codebase is currently developed against an embedded plan; expect rapid change.

## Why

GitHub is a well-built platform with broad feature coverage. The aggressive Copilot push has changed the product's character. shithub aims to recreate the parts that worked while staying honest about what an open-source forge can be.

## Quickstart (development)

```sh
make dev          # hot-reload server on http://localhost:8080
make test         # run tests
make build        # build bin/shithubd
make ci           # full CI pipeline locally (mirrors GitHub Actions)
```

Requires:
- Go 1.22+
- `golangci-lint`, `gofumpt`, `goimports`, `air` (installed via `go install` per the Makefile or Homebrew)

## Layout

```
cmd/shithubd/   # main entry point + subcommands (web, ssh, worker, migrate, version, ...)
internal/       # domain packages (web, auth, repo, git, issues, pulls, ...)
templates/      # html/template files
static/         # css, js, images, logo
migrations/     # SQL migrations
docs/           # public-facing docs
deploy/         # Ansible playbooks + systemd units
```

## License

AGPLv3. See [LICENSE](LICENSE).

## Status

Pre-launch. See [CONTRIBUTING.md](CONTRIBUTING.md) for the current contribution posture.

## Security

To report a security issue, see [SECURITY.md](SECURITY.md). Please do not open a public issue for security reports.
