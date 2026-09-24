# Load test

`PROJECT.md` §13's latency targets, measured on 2026-09-23/24.

## Setup

- **Tool:** `hey` v0.1.4 (`go install github.com/rakyll/hey@v0.1.4`, outside the
  module), `hey -z 5m -c 10 -q 5`: ten workers at five requests a second each,
  **50 RPS for five minutes, 15,000 requests per run.**
- **Machine:** the development laptop, an Apple Silicon Mac (arm64), service
  and Postgres 18.6 (Docker) on the same host, over localhost. The VM has one
  vCPU; the CPU figures below are what carries over, the wall-clock ones less so.
- **Service:** the real binary polling all five live feeds throughout, with
  their real timetables loaded. `HTTP_RATE_LIMIT_RPS=1000` so the test did not
  throttle itself (the default of 50 per IP would sit exactly at the test rate).
  `PPROF_ADDR=127.0.0.1:6060` for profiles.
- **Data:** a throwaway database with 30 days of **synthetic** hourly rollups
  for every real route (219k rows) and every real stop/route/direction
  combination (5.08M rows), because the real rollups covered hours, not a
  month. Dropped afterwards.
- **Targets:** `/v1/lines/APS_1a/now` (the T8); stop history for Central
  Platform 4 (`2000324`), the stop with the most route/direction rows; route
  history for `APS_1a`. History over 30 days, hourly buckets.

## Results

| Endpoint | p50 | p95 | p99 | Target (§13) | Status codes |
|---|---:|---:|---:|---|---|
| `/v1/lines/{id}/now` | 0.8 ms | 1.4 ms | 2.2 ms | p95 ≤ 100 ms, p99 ≤ 250 ms | 15,000 × 200 |
| `/v1/stops/{id}/history`, 30 d — **before** | 99.4 ms | 123.4 ms | 148.0 ms | p95 ≤ 300 ms | 15,000 × 200 |
| `/v1/stops/{id}/history`, 30 d — **after** | 19.1 ms | 24.0 ms | 27.6 ms | p95 ≤ 300 ms | 15,000 × 200 |
| `/v1/lines/{id}/history`, 30 d | 15.5 ms | 21.1 ms | 24.9 ms | p95 ≤ 300 ms | 15,000 × 200 |

All within target, and every request succeeded.

## What profiling found, and what was fixed

**Stop history read ~24,000 rows per request.** A stop's hourly rollups are one
row per route and direction, and Central Platform 4 has dozens of both: 30 days
was 24,412 rows, shipped to Go and combined into 720 buckets. The CPU profile
under load (30 s, `/debug/pprof/profile`) showed **212 % CPU** — two cores busy
at 50 RPS, which a one-vCPU VM could not have given — with 44 % of it in
`pgx.CollectRows` reading rows off the socket, then garbage collection and the
weighted-median combination.

**First attempt, rejected: aggregate in SQL.** Moving the combination into
Postgres (window functions for the weighted median) made it **120× worse**:
p50 12.2 s, 0.9 requests a second. `EXPLAIN ANALYZE` showed why. The custom
plan took 83 ms — no better, because Postgres still sorted the same 24,412 rows
twice — and after a few executions pgx's prepared statement got the *generic*
plan, which estimates `$4 IS NULL OR route_id = $4` at one row and chose nested
loops over the materialised CTE: **13.4 s**.

**The fix: do it once, at rollup time.** The rollup now also writes one row per
stop and hour for all routes together (`route_id = '~all'`), computed from the
raw observations, so its percentiles are **exact** rather than approximated.
Stop history without a route or direction filter reads those: 720 rows for 30
days. The history query was also split into one statement per filter
combination, so no plan ever depends on a `$n IS NULL OR` guess.

Result: **p50 99 → 19 ms, p99 148 → 28 ms, and CPU 212 % → 13.6 %** of one core
at 50 RPS.

## The ingest path: no worker pool

§12 asks for a worker pool between decode and match only if profiling shows the
poller goroutine is the bottleneck. It is not. `BenchmarkPoll_DecodeAndMatch_RecordedTrains`
(`internal/match`) decodes and matches a recorded 3,939-update trains response
in **6.6 ms** (7.2 MB, 60k allocations) against a 15 s poll interval: 0.04 % of
the budget. Buses, the largest feed and not enabled, are ~5× the updates. The
benchmark matches against the test timetable; the full one adds map lookups,
which does not change the conclusion at this margin.

## Reproducing

```sh
PPROF_ADDR=127.0.0.1:6060 HTTP_RATE_LIMIT_RPS=1000 make run
hey -z 5m -c 10 -q 5 http://localhost:8080/v1/lines/APS_1a/now
curl -o cpu.pprof "http://127.0.0.1:6060/debug/pprof/profile?seconds=30"   # during a run
go tool pprof -top bin/transitlateagain cpu.pprof
```
