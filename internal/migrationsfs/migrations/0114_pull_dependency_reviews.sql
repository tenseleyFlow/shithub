-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25a: pull-request dependency review. Reviews are PR-owned snapshots of the
-- dependency delta between base and head, plus any local advisory matches used
-- to publish the "Dependency review" check run.

-- +goose Up
CREATE TABLE pull_dependency_reviews (
    id                         bigserial   PRIMARY KEY,
    pr_id                      bigint      NOT NULL REFERENCES pull_requests(issue_id) ON DELETE CASCADE,
    repo_id                    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    base_sha                   text        NOT NULL,
    head_sha                   text        NOT NULL,
    conclusion                 text        NOT NULL DEFAULT 'neutral',
    manifest_count             int         NOT NULL DEFAULT 0,
    change_count               int         NOT NULL DEFAULT 0,
    added_count                int         NOT NULL DEFAULT 0,
    removed_count              int         NOT NULL DEFAULT 0,
    changed_count              int         NOT NULL DEFAULT 0,
    vulnerable_change_count    int         NOT NULL DEFAULT 0,
    reviewed_at                timestamptz NOT NULL DEFAULT now(),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pull_dependency_reviews_base_sha_shape CHECK (char_length(base_sha) BETWEEN 1 AND 128),
    CONSTRAINT pull_dependency_reviews_head_sha_shape CHECK (char_length(head_sha) BETWEEN 1 AND 128),
    CONSTRAINT pull_dependency_reviews_conclusion_known CHECK (conclusion IN ('success', 'failure', 'neutral')),
    CONSTRAINT pull_dependency_reviews_counts_nonnegative CHECK (
        manifest_count >= 0 AND change_count >= 0 AND added_count >= 0
        AND removed_count >= 0 AND changed_count >= 0 AND vulnerable_change_count >= 0
    ),
    CONSTRAINT pull_dependency_reviews_unique UNIQUE (pr_id, base_sha, head_sha)
);

CREATE INDEX pull_dependency_reviews_pr_latest_idx
    ON pull_dependency_reviews (pr_id, reviewed_at DESC, id DESC);

CREATE INDEX pull_dependency_reviews_repo_head_idx
    ON pull_dependency_reviews (repo_id, head_sha);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON pull_dependency_reviews
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE pull_dependency_review_items (
    id                    bigserial   PRIMARY KEY,
    review_id             bigint      NOT NULL REFERENCES pull_dependency_reviews(id) ON DELETE CASCADE,
    change_kind           text        NOT NULL,
    ecosystem             text        NOT NULL,
    package_name          text        NOT NULL,
    manifest_path         text        NOT NULL,
    lockfile_path         text        NOT NULL DEFAULT '',
    old_version           text        NOT NULL DEFAULT '',
    new_version           text        NOT NULL DEFAULT '',
    scope                 text        NOT NULL DEFAULT '',
    direct                boolean     NOT NULL DEFAULT true,
    package_manager       text        NOT NULL DEFAULT '',
    source                text        NOT NULL DEFAULT '',
    advisory_id           bigint      NULL REFERENCES dependency_advisories(id) ON DELETE SET NULL,
    severity              text        NOT NULL DEFAULT '',
    advisory_source       text        NOT NULL DEFAULT '',
    advisory_external_id  text        NOT NULL DEFAULT '',
    advisory_summary      text        NOT NULL DEFAULT '',
    patched_versions      text        NOT NULL DEFAULT '',
    recommendation        text        NOT NULL DEFAULT '',
    created_at            timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pull_dependency_review_items_change_kind_known CHECK (change_kind IN ('added', 'removed', 'changed')),
    CONSTRAINT pull_dependency_review_items_ecosystem_shape CHECK (char_length(ecosystem) BETWEEN 1 AND 40),
    CONSTRAINT pull_dependency_review_items_package_name_shape CHECK (char_length(package_name) BETWEEN 1 AND 512),
    CONSTRAINT pull_dependency_review_items_manifest_path_shape CHECK (char_length(manifest_path) BETWEEN 1 AND 1024),
    CONSTRAINT pull_dependency_review_items_lockfile_path_shape CHECK (char_length(lockfile_path) <= 1024),
    CONSTRAINT pull_dependency_review_items_old_version_shape CHECK (char_length(old_version) <= 255),
    CONSTRAINT pull_dependency_review_items_new_version_shape CHECK (char_length(new_version) <= 255),
    CONSTRAINT pull_dependency_review_items_scope_shape CHECK (char_length(scope) <= 80),
    CONSTRAINT pull_dependency_review_items_package_manager_shape CHECK (char_length(package_manager) <= 80),
    CONSTRAINT pull_dependency_review_items_source_shape CHECK (char_length(source) <= 80),
    CONSTRAINT pull_dependency_review_items_severity_known CHECK (severity IN ('', 'low', 'moderate', 'high', 'critical')),
    CONSTRAINT pull_dependency_review_items_advisory_summary_shape CHECK (char_length(advisory_summary) <= 500),
    CONSTRAINT pull_dependency_review_items_recommendation_shape CHECK (char_length(recommendation) <= 500)
);

CREATE INDEX pull_dependency_review_items_review_idx
    ON pull_dependency_review_items (review_id, ecosystem, lower(package_name), manifest_path);

CREATE INDEX pull_dependency_review_items_advisory_idx
    ON pull_dependency_review_items (advisory_id)
    WHERE advisory_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS pull_dependency_review_items;
DROP TABLE IF EXISTS pull_dependency_reviews;
