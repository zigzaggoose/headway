# Storage

`PROJECT.md` §13's storage rows. **What exists so far is one evening of data on
a laptop**, measured 2026-09-23; the daily figures below are projections from
it, marked as such, and must be replaced after a full day on the VM.

## Raw observations

| | Measured |
|---|---|
| Bytes per row, heap + indexes | **265.9** (`observations_2026_09_23`: 42,139 rows, 11 MB); 276.9 on 2026-09-21 |
| Rows written, first minutes after start, five feeds (23:21, late evening) | 3,201 in 105 s: trains 794, CBD light rail 858, metro 784, Parramatta 513, ferries 252 |
| Rows written, trains alone, off-peak steady state (2026-09-21) | ~670 a minute |

The change filter is what keeps this small: 100 % of 3,939 train updates were
identical across two polls 15 s apart (2026-09-21), so a trip running to time
writes one row per stop per day rather than one per poll.

**Projection, not measurement:** trains at ~270 MB a day (2026-09-21 rate). The
first minutes above put the four smaller feeds together at about the trains'
row count, but minutes just after a start include each feed's cold-filter
burst, so doubling is an upper estimate. At the deployed `RETENTION_DAYS=7`, that is on the order of 4 GB of raw
partitions against the §13 target of ≤ 10 GB, on a 20 GB disk.

## Rollups

One real hour, trains only (21:00–22:00 on 2026-09-23): **7,105 raw rows**
became **646 rollup rows** (610 stop, 36 route) — before the all-routes stop
rows were added, which add one row per stop per hour.

| Table | Rows | Size | Note |
|---|---:|---:|---|
| `otp_stop_hourly` | 610 | 184 kB | mostly page overhead at this size |
| `otp_route_hourly` | 36 | 48 kB | one 8 kB page per table and index minimum |

At this size the per-row figure is dominated by empty pages, so a reduction
ratio from this hour would mislead. The synthetic 30-day load-test database
gives an upper bound for a month of rollups: 5.08M stop rows and 219k route
rows in 1.26 GB with every stop, route and direction present every hour — real
rollups are sparser, because an hour with no visit writes no row.

## To measure on the VM

After 24 hours running there, fill these into §13:

```sql
SELECT pg_size_pretty(pg_total_relation_size('observations_YYYY_MM_DD'));   -- raw, one full day
SELECT pg_size_pretty(pg_total_relation_size('otp_stop_hourly') + pg_total_relation_size('otp_route_hourly'));
SELECT pg_size_pretty(pg_database_size(current_database()));
```

and the reduction: `1 − rollup bytes for the day / raw bytes for the day`.
