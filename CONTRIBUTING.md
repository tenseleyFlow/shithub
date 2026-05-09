# Contributing to shithub

shithub is open to contributions from anyone. The project is
post-launch (per S40); the workflow is the standard one.

By contributing, you certify that the work is yours to give and
that you're licensing it to the project under the same terms as
the rest of the codebase (AGPLv3). The mechanism is the
[Developer Certificate of Origin (DCO) v1.1](https://developercertificate.org/)
— sign off your commits with `-s`:

```sh
git commit -s -m "your message"
```

A CI check rejects PRs whose commits aren't signed off.

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).
In short: be civil; harassment isn't tolerated; report issues to
the email listed in that file.

## Reporting bugs

For non-security bugs: open an issue on
[github.com/tenseleyFlow/shithub/issues](https://github.com/tenseleyFlow/shithub/issues).

For security issues: see [SECURITY.md](./SECURITY.md). Do **not**
file public issues for security findings; the security mailbox
is the disclosure path.

## Development setup

```sh
git clone https://github.com/tenseleyFlow/shithub.git
cd shithub
make install-tools
make dev-db dev-storage    # Postgres + MinIO via docker compose
make dev-migrate           # apply migrations
make dev                   # hot-reload server on http://localhost:8080
```

Requires:
- Go 1.22+ (developed against 1.26).
- PostgreSQL 13+.
- A local MinIO (or any S3-compatible store).
- `golangci-lint`, `gofumpt`, `goimports`, `air`, `sqlc` (installed
  via `make install-tools`).

Copy `.env.example` to `.env` and fill in DB URL, session keys,
and the TOTP/webhook AEAD keys.

## Submitting a change

1. **Branch.** From `main`, create a feature branch.
2. **Commit.** Terse, imperative, single-line messages unless the
   change needs elaboration. One concern per commit. Avoid
   `git add -A`; stage by file.
3. **Test.** Add tests proportional to the scope. Integration
   tests hit a real Postgres; we do not mock the DB seam. The
   bench harness (`bench/`) is the right home for performance
   regressions.
4. **Lint.** `make ci` runs everything CI runs. PRs must be green.
5. **Push + PR.** Open against `main`. The PR template prompts
   for a summary + test plan + reviewer notes.

## Code style

- `gofumpt` + `goimports` (run via `make fmt`).
- `golangci-lint` per the in-repo config.
- Package-boundary lints (markdown, policy, secret-logs, CSRF)
  are enforced via `scripts/lint-*.sh`. Adding a goldmark import
  outside `internal/markdown` will fail CI.
- Comment style: lead with the why. Don't restate the code.
- Migrations: forward-only by convention; `down` exists for
  emergency rollback only and may drop data.

## Reviewing

Maintainers (and other contributors with write access) review
each PR. Aim for one approval before merge; protected branches
enforce status checks + required reviews.

If you've left a "Request changes" review, please follow up
within a few days — open PRs are a tax on the author.

## Cutting a release

Maintainers only. The procedure is in
`docs/internal/runbooks/upgrade.md`. Tag the release, push,
run the deploy.

## Communication

- **Discussion of features / RFCs:** open an issue tagged
  `proposal`.
- **Day-to-day chat:** the project doesn't run a Slack/Discord
  yet; PR comments and issues are the channel.
- **Security:** `security@shithub.sh` (see SECURITY.md).
