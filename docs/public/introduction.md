# Introduction

shithub is a self-hosted git forge that aims to look and feel like
GitHub. The project is AGPLv3, written in Go, and built around a
small operational footprint — one binary, Postgres, an S3-compatible
object store, and Caddy at the edge.

This site is the public documentation. It is split into three
sections:

- **Using shithub** — for end users of an instance: how to push
  code, file issues, manage tokens, configure webhooks.
- **API reference** — for integrators writing against the REST
  surface.
- **Self-hosting** — for operators standing up an instance.

The internal architecture and operator runbooks live in the
[repository](https://github.com/tenseleyFlow/shithub) under
`docs/internal/`. Some of that material is mirrored here in
operator-friendly form; the in-repo originals are the source of
truth for the people running the site.

## Project status

shithub is **post-launch but young**. The core forge loop (auth,
repos, git over HTTPS, issues, PRs with reviews, webhooks,
notifications, search, orgs/teams) works end-to-end. There are
gaps — SSH git transport, an Actions-equivalent CI runner, a
GraphQL API, and parts of the visual polish are still in flight
or planned. The `Changelog` lists what shipped when.

If you're new, start with the [Quickstart](./user/quickstart.md).
