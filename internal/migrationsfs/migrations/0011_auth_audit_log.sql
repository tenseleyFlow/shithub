-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Generic audit log for security-relevant events. Schema is intentionally
-- broad so future sprints can reuse it (S15 permissions, S30 org changes,
-- S34 admin actions, S07 SSH key changes).
--
-- - actor_id: the user performing the action (NULL for unauthenticated /
--   admin-CLI actions; admin actions populate meta->>'admin' instead).
-- - action: short snake_case verb (e.g. '2fa_enabled', 'recovery_regenerated',
--   'admin_cleared_2fa').
-- - target_type, target_id: the entity affected (e.g. 'user', user.id).
-- - meta: free-form JSON for action-specific detail (no secrets here).

-- +goose Up
CREATE TABLE auth_audit_log (
    id          bigserial   PRIMARY KEY,
    actor_id    bigint      REFERENCES users(id) ON DELETE SET NULL,
    action      text        NOT NULL,
    target_type text        NOT NULL,
    target_id   bigint,
    meta        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX auth_audit_log_actor_id_idx   ON auth_audit_log (actor_id);
CREATE INDEX auth_audit_log_target_idx     ON auth_audit_log (target_type, target_id);
CREATE INDEX auth_audit_log_action_idx     ON auth_audit_log (action);
CREATE INDEX auth_audit_log_created_at_idx ON auth_audit_log (created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS auth_audit_log;
