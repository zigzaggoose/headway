-- When the observed stop visit was scheduled, from the timetable: the rollup
-- buckets by it (§6.3, §16 q12). NULL on an unmatched observation, which is
-- bucketed by feed_ts as before.
--
-- Bucketing by the last observation's feed_ts alone put a visit in the hour
-- the feed last mentioned it. TfNSW keeps cancelled and replacement trips in
-- the feed for hours after their time (measured 2026-09-23: 122 of 143
-- cancellation rows more than 6 h late), so a morning's cancellations were
-- counted in the evening.
--
-- Nullable with no default, so adding it rewrites nothing: on a partitioned
-- table this is a catalogue change on every partition, not a table rewrite.
-- Rows written before this migration stay NULL and keep the old bucketing.

ALTER TABLE observations ADD COLUMN scheduled_at timestamptz;
