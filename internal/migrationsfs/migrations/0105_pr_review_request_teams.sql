-- +goose Up
-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SP19: team review requests graduate from reserved column to real target.

ALTER TABLE pr_review_requests
    ADD CONSTRAINT pr_review_requests_requested_team_id_fkey
    FOREIGN KEY (requested_team_id) REFERENCES teams(id) ON DELETE CASCADE;

CREATE INDEX pr_review_requests_team_pending_idx
    ON pr_review_requests (requested_team_id)
    WHERE dismissed_at IS NULL AND satisfied_by_review_id IS NULL;

-- +goose Down
DROP INDEX IF EXISTS pr_review_requests_team_pending_idx;

ALTER TABLE pr_review_requests
    DROP CONSTRAINT IF EXISTS pr_review_requests_requested_team_id_fkey;
