# Configuration reference

shithubd reads configuration from four sources, in increasing
precedence:

1. **Built-in defaults** (`internal/infra/config/Defaults()`).
2. **TOML file** at `$SHITHUB_CONFIG` (default
   `/etc/shithub/config.toml`). Absent is fine; bad syntax is
   fatal.
3. **Environment variables.** `SHITHUB_<area>__<key>` — note
   double underscore between nesting levels.
4. **CLI flags** passed by the caller.

After merging, a small alias table maps backward-compat names
(e.g., `SHITHUB_DATABASE_URL` → `db.url`); then the resolved
config is validated. Any failure exits non-zero with a one-line
error pointing at the offending key.

## Inspecting the active configuration

```sh
shithubd config print     # writes resolved config as TOML, secrets redacted
shithubd config validate  # exits non-zero if invalid
shithubd version          # one-line summary of which sinks are configured
```

`config print` redacts any field whose name contains `password`,
`pass`, `secret`, `key`, `token`, `dsn`, or `url` — URL fields
are redacted because they often carry credentials in the
userinfo component.

## Reference

| Key | Type | Default | Notes |
|---|---|---|---|
| `env` | string | `dev` | One of `dev`, `staging`, `prod`. |
| `web.addr` | string | `:8080` | Listen address. |
| `web.read_timeout` | duration | `30s` | Per-request read timeout. |
| `web.write_timeout` | duration | `30s` | Per-request write timeout. |
| `web.shutdown_timeout` | duration | `10s` | Graceful drain on SIGTERM. |
| `db.url` | string | `""` | Postgres DSN. Aliased by `SHITHUB_DATABASE_URL`. |
| `db.max_conns` | int | `10` | pgxpool max conns. |
| `db.min_conns` | int | `0` | pgxpool min conns. |
| `db.connect_timeout` | duration | `5s` | |
| `log.level` | string | `info` | One of `debug`, `info`, `warn`, `error`. |
| `log.format` | string | `text` | One of `text`, `json`. Use `json` in prod. |
| `metrics.enabled` | bool | `true` | Mounts `/metrics`. |
| `metrics.basic_auth_user` | string | `""` | Together with `pass`, gates `/metrics`. |
| `metrics.basic_auth_pass` | string | `""` | Redacted on print. |
| `tracing.enabled` | bool | `false` | When true, `tracing.endpoint` is required. |
| `tracing.endpoint` | string | `""` | OTLP HTTP endpoint. |
| `tracing.sample_rate` | float | `0.05` | Parent-based ratio sampler in [0, 1]. |
| `tracing.service_name` | string | `shithubd` | OTel resource attribute. |
| `error_reporting.dsn` | string | `""` | Sentry-protocol DSN. Empty disables. |
| `session.key_b64` | string | `""` | Base64 32-byte AEAD key. **Required in prod.** |
| `session.max_age` | duration | `720h` | Cookie session lifetime (30 days). |
| `session.secure` | bool | `false` | Set `Secure` cookie attribute. **True in prod.** |
| `storage.repos_root` | string | `/data/repos` | Filesystem root for bare repos. |
| `storage.s3.endpoint` | string | `""` | S3-compatible endpoint host[:port]. Empty disables S3. |
| `storage.s3.region` | string | `us-east-1` | Region for SigV4 signing. |
| `storage.s3.access_key_id` | string | `""` | |
| `storage.s3.secret_access_key` | string | `""` | Redacted. |
| `storage.s3.bucket` | string | `""` | One bucket per environment. |
| `storage.s3.use_ssl` | bool | `false` | True for Spaces, false for MinIO. |
| `storage.s3.force_path_style` | bool | `true` | True for MinIO, false for Spaces. |
| `auth.require_email_verification` | bool | `true` | Login rejected until primary email verified. |
| `auth.base_url` | string | `http://127.0.0.1:8080` | Used in transactional email links. **Set to your public URL in prod.** |
| `auth.site_name` | string | `shithub` | Branding token in email subjects. |
| `auth.email_from` | string | `shithub <noreply@shithub.local>` | Envelope From. |
| `auth.email_backend` | string | `stdout` | One of `stdout`, `smtp`, `postmark`. |
| `auth.smtp.addr` | string | `127.0.0.1:1025` | Required when backend=`smtp`. |
| `auth.smtp.username` | string | `""` | Optional. |
| `auth.smtp.password` | string | `""` | Optional. Redacted. |
| `auth.postmark.server_token` | string | `""` | Required when backend=`postmark`. Redacted. |
| `auth.argon2.memory_kib` | uint32 | `65536` | argon2id memory cost (KiB). |
| `auth.argon2.time` | uint32 | `3` | argon2id iterations. |
| `auth.argon2.threads` | uint8 | `2` | argon2id parallelism. |
| `auth.totp_key_b64` | string | `""` | Base64 32-byte AEAD for TOTP secrets. **Required if 2FA is in use.** |
| `billing.enabled` | bool | `false` | Enables Stripe Billing flows. Requires Stripe secret key, webhook secret, and Team Price ID. |
| `billing.grace_period` | duration | `336h` | Grace window before failed-payment paid features lock. |
| `billing.stripe.secret_key` | string | `""` | Stripe secret API key. Redacted. |
| `billing.stripe.webhook_secret` | string | `""` | Stripe webhook signing secret. Redacted. |
| `billing.stripe.team_price_id` | string | `""` | Recurring per-unit/licensed Team Price ID. shithub sends active org-member quantity for seats. |
| `billing.stripe.pro_price_id` | string | `""` | Optional recurring single-seat Pro Price ID. Enables personal-account Pro checkout. |
| `billing.stripe.automatic_tax` | bool | `false` | Enables Stripe automatic tax after the Stripe account is configured for tax collection. |
| `billing.enforce.user_required_reviewers` | bool | `false` | Hard-enforce the Pro required-reviewers gate. False is report-only. |
| `billing.enforce.user_advanced_branch_protection` | bool | `false` | Hard-enforce the Pro advanced-branch-protection gate. |
| `billing.enforce.user_profile_pins_beyond_free` | bool | `false` | Hard-enforce the Pro profile-pin cap gate. |

