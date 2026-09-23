# HANDOFF.md

Session state as of **2026-09-23**; the "Completed this session" and "Decisions"
sections below are from 2026-09-21. Delete or rewrite this file when it stops
being true. Permanent rules live in `CLAUDE.md`; design lives in
`PROJECT.md`.

## Run these first

```sh
open -a Docker                      # the daemon does not survive a reboot
docker start headway-dev-pg         # or the docker run in README.md if it is gone
make lint && make test && make test-integration
```

Everything should be green. If `test-integration` skips, `DATABASE_URL_TEST` is not
set — it is in `.env`, which `make` sources but a bare `go test` does not.

## Where we are

**Stage 1 (MVP), 13 of 15 items done** — every code item. The two left are the VM
and the deployed demo, deferred by the user. Stage 2 has `servicetime` done.

Working end to end today (2026-09-23): the service starts, applies migrations, polls
the live TfNSW feed every 15 s, decodes ~3,250 updates per poll, converts them with
`match.Unmatched`, and writes rows through the change filter and batch writer. A 50 s
live run wrote 3,887 rows from 4 polls, 0 failed, 0 dropped, and flushed the final
batch on SIGINT. Trip-level updates (~150 per poll) are skipped until the matcher
can expand them.

Observed on that run and not yet explained: `observed_delay_s` ranged 0 to 5,482 —
no negative (early) value at all across 3,887 rows. Check at Stage 2 whether TfNSW
clamps early running to zero, because that would bias every on-time percentage.

## Do this next

**Stage 2, starting with `internal/gtfsstatic`**: download the `sydneytrains` schedule
bundle, hash, unzip, parse, insert, activate (§9.1, §12 Stage 2). First thing to check
once it loads: **whether the bundle contains the feed's operational route ids**
(`RTTA`, `NSN_1a`, `IWL`…). If it does not, the match-rate target cannot be met and
§9.1 needs a mapping before the matcher is worth writing.

Stage 1 is code-complete (2026-09-23). The two unticked items are the VM and the
deployed demo, **deferred by the user's choice — not paying yet**. Rows are written
only while the laptop runs, and history missed is not recoverable; raise it again
when Stage 2 has something worth showing. `deploy/` is ready: clone to
`/opt/headway`, put `.env` there with `POSTGRES_PASSWORD`, `make up`.

## Completed this session

Eleven commits. Each component is tested and pushed:

| Package | State | Coverage |
|---|---|---|
| `internal/config` | 40 env vars, validated, secrets unprintable | 86.7 % |
| `internal/feed` | client, account-wide limiter, poller | 88.8 % |
| `internal/gtfsrt` | decoder, all nil handling | 93.2 % |
| `internal/ingest` | filter, queue, batch writer | 95.8 % |
| `internal/store` | pool, migration runner, retry policy | 87.3 % |
| `cmd/fixturedump` | `capture` and `derive` subcommands | — |

237 tests and subtests, all green under `-race`. Five migrations apply cleanly to
PostgreSQL 18.6.

## Decisions made this session

These are all in §15 with full reasoning. The ones most likely to be second-guessed:

1. **Observations are keyed on `stop_id`, not `stop_sequence`** (`migrations/0005`).
   The TfNSW feed never sends `stop_sequence` — measured at 0 of 3,836 updates — so
   it can only come from the timetable, which an unmatched observation has no access
   to. The user chose this over a sentinel value. Cost: a loop service revisiting one
   stop within a single feed timestamp loses the second visit. That is asserted by a
   test, not left to be discovered.
2. **The poller calls a `Handler` on its own goroutine** instead of emitting to a
   channel, contradicting the original §4.2 (which has been updated). §9.4 already
   puts decode and match on that goroutine.
3. **The decoder uses `proto.UnmarshalOptions{AllowPartial: true}`.** GTFS-realtime
   is proto2 with *required* fields, so a strict unmarshal lets one malformed entity
   destroy a whole 3,939-update response.
4. **Secrets are a `Secret` type, not a `redact()` helper**, superseding §8.2.
5. **The rate limiter and daily budget are one hand-rolled object**, not
   `x/time/rate` plus a counter — both limits are per account and must be answered
   together. Direct dependencies stayed at three.
