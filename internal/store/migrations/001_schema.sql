-- Weekday follows Postgres EXTRACT(DOW): 0 = Sunday ... 6 = Saturday.
CREATE TABLE IF NOT EXISTS programs (
    id         serial PRIMARY KEY,
    name       text     NOT NULL,
    host       text     NOT NULL,
    weekday    smallint NOT NULL CHECK (weekday BETWEEN 0 AND 6),
    start_time time     NOT NULL,
    end_time   time     NOT NULL
);

CREATE TABLE IF NOT EXISTS tracks (
    id               serial PRIMARY KEY,
    title            text   NOT NULL,
    artist           text   NOT NULL,
    album            text   NULL,
    duration_seconds int    NOT NULL CHECK (duration_seconds > 0),
    -- NOT NULL + default '{}' so "no tags" has exactly one representation ([] in JSON, never null).
    tags             text[] NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS plays (
    id        bigserial PRIMARY KEY,
    track_id  int         NOT NULL REFERENCES tracks (id),
    played_at timestamptz NOT NULL DEFAULT now()
);

-- now-playing always reads the most recent play.
CREATE INDEX IF NOT EXISTS plays_played_at_idx ON plays (played_at DESC);

-- One row per migrated route: the proxy's source of truth for how each route is served.
CREATE TABLE IF NOT EXISTS route_rules (
    route          text PRIMARY KEY CHECK (route LIKE '/%'),
    mode           text        NOT NULL CHECK (mode IN ('legacy', 'shadow', 'canary', 'go')),
    canary_percent int         NOT NULL DEFAULT 0 CHECK (canary_percent BETWEEN 0 AND 100),
    -- JSON fields that legitimately differ between implementations (timestamps, request ids)
    -- and must not count as a shadow mismatch.
    ignore_fields  text[]      NOT NULL DEFAULT '{}',
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS shadow_comparisons (
    id            bigserial PRIMARY KEY,
    route         text        NOT NULL,
    method        text        NOT NULL,
    path          text        NOT NULL,
    legacy_status int         NOT NULL,
    go_status     int         NOT NULL,
    match         boolean     NOT NULL,
    diffs         text[]      NOT NULL DEFAULT '{}',
    legacy_ms     int         NOT NULL,
    go_ms         int         NOT NULL,
    error         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Matches the dashboard query (latest comparisons per route) and the retention cleanup.
CREATE INDEX IF NOT EXISTS shadow_comparisons_route_created_idx
    ON shadow_comparisons (route, created_at DESC);
