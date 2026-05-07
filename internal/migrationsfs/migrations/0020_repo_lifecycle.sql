-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S16 repo lifecycle.
--
-- Two tables, both auxiliary to repos:
--
--   repo_redirects          — one row per (old_owner, old_name) → repo_id
--                             so /old/repo 301s to the current path.
--                             Written on rename and on transfer-accept.
--                             A single repo accumulates redirect rows
--                             over its lifetime; the current name is
--                             always on `repos.name` itself.
--
--   repo_transfer_requests  — pending transfer offers. The recipient
--                             accepts or declines; expiration is 7
--                             days; the sender can cancel.

-- +goose Up

CREATE TABLE repo_redirects (
    old_owner_user_id  bigint,
    old_owner_org_id   bigint,
    old_name           citext      NOT NULL,
    repo_id            bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    redirected_at      timestamptz NOT NULL DEFAULT now(),

    -- Exactly one old-owner FK is set, mirroring repos.owner_xor.
    CONSTRAINT repo_redirects_old_owner_xor CHECK (
        (old_owner_user_id IS NOT NULL AND old_owner_org_id IS NULL)
     OR (old_owner_user_id IS NULL     AND old_owner_org_id IS NOT NULL)
    ),
    CONSTRAINT repo_redirects_old_name_length CHECK (char_length(old_name::text) BETWEEN 1 AND 100)
);

-- Lookup index for /{old_owner}/{old_name} → repo_id. Two partial
-- indexes mirror the unique index pattern on `repos`.
CREATE UNIQUE INDEX repo_redirects_user_old_idx
    ON repo_redirects (old_owner_user_id, old_name)
    WHERE old_owner_user_id IS NOT NULL;

CREATE UNIQUE INDEX repo_redirects_org_old_idx
    ON repo_redirects (old_owner_org_id, old_name)
    WHERE old_owner_org_id IS NOT NULL;


CREATE TYPE transfer_principal_kind AS ENUM ('user', 'org');
CREATE TYPE transfer_status         AS ENUM ('pending', 'accepted', 'declined', 'canceled', 'expired');

CREATE TABLE repo_transfer_requests (
    id                 bigserial   PRIMARY KEY,
    repo_id            bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    from_user_id       bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    to_principal_kind  transfer_principal_kind NOT NULL,
    to_principal_id    bigint      NOT NULL,
    created_by         bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    status             transfer_status NOT NULL DEFAULT 'pending',
    accepted_at        timestamptz,
    declined_at        timestamptz,
    canceled_at        timestamptz,

    -- Status invariants. We don't validate the (status, *_at) cross-
    -- invariant here — the lifecycle orchestrator owns those updates
    -- in transactions and is the single writer.
    CONSTRAINT repo_transfer_requests_status_terminal CHECK (
        (status = 'pending'  AND accepted_at IS NULL AND declined_at IS NULL AND canceled_at IS NULL) OR
        (status = 'accepted' AND accepted_at IS NOT NULL) OR
        (status = 'declined' AND declined_at IS NOT NULL) OR
        (status = 'canceled' AND canceled_at IS NOT NULL) OR
        (status = 'expired')
    )
);

-- Recipient inbox lookup: "show me pending transfers offered to user X."
CREATE INDEX repo_transfer_requests_recipient_idx
    ON repo_transfer_requests (to_principal_kind, to_principal_id, status)
    WHERE status = 'pending';

-- Sender lookup + history.
CREATE INDEX repo_transfer_requests_repo_idx
    ON repo_transfer_requests (repo_id, created_at DESC);

-- Expiry sweep (the worker that flips pending → expired runs over this).
CREATE INDEX repo_transfer_requests_expiry_idx
    ON repo_transfer_requests (expires_at)
    WHERE status = 'pending';

-- +goose Down
DROP TABLE IF EXISTS repo_transfer_requests;
DROP TYPE IF EXISTS transfer_status;
DROP TYPE IF EXISTS transfer_principal_kind;
DROP TABLE IF EXISTS repo_redirects;
