-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow step log chunks — append-only log buffer for an
-- in-flight step.
--
-- Hot path during a running step: runner POSTs each chunk via the
-- chunks API (S41c); we INSERT a row keyed by (step_id, seq). The UI's
-- SSE handler (S41f) LISTENs on `step_log_<step_id>` and SELECTs new
-- chunks since the last seen seq.
--
-- Cold path after step completion: a single workflow:finalize_step
-- worker job (S41d) concatenates all chunks for the step, uploads the
-- result to Spaces (workflow_steps.log_object_key), and DELETEs the
-- chunk rows. Postgres stays small; long-term storage stays cheap.
--
-- Per-row cap: 512 KB chunk size. Per-step soft cap (10 MB) and per-job
-- hard cap (100 MB) are enforced runner-side at insert time (S41d) —
-- we don't put a CHECK constraint on the cumulative volume because
-- each row is independent. Total budget is observability + runner
-- discipline.
--
-- created_at indexed for retention sweep (S41g) on chunks orphaned by
-- runner crashes.

-- +goose Up

CREATE TABLE workflow_step_log_chunks (
    id          bigserial    PRIMARY KEY,
    step_id     bigint       NOT NULL REFERENCES workflow_steps(id) ON DELETE CASCADE,
    seq         integer      NOT NULL,
    chunk       bytea        NOT NULL,
    created_at  timestamptz  NOT NULL DEFAULT now(),

    UNIQUE (step_id, seq),

    CONSTRAINT workflow_step_log_chunks_seq_nonneg CHECK (seq >= 0),
    CONSTRAINT workflow_step_log_chunks_size_cap CHECK (
        octet_length(chunk) BETWEEN 1 AND 524288
    )
);

CREATE INDEX workflow_step_log_chunks_step_seq_idx
    ON workflow_step_log_chunks (step_id, seq);
CREATE INDEX workflow_step_log_chunks_created_idx
    ON workflow_step_log_chunks (created_at);


-- +goose Down
DROP TABLE IF EXISTS workflow_step_log_chunks;
