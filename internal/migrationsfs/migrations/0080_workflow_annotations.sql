-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
CREATE TYPE workflow_annotation_level AS ENUM ('notice', 'warning', 'error');

CREATE TABLE workflow_annotations (
    id bigserial PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    job_id bigint NOT NULL REFERENCES workflow_jobs(id) ON DELETE CASCADE,
    step_id bigint NOT NULL REFERENCES workflow_steps(id) ON DELETE CASCADE,
    level workflow_annotation_level NOT NULL,
    title text NOT NULL DEFAULT '',
    message text NOT NULL,
    path text NOT NULL DEFAULT '',
    start_line integer,
    end_line integer,
    start_column integer,
    end_column integer,
    log_line integer,
    log_chunk_seq integer,
    fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflow_annotations_title_len CHECK (char_length(title) <= 256),
    CONSTRAINT workflow_annotations_message_len CHECK (char_length(message) BETWEEN 1 AND 4096),
    CONSTRAINT workflow_annotations_path_len CHECK (char_length(path) <= 1024),
    CONSTRAINT workflow_annotations_start_line_positive CHECK (start_line IS NULL OR start_line > 0),
    CONSTRAINT workflow_annotations_end_line_positive CHECK (end_line IS NULL OR end_line > 0),
    CONSTRAINT workflow_annotations_start_column_positive CHECK (start_column IS NULL OR start_column > 0),
    CONSTRAINT workflow_annotations_end_column_positive CHECK (end_column IS NULL OR end_column > 0),
    CONSTRAINT workflow_annotations_log_line_positive CHECK (log_line IS NULL OR log_line > 0),
    CONSTRAINT workflow_annotations_log_chunk_seq_nonnegative CHECK (log_chunk_seq IS NULL OR log_chunk_seq >= 0),
    CONSTRAINT workflow_annotations_line_order CHECK (start_line IS NULL OR end_line IS NULL OR end_line >= start_line),
    CONSTRAINT workflow_annotations_column_order CHECK (
        start_column IS NULL OR end_column IS NULL OR start_line IS DISTINCT FROM end_line OR end_column >= start_column
    ),
    CONSTRAINT workflow_annotations_fingerprint_hex CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    UNIQUE (step_id, fingerprint)
);

CREATE INDEX workflow_annotations_run_idx ON workflow_annotations(run_id, level, created_at, id);
CREATE INDEX workflow_annotations_step_idx ON workflow_annotations(step_id, created_at, id);

-- +goose Down
DROP TABLE IF EXISTS workflow_annotations;
DROP TYPE IF EXISTS workflow_annotation_level;
