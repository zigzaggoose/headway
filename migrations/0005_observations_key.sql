-- The TfNSW realtime feeds never send stop_sequence: measured at 0 of 3,836
-- stop-time updates on 2026-09-21 (§9.1). It can only come from the timetable,
-- which means a matched observation has one and an unmatched observation never
-- will -- and unmatched observations must be storable, because the unmatched
-- rate is the signal that the matcher is drifting (§15).
--
-- So stop_id takes its place in the key and stop_sequence becomes a nullable
-- column, filled from the timetable when a match succeeds. The cost is a trip
-- that visits one stop twice within a single feed timestamp: the second visit
-- collides and is discarded by ON CONFLICT DO NOTHING. That is a loop service,
-- which §9.1 case 9 already treats as ambiguous, and it is rarer than an
-- unmatched update. Open Question 11.
--
-- 0002 is not edited: it has been applied, and an applied migration is
-- immutable (§14). The runner enforces that with a checksum.

ALTER TABLE observations DROP CONSTRAINT observations_pkey;

ALTER TABLE observations ALTER COLUMN stop_sequence DROP NOT NULL;

ALTER TABLE observations
    ADD CONSTRAINT observations_pkey
    PRIMARY KEY (service_date, feed_id, trip_id, stop_id, feed_ts);