6. **`RawUpdate.TripLevel`** marks a trip-level update. 103 of 488 live entities
   carry no stop-time updates at all; that is how a cancellation arrives, and without
   this they decode to nothing.

## What the live feed actually contains

Measured 2026-09-21 and recorded in §9.1. These overturn assumptions in the original
design, and the matcher depends on them:

- **No `stop_sequence`, ever.** Match order 1 (`trip_id` + `stop_sequence`) can never
  fire. `trip_id` + `stop_id` is the primary path.
- **No vehicle id, no direction id.** The `VehicleDescriptor` is present on all 488
  trip updates and completely empty. `direction_id` will always be `-1` in rollups.
- **`REPLACEMENT` (5) is 17–18 % of updates.** Not exotic.
- **44 % of each payload is TfNSW extension fields** the canonical bindings ignore,
  correctly.
- Route ids are operational codes (`RTTA`, `ESI`, `IWL`, `APS`, `NSN`…), not `T1`/`T2`.
  **Check at Stage 2 whether the `sydneytrains` schedule bundle actually contains
  them** — `IWL` looks like Inner West Light Rail in a feed named for trains, and if
  the bundle lacks those routes the match rate target will not be met.

## Known issues

- **All rows land in `observations_default`.** No maintenance job exists to
  pre-create daily partitions, so retention is currently impossible. Expected until
  `internal/rollup` (Stage 3); §9.3 case 4. It is a paging alert in production.
- **`INGEST_QUEUE_SIZE` must exceed one poll's updates.** One poll submits ~3,939 in
  a burst; the default 8,192 leaves ~2× headroom for the trains feed alone. Stage 3
  adds buses, which is far larger. Re-check then.
- **`make up`, `make fixtures`, `make loadtest` do not work yet** — they reference
  `deploy/docker-compose.yml`, live API quota, and Stage 3 respectively.
- No CI. `.github/workflows/ci.yml` is Stage 2.

## Easy to misunderstand

- **`PROJECT.md` is authoritative, and it is kept true.** If code contradicts it,
  that is a bug in `PROJECT.md` to be fixed in the same change — not a divergence to
  live with. Several sections were corrected this session when reality disagreed.
- **`§7.3` is out of date in three places** (listed under "Contradictions" below).
  Trust the code there.
- **`observations.stop_sequence` is nullable now.** The DDL block in §6.2 still shows
  the original key with a `SUPERSEDED` comment pointing at `migrations/0005`.
- **Test counts include subtests.** `go test -v | grep -c '=== RUN'` is how the
  numbers above were produced.
- **The API key is real and works.** It is in `.env` (gitignored, mode 0600) and was
  pasted into this session's transcript — worth rotating if that transcript is shared.
- **`testdata/*_0001.pb` and `*_0002.pb` are 15 seconds apart** and decode to
  identical core fields. That is not a bad capture: the producer republishes on a
  15 s header cadence while changing core content far less often.

## Waiting on the user

Not blocking, but Stage 1 cannot finish without the first two:

1. **BinaryLane VM** — Standard 1 GB, Sydney, Ubuntu 24.04, x86-64 (so builds target
   `linux/amd64`). Replaces Oracle (debit card rejected) and Azure for Students (expiry
   cliff); §16 q6 and the §15 row of 2026-09-23 have the sizing. Buy it once `main`
   writes rows, not before — until then the VM would capture nothing.
   Paste what gets run on the VM so `deploy/vm-bootstrap.md` is written from reality.
2. **Confirm the TfNSW plan** is the default 60,000/day at 5/s. `FEED_RATE_LIMIT_RPS=4`
   and `FEED_DAILY_BUDGET=55000` are sized against exactly that.
3. §16 questions 2–5, 7, 8 have working defaults and block nothing.

## Contradictions between PROJECT.md and the code

Fix these when next touching the area — or now, if the next session prefers a clean
start:

| §7.3 says | Code does | Why |
|---|---|---|
| `StopSequence int32` | `*int32` | Nullable since `migrations/0005`. |
| `type Filter interface{ Admit(Observation) bool }` | concrete `type Filter struct` | One implementation; an interface would be speculative. |
| `NewWriter(pool, in, cfg)` | also takes `*slog.Logger` | The writer logs failed batches. |

§5 also omits `internal/ingest/pipeline_integration_test.go`.
