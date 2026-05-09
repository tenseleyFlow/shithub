# Security policy

## Reporting a vulnerability

Email **`security@shithub.sh`**. PGP-encrypt the report
using the key fingerprint published at
`https://shithub.sh/.well-known/pgp-key.asc` if your finding
is sensitive.

The mailbox auto-acknowledges receipt within minutes. A human
response (initial assessment + next steps) follows within
**72 hours**.

Please **do not** file public issues for security findings.
Coordinated disclosure is the norm; we will credit you in the
hall of fame on resolution unless you ask not to be named.

## Scope

In scope:

- The hosted shithub instance (`shithub.sh`).
- The shithub source as published on GitHub
  (`github.com/tenseleyFlow/shithub`), exploited against any
  reasonably-deployed self-hosted instance running an unmodified
  release tag.

Out of scope:

- Findings against third-party services we depend on
  (DigitalOcean, Postmark, Let's Encrypt). Report those to the
  vendor.
- Misconfiguration of a self-hosted instance (e.g., operator
  exposed `/metrics` without auth) — unless the misconfiguration
  is the *default* of a current release.
- Rate-limit-bypass via heroic distributed-IP infrastructure —
  outside the threat model
  (`docs/internal/threat-model.md`).
- Issues that require physical access to the server.
- DoS via resource exhaustion that requires sustained heavy
  traffic from many unique IPs.
- Best-practice findings without an exploit path (e.g., "you're
  not setting `X-Permitted-Cross-Domain-Policies`") — file these
  as regular issues.

## Bug bounty

shithub does not currently run a paid bounty program. We welcome
findings regardless and will publicly credit you.

## Severity

Coarse 4-level scale:

| Severity | Examples                                                       | Target fix |
|----------|----------------------------------------------------------------|-----------:|
| Critical | RCE; auth bypass; mass-account-takeover; private-data leak     | < 24h      |
| High     | Per-user privilege escalation; SSRF into internal infra        | < 7d       |
| Medium   | Stored XSS limited to an attacker's own scope; CSRF on a non-destructive route | < 30d |
| Low      | Information disclosure of non-sensitive data                   | best-effort |

## What you'll receive

- **Acknowledgement** within 72 hours (auto-ack faster).
- **Triage decision** — accepted, duplicate, out-of-scope, or
  needs-more-info — within 7 days for High+ and 30 days for
  Medium/Low.
- **Fix timeline** based on severity.
- **Coordinated disclosure** on patched release; we publish a
  brief writeup naming you (with consent) and the affected
  versions.

## Hall of fame

Reporters who responsibly disclosed accepted findings:

*(Empty for now — first credit goes to the first reporter.)*

## Our threat model

Published at
[`docs/internal/threat-model.md`](./docs/internal/threat-model.md).
Useful context on what we defend against and what we don't.
