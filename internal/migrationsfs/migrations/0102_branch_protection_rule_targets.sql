-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP18b extends repository rules from branch-only protection to
-- branch-or-tag targeting. Existing rows are branch rules.

-- +goose Up
ALTER TABLE branch_protection_rules
    ADD COLUMN target text NOT NULL DEFAULT 'branch',
    ADD CONSTRAINT branch_protection_rules_target_check CHECK (target IN ('branch', 'tag'));

CREATE INDEX branch_protection_rules_repo_target_idx
    ON branch_protection_rules (repo_id, target, pattern);

-- +goose Down
DROP INDEX IF EXISTS branch_protection_rules_repo_target_idx;

ALTER TABLE branch_protection_rules
    DROP CONSTRAINT IF EXISTS branch_protection_rules_target_check,
    DROP COLUMN IF EXISTS target;
