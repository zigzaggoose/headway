-- Raw delay observations, range-partitioned by service date so retention is a
-- DROP TABLE. Daily partitions are created by the maintenance job (§9.3).

CREATE TABLE observations (
    -- Partition key. Must be in the primary key: Postgres requires every unique
    -- constraint on a partitioned table to include the partition key.
    service_date      date        NOT NULL,
    feed_id           text        NOT NULL,
    trip_id           text        NOT NULL,
    stop_sequence     integer     NOT NULL,
    -- The feed's own header timestamp, not our clock. Part of the natural key
    -- so that replaying the same feed response twice is a no-op.
    feed_ts           timestamptz NOT NULL,

    stop_id           text        NOT NULL,
    route_id          text,               -- NULL when unmatched
    direction_id      smallint,           -- NULL when unmatched

    arrival_delay_s   integer,
    departure_delay_s integer,
    -- The single number every query uses. See §9.4 for the derivation rules.
    observed_delay_s  integer,

    -- Raw integers from the feed, NOT a Go enum and NOT a Postgres enum.
    -- TfNSW emits at least one TripDescriptor value the current GTFS-realtime
    -- specification no longer defines; a constrained type would reject it.
    trip_rel          smallint    NOT NULL,
    stop_time_rel     smallint    NOT NULL,

    matched           boolean     NOT NULL,
    vehicle_id        text,
    ingested_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (service_date, feed_id, trip_id, stop_sequence, feed_ts)
) PARTITION BY RANGE (service_date);

-- A default partition means a write for an unexpected service_date never fails.
-- A non-empty default partition is an alert (§10), not a normal condition.
CREATE TABLE observations_default PARTITION OF observations DEFAULT;

-- Created per-day by the maintenance job; this is the shape:
-- CREATE TABLE observations_2026_09_21 PARTITION OF observations
--     FOR VALUES FROM ('2026-09-21') TO ('2026-09-22');
