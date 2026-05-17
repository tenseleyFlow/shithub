-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertDeviceAuthorization :one
INSERT INTO device_authorizations (
    device_code_hash, user_code, client_id, scopes,
    interval_seconds, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, device_code_hash, user_code, client_id, scopes, user_id,
          approved_at, denied_at, issued_token_id, interval_seconds,
          expires_at, last_polled_at, created_at;

-- name: GetDeviceAuthorizationByCodeHash :one
-- Hot path for the polling /access_token endpoint. The middleware
-- enforces interval_seconds via last_polled_at downstream.
SELECT id, device_code_hash, user_code, client_id, scopes, user_id,
       approved_at, denied_at, issued_token_id, interval_seconds,
       expires_at, last_polled_at, created_at
FROM device_authorizations
WHERE device_code_hash = $1;

-- name: GetDeviceAuthorizationByCodeHashForUpdate :one
-- Same SELECT body, but with FOR UPDATE so concurrent Exchange polls on
-- the same device_code serialize. Used by the Exchange path inside its
-- transaction; the non-FOR-UPDATE variant stays for read-only lookups
-- (e.g. the HTML consent page).
SELECT id, device_code_hash, user_code, client_id, scopes, user_id,
       approved_at, denied_at, issued_token_id, interval_seconds,
       expires_at, last_polled_at, created_at
FROM device_authorizations
WHERE device_code_hash = $1
FOR UPDATE;

-- name: GetDeviceAuthorizationByUserCode :one
-- Lookup path for the verification page. Returns even non-pending rows
-- so the handler can render a clean "already approved" / "expired" page
-- instead of a generic 404.
SELECT id, device_code_hash, user_code, client_id, scopes, user_id,
       approved_at, denied_at, issued_token_id, interval_seconds,
       expires_at, last_polled_at, created_at
FROM device_authorizations
WHERE user_code = $1;

-- name: ApproveDeviceAuthorization :exec
-- Records the user's approval and links the freshly minted PAT.
-- Idempotency is preserved by the caller — the orchestrator only
-- calls this once per row.
UPDATE device_authorizations
SET user_id = $2,
    approved_at = now(),
    issued_token_id = $3
WHERE id = $1
  AND approved_at IS NULL
  AND denied_at IS NULL
  AND expires_at > now();

-- name: DenyDeviceAuthorization :exec
UPDATE device_authorizations
SET denied_at = now()
WHERE id = $1
  AND approved_at IS NULL
  AND denied_at IS NULL;

-- name: TouchDeviceAuthorizationPoll :exec
UPDATE device_authorizations
SET last_polled_at = now()
WHERE id = $1;

-- name: StampIssuedTokenID :exec
-- Records the user_tokens.id minted by Exchange against the grant row.
-- Runs inside the Exchange transaction so the PAT insert and this stamp
-- commit atomically — no orphan PATs if the process dies mid-Exchange,
-- no double-mint if two polls land concurrently (the FOR UPDATE in
-- GetDeviceAuthorizationByCodeHashForUpdate serializes them).
UPDATE device_authorizations
SET issued_token_id = $2
WHERE id = $1;

-- name: DeleteExpiredDeviceAuthorizations :exec
-- Janitor invocation: a small forensics window past expiry is fine,
-- but eventually drop the row so the user_code index stays small.
DELETE FROM device_authorizations
WHERE expires_at < now() - interval '24 hours';
