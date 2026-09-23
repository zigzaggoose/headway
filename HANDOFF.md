# HANDOFF.md

Session state as of **2026-09-23**. Delete or rewrite this file when it stops
being true. Permanent rules live in `CLAUDE.md`; design lives in `PROJECT.md`.

## Run these first

```sh
open -a Docker                      # the daemon does not survive a reboot
docker start headway-dev-pg         # or the docker run in README.md if it is gone
make lint && make test && make test-integration
```

Everything should be green, and CI (`.github/workflows/ci.yml`) runs the same.
`make lint` now includes staticcheck, pinned to v0.8.1 and run through `go run`.
If `test-integration` skips, `DATABASE_URL_TEST` is not set — it is in `.env`,
which `make` sources but a bare `go test` does not.

**Check exit codes, not piped output.** A commit was once pushed with a failing
test because `make test-integration | grep | tail` hid the status.

## Where we are

**Stage 2 is complete** (every §12 box ticked, CI green). **Stage 1 has two open
boxes, both the VM**, deferred by the user's choice — not paying yet.

The service polls the Sydney Trains feed every 15 s, matches updates against the
daily timetable (99.67 % live), stores them in daily partitions, rolls them up
hourly, and serves `/v1/lines`, `/v1/lines/{id}/now`, `/v1/stops/{id}/now`, both
history endpoints, `/v1/admin/stats`, `/healthz` and `/readyz`. The README has the
measured numbers.

## Do this next

1. **Stage 3** (§12): more feeds, the load test, storage measurements — in progress.
   §16 q12 is answered and built (2026-09-23): rollups bucket by `scheduled_at`
   and roll each hour up three hours after it ends, so `/history` runs up to four
   hours behind.
2. **Deploy** when the user is ready to pay: BinaryLane Standard 1 GB, Sydney,
   Ubuntu 24.04. Clone to `/opt/headway`, put `.env` there with
   `POSTGRES_PASSWORD`, `make up`. Write `deploy/vm-bootstrap.md` from what is
   actually run. Stage 1's last two boxes close there.

## Found in the post-Stage-2 end-to-end test (2026-09-23)

A fresh Compose stack from an empty volume, every endpoint and error path live,
a restart on existing data, memory, graceful stop. Fixed and committed:

- **Rows written before the timetable loaded stayed unmatched for good** — the
  change filter suppressed their matched version as unchanged. Fixed; regression
  test fails without it.
- **Every readiness probe during startup logged an ERROR** (503). 503 no longer
  gets the extra error line.
- `/v1/lines` sorted TfNSW's unnamed `RTTA_*` routes first. Now last.

Measured: ready in ~26 s from empty (timetable download 22.5 s), 2 s on restart;
service 141 MiB and Postgres 300 MiB under Compose; graceful stop in under a
second with 0 rows lost.

## Things that are true and easy to forget

- **TfNSW's schedule endpoint returns 502 often.** The loader retries after
  15 minutes and the matcher keeps the version already in Postgres. An ERROR line
  for it is real but not an outage.
- **The feed never sends `stop_sequence`, `start_date`, `direction_id` or a
  vehicle id.** All of them come from the timetable or not at all.
- **`REPLACEMENT` (5) is ~15–18 % of updates.** Schedule relationships stay raw
  integers everywhere.
- **The dev database's `public` schema has real daily partitions.** Tests that
  inspect `pg_class` must scope to their own schema (go through
  `'observations'::regclass`) or they see those too.
- **The API key is real and in `.env`** (gitignored, mode 0600). It was pasted into
  an early session transcript; worth rotating if that transcript is shared.
- `POSTGRES_PASSWORD` in `.env` was generated locally for Compose; the VM gets
  its own.

## Contract decisions the user made (all in §15)

`not_found` code, `summary.early`, `limit` caps, `reasons` beside the envelope;
history percentiles approximate when combining rows; `status: "cancelled"`.
Ask before any other change to §7.
