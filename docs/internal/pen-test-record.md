# Internal pen-test record

Records the S39 internal pen-test (3 days of focused effort by
the project author against the staging instance). Findings logged
here with their disposition.

> **Status.** Like the a11y record, this file's structure is in
> place; the body is filled in at audit time. Nothing here yet
> because the live test happens against the deployed staging
> instance (S37) once it's stood up — that's the operator's call,
> not a code-time deliverable.

## Scope

Per the S39 spec:

- Top OWASP risks (injection, broken auth, sensitive data
  exposure, XXE, broken access control, security
  misconfiguration, XSS, insecure deserialization, vulnerable
  components, insufficient logging).
- Auth surfaces: signup, login, password reset, 2FA, PATs,
  sessions, SSH key add/remove, session-epoch revocation,
  per-account "sign out everywhere".
- Git protocols: HTTPS smart-HTTP push/pull, SSH (when shipped),
  hook subprocess privilege boundary.
- Webhook SSRF: URL validation, redirect-following defense,
  IP block-list coverage.

Out of scope (covered separately or post-launch):

- Third-party penetration test — post-launch.
- Public bug bounty — post-launch.
- Side-channel attacks on the host — OS/runtime concern.
- Physical access — standard ops practice.

## Methodology

1. Re-run the `security audit` CLI from S35 — every finding
   triaged.
2. Manual exploration of the auth surfaces. Account takeover
   scenarios (password reuse, session fixation, CSRF on
   state-changing forms, TOTP recovery race).
3. Git protocol review. Authorization for push (pre-receive),
   read access for fetch (visibility check), AKC privilege
   boundary.
4. Webhook fuzzing. SSRF attempts against private-IP ranges,
   redirect chains, DNS rebinding, payload size manipulation.
5. Authorization grid. Each policy.Action × actor-shape — verify
   `policy.Can` returns the expected decision. The per-action
   table from `internal/auth/policy/` is the checklist.

## Findings template

```
### P-NN — <short title>

- **Severity:** critical / high / medium / low
- **Class:** auth / git / webhook / xss / csrf / ssrf / injection / info-leak / dos
- **Found by:** security audit CLI / manual / fuzzing
- **Route or surface:** /…/…
- **Description:** what's wrong + how to reproduce.
- **Disposition:** fixed in <commit-sha> / accepted: <rationale> / deferred to <sprint>
- **Re-tested on:** <date>
```

## Findings

(none yet)

## Accepted with rationale

(none yet)

## Areas NOT looked at

Documented so the gap is the post-launch third-party scope:

- Race conditions in concurrent webhook delivery.
- TOCTOU bugs on file-system operations during git push.
- Side-channel timing on argon2 verification. (Mitigation:
  argon2id is constant-time per implementation.)
- Cryptanalysis of HMAC-signed cursors / unsubscribe links.

## Tooling notes

- The `security audit` CLI lives at
  `cmd/shithubd/admin.go` — sub-command list at S35.
- Burp / ZAP are not part of the toolchain; manual + curl + the
  in-binary helpers cover what we need at MVP.
- `internal/security/ssrf` ships with its own unit tests; the
  fuzzing pass exercises the **integration** of SSRF defense
  with the webhook delivery path, not the unit logic.

## Re-audit cadence

- Every release with auth or git surface changes.
- Quarterly full pass.
- After any incident with a security flavor — investigation +
  audit go together.
