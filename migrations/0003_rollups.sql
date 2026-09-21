-- Hourly pre-aggregates. The history endpoints read these and nothing else,
-- which is what lets raw partitions be dropped after RETENTION_DAYS (§9.3).

-- Bucket boundaries are UTC instants, deliberately. Bucketing by local hour is
-- ambiguous on the April DST transition, when 02:00–02:59 Sydney time happens
-- twice. The API renders bucket_start in Australia/Sydney with its UTC offset,
-- so both occurrences are distinguishable to a reader.
CREATE TABLE otp_stop_hourly (
    bucket_start  timestamptz NOT NULL,
    service_date  date        NOT NULL,
    stop_id       text        NOT NULL,
    route_id      text        NOT NULL,
    direction_id  smallint    NOT NULL DEFAULT -1,   -- -1 means unknown; NULL would break the PK
    n_obs         integer     NOT NULL,
    n_early       integer     NOT NULL,
    n_on_time     integer     NOT NULL,
    n_late        integer     NOT NULL,
    n_very_late   integer     NOT NULL,
    n_skipped     integer     NOT NULL,
    n_cancelled   integer     NOT NULL,
    delay_p50_s   integer,
    delay_p90_s   integer,
    delay_mean_s  real,
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (bucket_start, stop_id, route_id, direction_id)
);

-- The history endpoint is always "one stop, a time range, newest first".
CREATE INDEX otp_stop_hourly_by_stop
    ON otp_stop_hourly (stop_id, bucket_start DESC);

CREATE TABLE otp_route_hourly (
    bucket_start  timestamptz NOT NULL,
    service_date  date        NOT NULL,
    route_id      text        NOT NULL,
    direction_id  smallint    NOT NULL DEFAULT -1,
    n_obs         integer     NOT NULL,
    n_early       integer     NOT NULL,
    n_on_time     integer     NOT NULL,
    n_late        integer     NOT NULL,
    n_very_late   integer     NOT NULL,
    n_skipped     integer     NOT NULL,
    n_cancelled   integer     NOT NULL,
    delay_p50_s   integer,
    delay_p90_s   integer,
    delay_mean_s  real,
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (bucket_start, route_id, direction_id)
);

CREATE INDEX otp_route_hourly_by_route
    ON otp_route_hourly (route_id, bucket_start DESC);

-- One row per named job. The watermark is the exclusive upper bound of what has
-- been rolled up. Retention refuses to drop a partition whose service_date is
-- not fully below this watermark.
CREATE TABLE rollup_state (
    name       text        PRIMARY KEY,
    watermark  timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO rollup_state (name, watermark)
VALUES ('hourly', '2000-01-01T00:00:00Z')
ON CONFLICT DO NOTHING;