## Examples

```sh
# Listen elsewhere
export SHITHUB_WEB__ADDR=:9090

# Postgres
export SHITHUB_DATABASE_URL=postgres://shithub:****@127.0.0.1:5432/shithub?sslmode=require

# JSON logs for prod
export SHITHUB_LOG__FORMAT=json
export SHITHUB_LOG__LEVEL=info

# Tracing
export SHITHUB_TRACING__ENABLED=true
export SHITHUB_TRACING__ENDPOINT=http://otel-collector:4318
export SHITHUB_TRACING__SAMPLE_RATE=0.05

# Session signing key
export SHITHUB_SESSION_KEY=$(openssl rand -base64 32)

# Metrics behind Basic auth
export SHITHUB_METRICS__BASIC_AUTH_USER=prom
export SHITHUB_METRICS__BASIC_AUTH_PASS=<long-random>
```

## Secrets handling

- Never commit secrets. `.env` is gitignored; production secrets
  live in a systemd `EnvironmentFile=` with mode `0600`.
- The Ansible roles read sensitive values from the inventory
  (which you keep encrypted with `ansible-vault` or your secret
  store) and template them into `/etc/shithub/web.env` and
  `worker.env`.
- Log-line redaction is independent of `config print` redaction;
  both layers exist on purpose.

## Adding a key

If you fork shithub and add a config key:

1. Add the field to the appropriate struct in
   `internal/infra/config/config.go` with a `toml:` tag.
2. Set its default in `Defaults()`.
3. Add validation in `Validate()` if it has invariants.
4. If it carries a secret, name it so the redactor matches.
5. Document it here.
