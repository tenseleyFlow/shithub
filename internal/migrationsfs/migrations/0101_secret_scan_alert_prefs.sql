-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10d: per-user secret-scan alert preferences.
--
-- A row exists only if the user has explicitly opted in to at least
-- one alert channel (email or webhook). Absence of a row → silent
-- (which is also the implicit Free behavior under enforce).
--
-- The webhook secret is the HMAC-SHA256 signing key for outbound
-- POSTs; we hold the raw bytes here (not a hash) because we need to
-- sign payloads on send. Webhook URL is bounded at 2 KiB to defeat
-- pathological lengths without preventing reasonable Cloudflare-style
-- worker URLs.

-- +goose Up
CREATE TABLE secret_scan_alert_prefs (
    user_id         bigint  PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    email_enabled   boolean NOT NULL DEFAULT false,
    webhook_url     text    NULL,
    webhook_secret  bytea   NULL,
    -- last_alerted_at lets the alert worker dedupe: a re-scan that
    -- re-surfaces the same finding (status returned to open) shouldn't
    -- spam the user. NULL until the first successful send.
    last_alerted_at timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT secret_scan_alert_prefs_webhook_pair CHECK (
        (webhook_url IS NULL AND webhook_secret IS NULL)
        OR (webhook_url IS NOT NULL AND webhook_secret IS NOT NULL)
    ),
    CONSTRAINT secret_scan_alert_prefs_webhook_url_shape CHECK (
        webhook_url IS NULL
        OR (char_length(webhook_url) BETWEEN 8 AND 2048
            AND (webhook_url LIKE 'http://%' OR webhook_url LIKE 'https://%'))
    ),
    CONSTRAINT secret_scan_alert_prefs_webhook_secret_shape CHECK (
        webhook_secret IS NULL
        OR octet_length(webhook_secret) BETWEEN 32 AND 64
    ),
    CONSTRAINT secret_scan_alert_prefs_at_least_one CHECK (
        email_enabled OR webhook_url IS NOT NULL
    )
);

-- +goose Down
DROP TABLE IF EXISTS secret_scan_alert_prefs;
