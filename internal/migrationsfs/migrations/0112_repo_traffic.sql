-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
CREATE TABLE repo_traffic_daily (
    repo_id bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    day date NOT NULL,
    views bigint NOT NULL DEFAULT 0 CHECK (views >= 0),
    unique_views bigint NOT NULL DEFAULT 0 CHECK (unique_views >= 0),
    clones bigint NOT NULL DEFAULT 0 CHECK (clones >= 0),
    unique_clones bigint NOT NULL DEFAULT 0 CHECK (unique_clones >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, day)
);

CREATE TABLE repo_traffic_paths (
    repo_id bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    day date NOT NULL,
    path text NOT NULL CHECK (length(path) BETWEEN 1 AND 2048),
    views bigint NOT NULL DEFAULT 0 CHECK (views >= 0),
    unique_views bigint NOT NULL DEFAULT 0 CHECK (unique_views >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, day, path)
);

CREATE TABLE repo_traffic_referrers (
    repo_id bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    day date NOT NULL,
    referrer text NOT NULL CHECK (length(referrer) BETWEEN 1 AND 255),
    views bigint NOT NULL DEFAULT 0 CHECK (views >= 0),
    unique_views bigint NOT NULL DEFAULT 0 CHECK (unique_views >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, day, referrer)
);

CREATE TABLE repo_traffic_uniques (
    repo_id bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    day date NOT NULL,
    metric text NOT NULL CHECK (metric IN ('view', 'clone', 'path', 'referrer')),
    key text NOT NULL DEFAULT '' CHECK (length(key) <= 2048),
    visitor_hash bytea NOT NULL CHECK (octet_length(visitor_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, day, metric, key, visitor_hash)
);

CREATE INDEX repo_traffic_daily_day_idx
    ON repo_traffic_daily (day DESC);

CREATE INDEX repo_traffic_uniques_created_idx
    ON repo_traffic_uniques (created_at);

-- +goose Down
DROP TABLE IF EXISTS repo_traffic_uniques;
DROP TABLE IF EXISTS repo_traffic_referrers;
DROP TABLE IF EXISTS repo_traffic_paths;
DROP TABLE IF EXISTS repo_traffic_daily;
