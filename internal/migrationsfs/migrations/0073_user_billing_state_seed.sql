-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO03 — seed user_billing_states for every user.
--
-- Mirrors 0061's org billing seed pattern. Without the dense backfill
-- the entitlements loader (PRO05) would blow up on any user created
-- before 0072 landed. ON CONFLICT DO NOTHING keeps the migration
-- idempotent across reruns.

-- +goose Up

INSERT INTO user_billing_states (user_id, plan)
SELECT id, 'free'::user_plan
FROM users
ON CONFLICT (user_id) DO NOTHING;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_user_billing_state_seed() RETURNS trigger AS $$
BEGIN
    INSERT INTO user_billing_states (user_id, plan)
    VALUES (NEW.id, 'free'::user_plan)
    ON CONFLICT (user_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tg_user_billing_state_seed_ai
    AFTER INSERT ON users
    FOR EACH ROW EXECUTE FUNCTION tg_user_billing_state_seed();

-- +goose Down

DROP TRIGGER IF EXISTS tg_user_billing_state_seed_ai ON users;
DROP FUNCTION IF EXISTS tg_user_billing_state_seed();

-- Down deliberately leaves the backfilled rows; the previous migration
-- (0072) drops user_billing_states entirely, which is the right place
-- for that cleanup.
