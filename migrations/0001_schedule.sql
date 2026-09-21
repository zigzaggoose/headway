-- Static GTFS: one immutable schedule version per downloaded bundle, keyed by
-- content hash, plus the timetable tables that hang off it. See PROJECT.md §6.2.

CREATE TABLE schedule_versions (
    id             bigserial   PRIMARY KEY,
    feed_id        text        NOT NULL,
    sha256         bytea       NOT NULL,
    etag           text,
    last_modified  timestamptz,
    loaded_at      timestamptz NOT NULL DEFAULT now(),
    activated_at   timestamptz,
    active         boolean     NOT NULL DEFAULT false,
    trip_count     integer     NOT NULL DEFAULT 0,
    stop_time_count integer    NOT NULL DEFAULT 0
);

-- Exactly one active version per feed. A partial unique index enforces it in the
-- database rather than in application code, so a double-activation is an error,
-- not a silent overwrite.
CREATE UNIQUE INDEX schedule_versions_one_active
    ON schedule_versions (feed_id) WHERE active;

-- Re-downloading an unchanged bundle must be a no-op. The content hash is the
-- natural key: TfNSW does not always change ETag or Last-Modified when the
-- content is identical, and does not always keep them stable when it is.
CREATE UNIQUE INDEX schedule_versions_content
    ON schedule_versions (feed_id, sha256);

CREATE TABLE routes (
    version_id   bigint   NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    route_id     text     NOT NULL,
    agency_id    text,
    short_name   text,
    long_name    text,
    route_type   smallint NOT NULL,
    PRIMARY KEY (version_id, route_id)
);

CREATE TABLE stops (
    version_id     bigint NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    stop_id        text   NOT NULL,
    stop_code      text,
    name           text   NOT NULL,
    lat            double precision,
    lon            double precision,
    parent_station text,
    location_type  smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, stop_id)
);

-- Stop search by name is a user-facing feature; trigram or full-text would be
-- overkill for a few thousand stops, so a plain lower(name) index backs a
-- prefix search and nothing more.
CREATE INDEX stops_name_lower ON stops (version_id, lower(name));

CREATE TABLE trips (
    version_id   bigint   NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    trip_id      text     NOT NULL,
    route_id     text     NOT NULL,
    service_id   text     NOT NULL,
    direction_id smallint,
    headsign     text,
    PRIMARY KEY (version_id, trip_id)
);

-- "Which trips belong to route R" is how the schedule cache is built and how
-- /v1/lines/{id}/now resolves a route to trips.
CREATE INDEX trips_by_route ON trips (version_id, route_id);

CREATE TABLE stop_times (
    version_id    bigint   NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    trip_id       text     NOT NULL,
    stop_sequence integer  NOT NULL,
    stop_id       text     NOT NULL,
    -- Seconds from the start of the service day. MAY exceed 86400 for services
    -- running past midnight. Storing seconds rather than `time` is deliberate:
    -- `time` cannot represent 25:10:00.
    arrival_s     integer,
    departure_s   integer,
    pickup_type   smallint NOT NULL DEFAULT 0,
    drop_off_type smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, trip_id, stop_sequence)
);

-- "Which trips call at stop S" backs the stop-level endpoints and the rollup's
-- route attribution for unmatched-but-known stops.
CREATE INDEX stop_times_by_stop ON stop_times (version_id, stop_id);

CREATE TABLE calendar (
    version_id bigint  NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    service_id text    NOT NULL,
    mon boolean NOT NULL, tue boolean NOT NULL, wed boolean NOT NULL,
    thu boolean NOT NULL, fri boolean NOT NULL, sat boolean NOT NULL,
    sun boolean NOT NULL,
    start_date date NOT NULL,
    end_date   date NOT NULL,
    PRIMARY KEY (version_id, service_id)
);

CREATE TABLE calendar_dates (
    version_id     bigint   NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    service_id     text     NOT NULL,
    service_date   date     NOT NULL,
    exception_type smallint NOT NULL,   -- 1 = added, 2 = removed
    PRIMARY KEY (version_id, service_id, service_date)
);
