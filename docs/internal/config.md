# Configuration

shithub uses a layered configuration loader. Sources, in increasing-precedence order:

1. **Built-in defaults.** Encoded in `internal/infra/config/Defaults()`.
2. **TOML file.** Path comes from `SHITHUB_CONFIG` env var, falling back to `/etc/shithub/config.toml`. Absence is fine; bad syntax is a hard error.
3. **Environment variables.** `SHITHUB_<area>__<key>` (double-underscore separates nested keys). Examples below.
4. **CLI flag overrides.** Passed in by the caller (mostly `--addr` from `shithubd web`).

After all four merge, `config.Load` applies a small set of named **aliases** (e.g. `SHITHUB_DATABASE_URL` → `db.url`) for backward compatibility, then runs `Validate`. Any validation failure causes `shithubd` to exit non-zero with a one-line error pointing at the offending key.

## Inspecting the active configuration

```sh
shithubd config print     # writes the resolved config as TOML, with secrets redacted
shithubd config validate  # exits non-zero if the resolved config is invalid
shithubd version          # includes a one-line summary of which sinks are configured
```

`config print` redacts any field whose name contains `password`, `pass`, `secret`, `key`, `token`, `dsn`, or `url` (URL fields are redacted because they often carry credentials in the userinfo component).

## Reference

| Key | Type | Default | Notes |
|---|---|---|---|
| `env` | string | `dev` | One of `dev | staging | prod`. Drives default log format and Sentry environment. |
| `web.addr` | string | `:8080` | Listen address. |
| `web.read_timeout` | duration | `30s` | Per-request read timeout. |
| `web.write_timeout` | duration | `30s` | Per-request write timeout. |
| `web.shutdown_timeout` | duration | `10s` | Graceful drain on SIGTERM. |
| `db.url` | string | `""` | Postgres DSN. Aliased by `SHITHUB_DATABASE_URL`. |
| `db.max_conns` | int | `10` | pgxpool max conns. |
| `db.min_conns` | int | `0` | pgxpool min conns. |
| `db.connect_timeout` | duration | `5s` | |
| `log.level` | string | `info` | One of `debug | info | warn | error`. |
| `log.format` | string | `text` | One of `text | json`. |
| `metrics.enabled` | bool | `true` | Mounts `/metrics`. |
| `metrics.basic_auth_user` | string | `""` | When set together with `pass`, gate `/metrics` behind HTTP Basic. |
| `metrics.basic_auth_pass` | string | `""` | |
| `tracing.enabled` | bool | `false` | When true, `tracing.endpoint` is required. |
| `tracing.endpoint` | string | `""` | OTLP HTTP endpoint, e.g. `http://otel-collector:4318`. |
| `tracing.sample_rate` | float | `0.05` | Parent-based ratio sampler in [0, 1]. |
| `tracing.service_name` | string | `shithubd` | OTel resource attribute. |
| `error_reporting.dsn` | string | `""` | Sentry-protocol DSN (works against GlitchTip). Empty disables. |
| `error_reporting.environment` | string | `""` | Tag for filtering events. |
| `error_reporting.release` | string | `""` | Tag for filtering events. |
| `session.key_b64` | string | `""` | Base64 32-byte AEAD key. Aliased by `SHITHUB_SESSION_KEY`. |
| `session.max_age` | duration | `720h` | Cookie session lifetime (30 days). |
| `session.secure` | bool | `false` | Set `Secure` cookie attribute. Enable under TLS (S37 deploy). |
| `storage.repos_root` | string | `/data/repos` | Filesystem root for bare repos. Required. |
| `storage.s3.endpoint` | string | `""` | S3-compatible endpoint host[:port], no scheme. Empty disables S3. |
| `storage.s3.region` | string | `us-east-1` | Region for SigV4 signing. |
| `storage.s3.access_key_id` | string | `""` | |
| `storage.s3.secret_access_key` | string | `""` | Redacted by `config print`. |
| `storage.s3.bucket` | string | `""` | Single bucket per environment. |
| `storage.s3.use_ssl` | bool | `false` | True for Spaces, false for local MinIO. |
| `storage.s3.force_path_style` | bool | `true` | True for MinIO, false for Spaces. |
| `auth.require_email_verification` | bool | `true` | When true, login is rejected until the primary email is verified. |
| `auth.base_url` | string | `http://127.0.0.1:8080` | Used for absolute links in transactional emails. |
| `auth.site_name` | string | `shithub` | Branding token for email subjects/bodies. |
| `auth.email_from` | string | `shithub <noreply@shithub.local>` | Envelope From for outgoing email. |
| `auth.email_backend` | string | `stdout` | One of `stdout | smtp | postmark`. |
| `auth.smtp.addr` | string | `127.0.0.1:1025` | Required when `email_backend=smtp`. |
| `auth.smtp.username` | string | `""` | Optional SMTP auth username. |
| `auth.smtp.password` | string | `""` | Optional SMTP auth password. Redacted by `config print`. |
| `auth.postmark.server_token` | string | `""` | Required when `email_backend=postmark`. Redacted. |
| `auth.argon2.memory_kib` | uint32 | `65536` | argon2id memory cost (KiB). |
| `auth.argon2.time` | uint32 | `3` | argon2id iterations. |
| `auth.argon2.threads` | uint8 | `2` | argon2id parallelism. |
| `auth.totp_key_b64` | string | `""` | Base64 32-byte AEAD key for at-rest TOTP secrets. Aliased by `SHITHUB_TOTP_KEY`. Empty disables 2FA enrollment routes. |

## Env-var examples

```sh
# Listen elsewhere
export SHITHUB_WEB__ADDR=:9090

# Connect to Postgres
export SHITHUB_DATABASE_URL=postgres://shithub:dev@127.0.0.1:5432/shithub?sslmode=disable
# (equivalent: export SHITHUB_DB__URL=...)

# JSON logs for prod
export SHITHUB_LOG__FORMAT=json
export SHITHUB_LOG__LEVEL=info

# Enable tracing
export SHITHUB_TRACING__ENABLED=true
export SHITHUB_TRACING__ENDPOINT=http://otel-collector.bare-metal:4318
export SHITHUB_TRACING__SAMPLE_RATE=0.05

# Error reporting via GlitchTip
export SHITHUB_ERROR_REPORTING__DSN=https://glitchtip.bare-metal/<project-id>

# Session signing key (deterministic across restarts in prod)
export SHITHUB_SESSION_KEY=$(openssl rand -base64 32)

# Gate /metrics behind Basic auth
export SHITHUB_METRICS__BASIC_AUTH_USER=prom
export SHITHUB_METRICS__BASIC_AUTH_PASS=<long-random>
```

## Secrets

- **Never** commit secrets. `.env` is gitignored; production keys live in a systemd `EnvironmentFile=` with mode `0600`.
- The redaction behavior of `config print` is documented above and tested in `internal/infra/config/config_test.go`. If you add a new secret-bearing field, name it so the redactor matches it (containing `pass`, `secret`, `key`, `token`, `dsn`, `password`, or `url`) — or extend `secretFieldNames` in `internal/infra/config/redact.go`.
- Log-line redaction is independent (see `docs/internal/observability.md`). Both layers exist on purpose; secrets in env-loaded config and in handler-emitted logs travel different paths.

## Adding a new key

1. Add the field to the appropriate config struct in `internal/infra/config/config.go` with a `toml:` tag.
2. Set its default in `Defaults()`.
3. Add validation in `Validate()` if it has invariants.
4. If it's secret-bearing, confirm its name matches the redactor.
5. Document it in this file.
6. Update `.env.example` if the env-var form is the typical usage.
