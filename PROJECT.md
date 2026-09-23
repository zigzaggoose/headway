# Headway

Single source of truth for this repository. Claude re-reads this file at the start of every session and has no other memory of the project. Everything needed to write code here is in this file or reachable from it.

Last revised: 2026-09-21.

---

## 1. Project

Headway is a Go service that polls Transport for NSW GTFS-realtime feeds on a fixed interval, decodes the protobuf payloads, matches each realtime stop-time update against the published static timetable, and stores the resulting delay observations in PostgreSQL. It serves two kinds of question over HTTP: *what is happening on this line right now*, and *how has this stop or line performed over the last N days*. Raw observations are partitioned by service date, rolled up hourly, and dropped on a retention schedule so the whole thing fits on a free-tier Linux VM.

**Problem it solves.** TfNSW publishes realtime data and timetable data, but not on-time performance. Answering "is the T1 usually late at Strathfield on a Tuesday morning" requires you to capture the realtime feed continuously, because it is not archived in a queryable form. Headway captures it and answers the question.

**Definition of Done for v1.**

| # | Criterion | How it is verified |
|---|---|---|
| 1 | The service runs continuously on the deployment VM for 7 consecutive days without manual intervention. | `process_start_time_seconds` unchanged in `/metrics`; uptime noted in README. |
| 2 | At least two TfNSW realtime feeds are polled inside the API quota, with zero sustained 403 responses. | `headway_feed_requests_total{outcome="rate_limited"}` flat over 24 h. |
| 3 | `GET /v1/lines/{route_id}/now` returns in under 100 ms at p95 under a 50 RPS load test. | `k6` or `hey` run recorded in `docs/loadtest.md`. |
| 4 | `GET /v1/stops/{stop_id}/history` returns 30 days of hourly buckets in under 300 ms at p95. | Same load test. |
| 5 | Match rate (realtime updates resolved to a scheduled trip) is at or above 90 % on the trains feed. | `headway_match_rate` gauge, and the `/v1/admin/stats` endpoint. |
| 6 | Freshness lag from feed timestamp to queryable is under 30 s at p95. | `headway_freshness_lag_seconds` histogram. |
| 7 | Raw partitions older than the retention window are dropped automatically and the rollups still answer historical queries. | Partition list shrinks; history endpoint still returns data for dates with no raw partition. |
| 8 | `docker compose up` on a clean machine brings up Postgres and the service, applies migrations, and serves traffic with no manual SQL. | Followed from a clean clone on the VM. |
| 9 | CI runs `go vet`, `go test ./...` and a build on every push, including integration tests against a real Postgres. | Green badge on `main`. |
| 10 | A graceful shutdown loses no accepted batch. | Integration test `TestShutdownFlushesPendingBatch`. |

v1 is done when all ten hold at the same time. Stage 4 items (Grafana, Next.js) are not part of the Definition of Done — see §12.

---

## 2. Non-goals

Scope creep is the main risk on this project. Headway does **not** do any of the following, and a request to add one of them is a change to this file first, not a branch.

- **No trip planning, routing or journey search.** No shortest path, no transfers, no fare calculation. TfNSW already has a Trip Planner API for that.
- **No map rendering, no shape/geometry storage.** `shapes.txt` is skipped at load time. Stop coordinates are stored only so the API can return them; nothing draws a line on a map.
- **No user accounts, authentication, sessions or multi-tenancy.** The HTTP API is public and read-only. There are no writes over HTTP.
- **No push notifications, alerting to end users, email, or SMS.**
- **No service-alerts feed ingestion.** Only trip updates. Vehicle positions are an explicit stretch item and are not ingested in v1 (see §16).
- **No prediction or forecasting.** Headway reports what the feed said. It does not estimate future delays or build a model.
- **No Kubernetes, no Terraform, no service mesh, no message broker.** One binary, one Postgres, one VM, Docker Compose. Introducing Kafka or NATS here would be resume theatre and is explicitly rejected.
- **No ORM.** SQL is written by hand in `internal/store`.
- **No GraphQL.** REST with JSON.
- **No horizontal scaling, leader election or distributed locking.** Exactly one instance writes. Concurrency is inside the process.
- **No backfill of historical data from before first deployment.** TfNSW publishes a historical archive; loading it is out of scope.
- **No support for GTFS features the NSW feeds do not use**: `frequencies.txt`, `transfers.txt`, `pathways.txt`, `levels.txt`, `fare_*` are parsed past and discarded.
- **No non-NSW agencies.** No abstraction layer for "any GTFS producer". The feed catalogue is a config file, not a plugin system.
- **No mobile app.** The optional Next.js front end (Stage 4) is one page.

---

## 3. Tech stack

Versions are pinned. Where a version is stated below it was checked against the upstream project on 2026-09-21; check `go.mod` and `deploy/docker-compose.yml` for the authoritative value, and treat a mismatch between this table and those files as a bug in this table.

| Technology | Version | Used for | Chosen over | Why |
|---|---|---|---|---|
| Go | 1.27.1 (toolchain directive `go 1.27`) | The whole service | Node.js / TypeScript | One static binary, no runtime on the VM, goroutines make N concurrent pollers trivial, and the learning goal is Go. Node would have been fine for this I/O-bound workload — be ready to say so. Go 1.26 also works; 1.27 is current. |
| PostgreSQL | 18.6 | All storage | SQLite; TimescaleDB; ClickHouse | Native declarative range partitioning and `ON CONFLICT` cover the entire storage design without an extension. SQLite cannot be written concurrently from a batch writer while being read by the API under load. TimescaleDB adds an extension to install and maintain on a free-tier VM for compression we can get from rollups. ClickHouse is the right answer at 100× this volume and the wrong answer at this one. |
| jackc/pgx | v5.9.2 or later v5 | Postgres driver and connection pool | `database/sql` + `lib/pq` | `pgx.CopyFrom` uses the Postgres COPY protocol, which is the fastest way to land a batch of observations. `pgxpool` gives a pool with per-connection health checks. pgx v5.9.2 is the minimum acceptable version: 5.9.2 fixed a SQL-injection issue affecting the non-default simple protocol. Do not pin below it. |
| MobilityData/gtfs-realtime-bindings (Go) | v1.0.0 | Generated protobuf structs for GTFS-realtime | Hand-generating from the `.proto` | The module is `github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs`. **Note:** this module used to carry no semver tags, and this table long said to expect a `v0.0.0-<date>-<commit>` pseudo-version. As of 2026-09-21 it resolves to **v1.0.0**, which is what `go.mod` now pins. See §9.1 for why the TfNSW feeds may still need a locally generated binding. |
| google.golang.org/protobuf | latest v1 | `proto.Unmarshal` | `github.com/golang/protobuf` | The old module is deprecated and forwards to this one. |
| `net/http` `ServeMux` (stdlib) | — | HTTP routing | chi, gorilla/mux, echo, gin | Since Go 1.22 the standard `ServeMux` supports method-and-wildcard patterns such as `GET /v1/lines/{id}/now`, which is the entire routing requirement here. Zero dependencies to justify in an interview. |
| `log/slog` (stdlib) | — | Structured logging | zerolog, zap | Standard library, JSON handler built in, good enough at this volume. |
| `time/tzdata` (stdlib, blank import) | — | Embedding the IANA timezone database in the binary | Installing `tzdata` in the runtime image | The runtime image is distroless and has no timezone database. `import _ "time/tzdata"` makes `time.LoadLocation("Australia/Sydney")` work anyway. Without it, every DST calculation silently falls back to UTC. This is a real failure mode; see §9.2. |
| prometheus/client_golang | latest v1 | `/metrics` (Stage 4) | Hand-written text exposition | Histograms with correct bucket accounting are tedious to hand-roll. Not a dependency until Stage 4. |
| Docker + Docker Compose | Compose v2 | Local dev and deployment | systemd unit + host Postgres | One `docker compose up` reproduces production on a laptop, and the learning goal includes Docker. |
| Base image (build) | `golang:1.27-bookworm` | Compile stage | `golang:1.27-alpine` | CGO is off, so libc does not matter; bookworm avoids musl surprises if CGO is ever needed. |
| Base image (runtime) | `gcr.io/distroless/static-debian12:nonroot` | Runtime stage | `alpine`, `scratch` | No shell, no package manager, runs as non-root, ~2 MB. Works with `CGO_ENABLED=0` static binaries on `linux/amd64`. |
| Postgres image | `postgres:18-alpine` | Local and deployed database | Managed Postgres | Free. Pin the digest in `deploy/docker-compose.yml`; verify the exact tag exists on Docker Hub before pinning a patch-level tag such as `18.6-alpine`. |
| GitHub Actions | — | CI | Drone, self-hosted runner | Free for public repos, service containers give a real Postgres for integration tests. |
| BinaryLane Standard 1 GB (x86-64, 1 vCPU, 1 GB, 20 GB NVMe), Ubuntu 24.04 LTS, Sydney | — | Deployment target | Oracle Cloud Always Free Ampere A1; Azure B2pts v2 (Students); Hetzner CAX11; BinaryLane 2 GB | AUD 4.90/month ex GST, billed hourly, no expiry and no idle reclamation, same city as the TfNSW API. Oracle rejected a debit-card signup and Azure for Students stops the capture when the credit runs out — §15, 2026-09-23. Build target is `linux/amd64`; the dev laptop is arm64, so a missed `GOARCH` produces an `exec format error` on the VM. 1 GB RAM and a 20 GB disk are the binding constraints — see §16 q6. |
| Next.js | latest stable at Stage 4 | One-page dashboard | Plain HTML + fetch | Stage 4 only, and the point is to have TypeScript in the repo. Do not start it before Stage 4. |
| k6 *or* hey | — | Load testing | wrk | Either is acceptable; record which one was used in `docs/loadtest.md`. |

### 3.1 Dependency policy

- Every new direct dependency needs a line in the Decision Log (§15) saying what it replaced and why the standard library was not enough.
- Total direct dependencies in v1 should stay at or below six.
- `go.sum` is committed. Dependabot is not configured; run `go get -u ./... && go test ./...` deliberately.

---

## 4. Architecture

### 4.1 Data flow

```
                        ┌──────────────────────────────────────────┐
                        │   api.transport.nsw.gov.au (TfNSW)       │
                        │   Authorization: apikey <KEY>            │
                        └───────────┬──────────────────┬───────────┘
                                    │                  │
                  GTFS static .zip  │                  │  GTFS-realtime protobuf
                  (daily, per feed) │                  │  (every 15 s, per feed)
                                    ▼                  ▼
                        ┌───────────────────┐   ┌──────────────────────┐
                        │ (1) schedule      │   │ (2) poller           │
                        │     loader        │   │     one goroutine    │
                        │  zip → csv → rows │   │     per feed         │
                        └─────────┬─────────┘   │  global token bucket │
                                  │             └──────────┬───────────┘
                                  │                        │ []byte
                                  │                        ▼
                                  │             ┌──────────────────────┐
                                  │             │ (3) decoder /        │
                                  │             │     normaliser       │
                                  │             │  proto → []RawUpdate │
                                  │             └──────────┬───────────┘
                                  │                        │
                                  ▼                        ▼
                     ┌─────────────────────┐    ┌──────────────────────┐
                     │ (7) schedule cache  │◄───┤ (4) matcher          │
                     │  active version,    │    │  realtime ↔ timetable│
                     │  trip → stop_times  │───►│  computes delay      │
                     └─────────────────────┘    └──────────┬───────────┘
                                                           │ []Observation
                                                           ▼
                                                ┌──────────────────────┐
                                                │ (5) change filter    │
                                                │  drop unchanged      │
                                                │  (trip,stop) values  │
                                                └──────────┬───────────┘
                                                           │
                                     ┌─────────────────────┴─────────────┐
                                     │                                   │
                                     ▼                                   ▼
                        ┌────────────────────────┐          ┌────────────────────────┐
                        │ bounded channel        │          │ (8) latest-state cache │
                        │ cap = INGEST_QUEUE_SIZE│          │  in memory, per        │
                        └───────────┬────────────┘          │  (route, stop, trip)   │
                                    │                       └───────────┬────────────┘
                                    ▼                                   │
                        ┌────────────────────────┐                      │
                        │ (6) batch writer       │                      │
                        │  COPY / upsert         │                      │
                        └───────────┬────────────┘                      │
                                    ▼                                   │
                  ┌──────────────────────────────────────┐              │
                  │  PostgreSQL                          │              │
                  │   schedule_versions, routes, trips,  │              │
                  │   stops, stop_times, calendar*       │              │
                  │   observations  (RANGE by day)       │              │
                  │   otp_stop_hourly, otp_route_hourly  │              │
                  └───────┬──────────────────────┬───────┘              │
                          │                      ▲                      │
                          │                      │                      │
                          │           ┌──────────┴───────────┐          │
                          │           │ (9) maintenance job  │          │
                          │           │  rollup → retention  │          │
                          │           │  → partition precreate│         │
                          │           └──────────────────────┘          │
                          │                                             │
                          ▼                                             ▼
                  ┌──────────────────────────────────────────────────────────┐
                  │ (10) HTTP API                                            │
                  │   /v1/lines/{id}/now      ← latest-state cache           │
                  │   /v1/stops/{id}/now      ← latest-state cache           │
                  │   /v1/stops/{id}/history  ← otp_stop_hourly              │
                  │   /v1/lines/{id}/history  ← otp_route_hourly             │
                  │   /healthz /readyz /metrics                              │
                  └──────────────────────────────────────────────────────────┘
```

Everything above runs in **one process** (`cmd/headway`). The numbers are package boundaries, not deployment units.

### 4.2 Components

**(1) Schedule loader — `internal/gtfsstatic`**

- Single responsibility: turn a static GTFS zip into a complete, immutable schedule version in Postgres, and mark it active.
- Inputs: a feed id, a URL, an HTTP client, a `*pgxpool.Pool`.
- Outputs: a `schedule_versions` row with `active = true`, plus rows in `routes`, `trips`, `stops`, `stop_times`, `calendar`, `calendar_dates` carrying that `version_id`. Returns `(versionID int64, changed bool, err error)`.
- Must not know about: realtime, delays, the HTTP API, the observation schema.
- Runs at startup and then daily on a timer.

**(2) Poller — `internal/feed`**

- Single responsibility: fetch one feed's bytes on a schedule without exceeding the upstream quota.
- Inputs: a `config.Feed`, the shared `Client` and `Limiter`, a `Handler`, a `context.Context`.
- Outputs: calls `Handler(ctx, Response{FeedID string, Body []byte, FetchedAt time.Time, StatusCode int})` on its own goroutine, and does not start the next fetch until it returns. Not a channel — see the Decision Log, and §9.4, which puts decode and match on this goroutine anyway.
- Must not know about: protobuf, GTFS semantics, Postgres. It is an HTTP client with a clock.

**(3) Decoder / normaliser — `internal/gtfsrt`**

- Single responsibility: `[]byte` → `[]RawUpdate`, with every field-presence check done here so no other package calls `GetX()` on a possibly-nil pointer.
- Inputs: `FeedResponse`.
- Outputs: `DecodedFeed{FeedID string, HeaderTimestamp time.Time, Updates []RawUpdate, Dropped int}`.
- Must not know about: the schedule, the database, what "late" means.

**(4) Matcher — `internal/match`**

- Single responsibility: resolve a `RawUpdate` to a scheduled stop-time and compute a delay in seconds.
- Inputs: `RawUpdate`, the schedule cache (7).
- Outputs: `Observation` (always — an unmatched update still produces an observation with `Matched=false`).
- Must not know about: HTTP, batching, partitions, retention.

**(5) Change filter — `internal/ingest`**

- Single responsibility: suppress observations whose delay and status are unchanged since the last write for the same `(service_date, feed_id, trip_id, stop_sequence)`.
- Inputs: `Observation`.
- Outputs: `Observation` forwarded, or dropped with a counter increment.
- Must not know about: SQL. It owns an in-memory `map[obsKey]lastWritten` with its own eviction.
- This component is the reason the storage budget works. See §9.3.

**(6) Batch writer — `internal/ingest`**

- Single responsibility: drain the bounded channel and write batches to Postgres idempotently.
- Inputs: `<-chan Observation`, a `*pgxpool.Pool`.
- Outputs: rows in `observations`; errors to the logger; counters.
- Flushes on `INGEST_BATCH_SIZE` rows or `INGEST_FLUSH_INTERVAL`, whichever comes first.
- Must not know about: feeds, HTTP, the schedule.

**(7) Schedule cache — `internal/match`**

- Single responsibility: hold the active schedule version in memory so the matcher does not hit Postgres per update.
- Inputs: a `version_id`; loads from Postgres.
- Outputs: lookups by `(trip_id)` → trip + ordered stop-times; `(route_id)` → trip ids.
- Swapped atomically behind an `atomic.Pointer[Schedule]` when a new version is loaded. Readers never block.
- Must not know about: how schedule versions get into the database.

**(8) Latest-state cache — `internal/cache`**

- Single responsibility: answer "now" queries without touching Postgres.
- Inputs: `Observation` (fed from the same place as the change filter, before the channel).
- Outputs: per-route and per-stop snapshots.
- Bounded: each poll replaces its feed's snapshot wholesale, so a trip absent from the latest poll is gone. A feed whose latest snapshot is older than `CACHE_TTL` is not served; that is checked on read, with no sweep goroutine (§15, 2026-09-23).
- Must not know about: SQL, partitions, rollups.

**(9) Maintenance job — `internal/rollup`**

- Single responsibility: keep the database small and historical queries fast.
- Three ordered steps on each run: roll up completed hours; drop partitions older than retention; pre-create partitions for the next `PARTITION_LOOKAHEAD_DAYS` days.
- Inputs: a `*pgxpool.Pool`, a clock.
- Outputs: rows in `otp_stop_hourly` / `otp_route_hourly`, `DROP TABLE` / `CREATE TABLE` DDL, a watermark in `rollup_state`.
- Must not know about: feeds, HTTP, matching. Order matters: never drop a partition that has not been rolled up.

**(10) HTTP API — `internal/api`**

- Single responsibility: map HTTP requests onto the cache and the rollup tables.
- Inputs: `*http.Request`.
- Outputs: JSON.
- Must not know about: pollers, the writer, protobuf. It reads; it never writes.
- **Must never block on the ingest path.** No shared mutex with the writer, no synchronous database write, no unbounded query.

---

## 5. Repository layout

```
headway/
├── PROJECT.md                        This file. Source of truth.
├── CLAUDE.md                         Persistent instructions for Claude. The subset of this file needed every session.
├── HANDOFF.md                        Current session state. Transient; delete when stale.
├── README.md                         Short: what it is, how to run it, a screenshot.
├── embed.go                          //go:embed migrations/*.sql. No logic; embed cannot reach out of its own directory.
├── go.mod                            Module github.com/<you>/headway, go 1.27.
├── go.sum
├── Makefile                          run, test, lint, fixtures, migrate, loadtest targets.
├── .gitignore                        Includes .env — never commit it.
├── .env.example                      Every env var with a safe placeholder value.
├── .dockerignore
│
├── cmd/
│   ├── headway/
│   │   └── main.go                   Wires config → components → signal handling. No logic.
│   ├── scheduleload/
│   │   └── main.go                   One-shot static GTFS load. Useful in dev and in a cron.
│   ├── maintain/
│   │   └── main.go                   One-shot rollup + retention. Same code as the in-process job.
│   └── fixturedump/
│       └── main.go                   Fetch one live feed, write it to testdata/ as .pb. See §11.
│
├── internal/
│   ├── config/
│   │   ├── config.go                 Loads env into a Config struct. Validates. Fails fast.
│   │   ├── env.go                    Typed env readers that collect errors instead of returning one.
│   │   ├── feeds.go                  Parses the feed catalogue JSON.
│   │   ├── config_test.go
│   │   └── feeds_test.go
│   ├── feed/
│   │   ├── poller.go                 One goroutine per feed; jittered ticker.
│   │   ├── client.go                 HTTP client, auth header, conditional requests, timeouts.
│   │   ├── limiter.go                Global token bucket + daily budget, shared by all pollers.
│   │   ├── client_test.go            Every status case in §9.5, against httptest.
│   │   ├── limiter_test.go           Rate spacing and the UTC daily rollover, on a fake clock.
│   │   └── poller_test.go
│   ├── gtfsrt/
│   │   ├── decode.go                 proto.Unmarshal → []RawUpdate. All nil checks live here.
│   │   ├── types.go                  RawUpdate, DecodedFeed, schedule-relationship constants.
│   │   └── decode_test.go            Runs against testdata/*.pb fixtures.
│   ├── gtfsstatic/
│   │   ├── load.go                   Download, hash, unzip, parse, insert, activate.
│   │   ├── csv.go                    Streaming CSV reader tolerant of BOM and unknown columns.
│   │   ├── servicedays.go            calendar + calendar_dates → does service S run on date D.
│   │   └── load_test.go              Runs against testdata/gtfs_mini.zip.
│   ├── servicetime/
│   │   ├── servicetime.go            GTFS "HH:MM:SS" (HH may exceed 23) ↔ seconds; service day maths.
│   │   └── servicetime_test.go       DST cases are the whole point of this package.
│   ├── match/
│   │   ├── schedule.go               Schedule struct, loader, atomic swap.
│   │   ├── matcher.go                RawUpdate → Observation.
│   │   ├── unmatched.go              The no-schedule conversion (§9.1 order 4). Stage 1; the matcher's fallback after.
│   │   ├── delay.go                  Delay derivation and the on-time classification.
│   │   ├── matcher_test.go
│   │   └── unmatched_test.go
│   ├── ingest/
│   │   ├── observation.go            The Observation type and its key.
│   │   ├── filter.go                 Change filter (5).
│   │   ├── writer.go                 Batch writer (6).
│   │   ├── pipeline.go               Channel wiring and shutdown ordering.
│   │   ├── filter_test.go
│   │   ├── pipeline_integration_test.go
│   │   └── writer_integration_test.go  Tagged `//go:build integration`. Holds TestShutdownFlushesPendingBatch.
│   ├── cache/
│   │   ├── latest.go                 Latest-state cache (8).
│   │   └── latest_test.go
│   ├── store/
│   │   ├── store.go                  Pool construction, ping, retries.
│   │   ├── observations.go           Batch upsert, per-trip reads.
│   │   ├── schedule.go               Reads/writes for the static tables.
│   │   ├── rollup.go                 Rollup and retention SQL.
│   │   ├── migrate.go                Embedded migration runner. No external tool.
│   │   ├── migrate_test.go           Loader rules, retry policy, secret redaction. No database needed.
│   │   └── store_integration_test.go Tagged `//go:build integration`. Schema per test.
│   ├── rollup/
│   │   ├── job.go                    Ordered maintenance run (9).
│   │   └── job_test.go
│   ├── api/
│   │   ├── router.go                 ServeMux patterns. One place that knows about paths.
│   │   ├── handlers_now.go
│   │   ├── handlers_history.go
│   │   ├── handlers_meta.go          /healthz, /readyz, /v1/admin/stats.
│   │   ├── render.go                 writeJSON, writeError. Single error envelope.
│   │   ├── middleware.go             Request id, access log, panic recovery, timeout.
│   │   └── api_test.go               httptest against a fake store.
│   └── obs/
│       ├── log.go                    slog setup, the standard field names.
│       └── metrics.go                Prometheus collectors. Stage 4.
│
├── migrations/
│   ├── 0001_schedule.sql             schedule_versions and the static tables.
│   ├── 0002_observations.sql         Partitioned parent + default partition.
│   ├── 0003_rollups.sql              otp_stop_hourly, otp_route_hourly, rollup_state.
│   └── 0004_indexes.sql              Indexes added after measuring. Empty until then.
│
├── config/
│   └── feeds.json                    Feed catalogue. No secrets. Committed.
│
├── testdata/
│   ├── sydneytrains_tripupdate_0001.pb     Recorded live response, ~peak hour.
│   ├── sydneytrains_tripupdate_0002.pb     Same feed 15 s later — the dedupe case.
│   ├── sydneytrains_cancelled.pb           Hand-trimmed: a CANCELED trip.
│   ├── sydneytrains_added.pb               Hand-trimmed: an ADDED trip with no schedule.
│   ├── sydneytrains_pastmidnight.pb        A trip whose stop times exceed 24:00:00.
│   ├── malformed_truncated.pb              First 200 bytes of a valid feed.
│   └── gtfs_mini.zip                       10 trips, 40 stop_times, 2 routes, hand-built.
│
├── deploy/
│   ├── Dockerfile                    Multi-stage, CGO_ENABLED=0, GOARCH=amd64, distroless.
│   ├── docker-compose.yml            postgres + headway. Volumes, healthchecks, restart policy.
│   ├── docker-compose.override.yml   Local only: port mapping, hot env.
│   └── vm-bootstrap.md               Exact commands run on a fresh BinaryLane VM. Keep current.
│
├── docs/
│   ├── loadtest.md                   Load test script, raw numbers, date, machine.
│   ├── storage.md                    Measured bytes/day before and after rollups.
│   └── runbook.md                    What to do when each alert fires.
│
├── web/                              Stage 4 only. Do not create before Stage 4.
│   └── (Next.js app)
│
└── .github/
    └── workflows/
        ├── ci.yml                    vet, build, unit tests, integration tests, coverage.
        └── docker.yml                Build and push the amd64 image on a tag.
```

---

## 6. Data model

### 6.1 Conventions

- All timestamps are `timestamptz`. Postgres stores them as UTC instants; the session timezone never affects stored values.
- `service_date` is a `date` in the **Australia/Sydney** service-day sense (see §9.2). It is not a UTC date and is not derived by truncating a timestamp.
- Durations are integer seconds, named `*_s`. Positive means late.
- Text ids from GTFS are stored as `text` verbatim. Never trim, upper-case or "normalise" them.
- The static tables are keyed by `version_id` so a republished bundle never mutates rows that an in-flight match is reading.

### 6.2 DDL

`migrations/0001_schedule.sql`:

```sql
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
```

`migrations/0002_observations.sql`:

```sql
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
    -- SUPERSEDED by migrations/0005: the key is now
    --   (service_date, feed_id, trip_id, stop_id, feed_ts)
    -- and stop_sequence is nullable. The TfNSW feeds never send stop_sequence
    -- (measured: 0 of 3,836 updates), so an unmatched observation can never
    -- have one, and unmatched observations must be storable. Open Question 11.
) PARTITION BY RANGE (service_date);

-- A default partition means a write for an unexpected service_date never fails.
-- A non-empty default partition is an alert (§10), not a normal condition.
CREATE TABLE observations_default PARTITION OF observations DEFAULT;

-- Created per-day by the maintenance job; this is the shape:
-- CREATE TABLE observations_2026_09_21 PARTITION OF observations
--     FOR VALUES FROM ('2026-09-21') TO ('2026-09-22');
```

Index reasoning for `observations`:

| Index | Why it exists | Why not something else |
|---|---|---|
| `PRIMARY KEY (service_date, feed_id, trip_id, stop_id, feed_ts)` — `stop_sequence` since `migrations/0005` | It *is* the idempotency key — `ON CONFLICT DO NOTHING` needs a unique constraint over exactly these columns. It also serves "everything that happened on trip T today" in one range scan. `stop_id` replaces `stop_sequence` because the feed never sends the latter. | A `bigserial` surrogate key plus a separate unique index costs the same bytes and buys nothing. Keeping `stop_sequence` in the key would make every unmatched observation unstorable. |
| `CREATE INDEX obs_by_stop ON observations (service_date, stop_id, feed_ts);` — created per partition by the maintenance job | Backs "raw observations for stop S on date D", which the rollup job and the drill-down view use. | A global index on the parent is not possible; Postgres creates a matching index on each partition automatically when you index the parent, so index the **parent** once and let Postgres propagate. |
| *No* index on `route_id` | The rollup reads a whole day's partition and aggregates; a sequential scan of one day's partition is faster than an index scan over most of it. Historical route queries hit `otp_route_hourly`, not this table. | Adding it would grow the hot write path for a query that never runs against raw data. Revisit only if a measured query needs it. |

`migrations/0003_rollups.sql`:

```sql
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
```

### 6.3 Rollup query shape

The rollup takes the **last** observation per `(service_date, feed_id, trip_id, stop_sequence)` — the final word on what happened at that stop — then aggregates those into hourly buckets. Intermediate updates are not counted, otherwise a trip that was updated fifty times would outweigh a trip updated twice.

```sql
WITH final AS (
    SELECT DISTINCT ON (service_date, feed_id, trip_id, stop_sequence)
           service_date, stop_id, route_id, direction_id,
           observed_delay_s, stop_time_rel, trip_rel, feed_ts
    FROM observations
    WHERE service_date >= $1 AND service_date < $2
      AND feed_ts >= $3 AND feed_ts < $4
    ORDER BY service_date, feed_id, trip_id, stop_sequence, feed_ts DESC
)
INSERT INTO otp_stop_hourly (...)
SELECT date_trunc('hour', feed_ts) AS bucket_start,
       service_date, stop_id, COALESCE(route_id, '~unmatched'),
       COALESCE(direction_id, -1),
       count(*),
       count(*) FILTER (WHERE observed_delay_s < $early_s),
       count(*) FILTER (WHERE observed_delay_s BETWEEN $early_s AND $late_s),
       count(*) FILTER (WHERE observed_delay_s > $late_s AND observed_delay_s <= $very_late_s),
       count(*) FILTER (WHERE observed_delay_s > $very_late_s),
       count(*) FILTER (WHERE stop_time_rel = 1),
       count(*) FILTER (WHERE trip_rel = 3),
       percentile_disc(0.5) WITHIN GROUP (ORDER BY observed_delay_s)::int,
       percentile_disc(0.9) WITHIN GROUP (ORDER BY observed_delay_s)::int,
       avg(observed_delay_s)::real
FROM final
GROUP BY 1, 2, 3, 4, 5
ON CONFLICT (bucket_start, stop_id, route_id, direction_id)
DO UPDATE SET n_obs = EXCLUDED.n_obs, /* ...all counters... */ computed_at = now();
```

`ON CONFLICT DO UPDATE` rather than `DO NOTHING`: a bucket may be recomputed after late-arriving data, and the recomputation is authoritative. [DECIDED — revisit if recomputation becomes expensive enough to need an append-only design.]

---

## 7. Interfaces

### 7.1 HTTP API

Base path `/v1`. All responses `application/json; charset=utf-8`. All timestamps RFC 3339 with offset. Example ids below are **illustrative**: real `route_id` and `stop_id` values come from the TfNSW bundle and must never be hard-coded in application code, tests excepted.

#### `GET /v1/lines`

List routes in the active schedule.

Query params: `mode` (optional, GTFS `route_type` integer), `limit` (default 200, max 1000).

```
GET /v1/lines?mode=2&limit=2
```
```json
{
  "lines": [
    { "route_id": "T1-EXAMPLE", "short_name": "T1", "long_name": "North Shore & Western Line", "route_type": 2, "feed_id": "sydneytrains" },
    { "route_id": "T2-EXAMPLE", "short_name": "T2", "long_name": "Inner West & Leppington Line", "route_type": 2, "feed_id": "sydneytrains" }
  ],
  "schedule_version_id": 41,
  "count": 2
}
```

#### `GET /v1/lines/{route_id}/now`

Current state of every active trip on a route, from the latest-state cache. Never queries `observations`.

Query params: `direction` (`0`, `1`, omitted = both), `limit` (default 100, max 1000).

`summary` counts every trip on the route before `limit` is applied. `active_trips` excludes cancelled trips, which are counted in `cancelled`; a trip with no known delay is active but in no status bucket. `median_delay_s` is the lower middle on an even count, so it is always a delay some trip actually has.

```
GET /v1/lines/T1-EXAMPLE/now?direction=0
```
```json
{
  "route_id": "T1-EXAMPLE",
  "short_name": "T1",
  "direction_id": 0,
  "as_of": "2026-09-21T11:04:12+10:00",
  "feed_age_s": 9,
  "summary": {
    "active_trips": 38,
    "early": 0,
    "on_time": 31,
    "late": 6,
    "very_late": 1,
    "cancelled": 2,
    "median_delay_s": 47
  },
  "trips": [
    {
      "trip_id": "1234.T.8.1-EXAMPLE",
      "service_date": "2026-09-21",
      "vehicle_id": "D12",
      "headsign": "Emu Plains",
      "trip_rel": 0,
      "matched": true,
      "next_stop": {
        "stop_id": "2000341",
        "name": "Strathfield Station, Platform 4",
        "stop_sequence": 11,
        "scheduled": "2026-09-21T11:07:00+10:00",
        "predicted": "2026-09-21T11:08:30+10:00",
        "delay_s": 90,
        "status": "late"
      },
      "last_update": "2026-09-21T11:04:03+10:00"
    }
  ],
  "count": 1
}
```

Errors: `404 line_not_found` if `route_id` is absent from the active schedule. `200` with an empty `trips` array if the route exists but nothing is running.

**Stage 1 (no schedule loaded):** a route is known only if it is in a fresh feed snapshot, so a real line with nothing running is also `404 line_not_found`, with the message saying "in the live feed". `short_name`, `headsign`, `vehicle_id` when the feed leaves it empty, and `next_stop.name` / `stop_sequence` / `scheduled` / `predicted` are omitted; `next_stop` is the first stop the feed still reports for the trip, and its `status` is `early`, `on_time`, `late`, `very_late` or `unknown`. The data-endpoint `503 not_ready` rule for a missing schedule applies from Stage 2.

#### `GET /v1/stops/{stop_id}/now`

Upcoming departures at a stop with live delay.

Query params: `window_min` (default 60, max 180), `limit` (default 20, max 100).

```
GET /v1/stops/2000341/now?window_min=30
```
```json
{
  "stop_id": "2000341",
  "name": "Strathfield Station, Platform 4",
  "as_of": "2026-09-21T11:04:12+10:00",
  "departures": [
    {
      "route_id": "T1-EXAMPLE",
      "short_name": "T1",
      "trip_id": "1234.T.8.1-EXAMPLE",
      "headsign": "Emu Plains",
      "scheduled": "2026-09-21T11:07:00+10:00",
      "predicted": "2026-09-21T11:08:30+10:00",
      "delay_s": 90,
      "status": "late",
      "stop_time_rel": 0
    }
  ],
  "count": 1
}
```

#### `GET /v1/stops/{stop_id}/history`

Hourly on-time performance for a stop, from `otp_stop_hourly`.

| Param | Type | Default | Notes |
|---|---|---|---|
| `from` | RFC 3339 or `YYYY-MM-DD` | now − 7 days | Inclusive. |
| `to` | RFC 3339 or `YYYY-MM-DD` | now | Exclusive. |
| `route_id` | string | all | Filter. |
| `direction` | `0` / `1` | both | Filter. |
| `bucket` | `hour` / `day` | `hour` | `day` aggregates hourly rows server-side. |

Range is capped at `HISTORY_MAX_DAYS` (default 90); exceeding it is `400 range_too_large`.

```
GET /v1/stops/2000341/history?from=2026-09-20&to=2026-09-21&route_id=T1-EXAMPLE
```
```json
{
  "stop_id": "2000341",
  "name": "Strathfield Station, Platform 4",
  "route_id": "T1-EXAMPLE",
  "bucket": "hour",
  "from": "2026-09-20T00:00:00+10:00",
  "to": "2026-09-21T00:00:00+10:00",
  "totals": { "n_obs": 812, "on_time_pct": 88.4, "delay_p50_s": 34, "delay_p90_s": 212 },
  "buckets": [
    {
      "bucket_start": "2026-09-20T08:00:00+10:00",
      "n_obs": 74,
      "n_early": 1, "n_on_time": 60, "n_late": 11, "n_very_late": 2,
      "n_skipped": 0, "n_cancelled": 1,
      "on_time_pct": 81.1,
      "delay_p50_s": 62,
      "delay_p90_s": 318,
      "delay_mean_s": 104.7
    }
  ],
  "count": 1
}
```

#### `GET /v1/lines/{route_id}/history`

Same shape as the stop history, reading `otp_route_hourly`, without `stop_id`.

#### `GET /v1/admin/stats`

Operational summary. Not a public contract; may change freely.

```json
{
  "active_schedule_versions": [{ "feed_id": "sydneytrains", "version_id": 41, "loaded_at": "2026-09-21T03:12:44+10:00", "trip_count": 2914 }],
  "feeds": [{ "feed_id": "sydneytrains", "last_success": "2026-09-21T11:04:03+10:00", "last_status": 200, "consecutive_failures": 0, "feed_age_s": 9 }],
  "ingest": { "queue_len": 12, "queue_cap": 8192, "rows_written_last_min": 1943, "filtered_last_min": 41022 },
  "match_rate_15m": 0.962,
  "oldest_partition": "2026-09-07",
  "rollup_watermark": "2026-09-21T10:00:00Z"
}
```

#### `GET /healthz`

Liveness. Returns `200 {"status":"ok"}` if the process is running. No dependency checks — a Postgres outage must not cause a restart loop.

#### `GET /readyz`

Readiness. `200` only if all of: a schedule version is active and loaded into the cache, the Postgres pool can `SELECT 1` within 2 s, and at least one feed has succeeded within `READY_MAX_FEED_AGE` (default 120 s). Otherwise `503` with the §7.2 envelope (`code: not_ready`) and a top-level `reasons` array beside `error`, one human-readable string per failed check. "A feed has succeeded" is measured by the newest feed snapshot in the latest-state cache, which exists only after a fetch and a decode both succeeded. **Stage 1:** the schedule check is absent until a schedule exists.

#### `GET /metrics`

Prometheus text exposition. Stage 4.

### 7.2 Error envelope

Every non-2xx response, without exception:

```json
{
  "error": {
    "code": "line_not_found",
    "message": "no route with id \"T9-EXAMPLE\" in the active schedule",
    "request_id": "01JBQ8Z2M4K9"
  }
}
```

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_parameter` | Unparseable or out-of-range query param. `message` names the param. |
| 400 | `range_too_large` | `to - from` exceeds `HISTORY_MAX_DAYS`. |
| 404 | `line_not_found` | Unknown `route_id`. |
| 404 | `stop_not_found` | Unknown `stop_id`. |
| 404 | `not_found` | No endpoint at this path. Without it the router would answer in plain text. |
| 429 | `too_many_requests` | Per-IP limiter tripped. `Retry-After` set. |
| 500 | `internal` | Anything unexpected. `message` is always the literal string `"internal error"`; detail goes to the log keyed by `request_id`. |
| 503 | `not_ready` | Only from `/readyz`, and from data endpoints when no schedule version is loaded. |

### 7.3 Internal interfaces

```go
// internal/gtfsrt

// RawUpdate is one StopTimeUpdate flattened with its parent TripDescriptor.
// Every pointer from the protobuf has already been dereferenced or defaulted
// here; no other package touches the generated structs.
type RawUpdate struct {
    FeedID        string
    HeaderTS      time.Time // FeedHeader.timestamp, feed-supplied
    FetchedAt     time.Time // our clock, when the body arrived
    EntityID      string
    TripID        string    // "" when the producer omitted it
    RouteID       string    // "" when the producer omitted it
    StartDate     string    // "YYYYMMDD" from TripDescriptor, "" when absent
    StartTime     string    // "HH:MM:SS" from TripDescriptor, "" when absent
    DirectionID   *uint32
    VehicleID     string
    TripRel       int32     // raw TripDescriptor.schedule_relationship
    StopSequence  *uint32
    StopID        string
    StopTimeRel   int32     // raw StopTimeUpdate.schedule_relationship
    ArrivalDelay  *int32
    ArrivalTime   *int64    // Unix seconds
    DepartureDelay *int32
    DepartureTime *int64
    Uncertainty   *int32
}

func Decode(feedID string, body []byte, fetchedAt time.Time) (DecodedFeed, error)
```

```go
// internal/servicetime

// Loc is Australia/Sydney, loaded once at init. Panics at init if the tz
// database is missing, which is the correct outcome: silently running in UTC
// corrupts every service date.
var Loc *time.Location

// ParseGTFSTime turns "25:10:00" into 90600. Hours above 23 are legal.
func ParseGTFSTime(s string) (seconds int, err error)

// ServiceDayStart returns the instant that seconds-since-service-day-start is
// measured from, which GTFS defines as noon minus twelve hours on the service
// date. On the October DST transition that is 23:00 the previous evening (a
// 23-hour day); on the April transition it is 01:00 (a 25-hour day). Computing
// it as "midnight" directly is wrong on transition days.
func ServiceDayStart(d time.Time) time.Time

// AtServiceOffset converts (service date, seconds) into an absolute instant.
func AtServiceOffset(serviceDate time.Time, seconds int) time.Time

// CandidateServiceDates returns the service dates a given instant could belong
// to, newest first. Usually one; two within overlap (SERVICE_DAY_OVERLAP_H) of
// the owning day's ServiceDayStart. Ownership goes by ServiceDayStart, not the
// local calendar date.
func CandidateServiceDates(t time.Time, overlap time.Duration) []time.Time
```

```go
// internal/match

type Schedule struct {
    VersionID int64
    FeedID    string
    // populated read-only at construction; never mutated after publish
}

func (s *Schedule) Trip(tripID string) (Trip, bool)
func (s *Schedule) StopTime(tripID string, seq uint32) (StopTime, bool)
func (s *Schedule) StopTimeByStopID(tripID, stopID string) (StopTime, bool)
func (s *Schedule) RunsOn(serviceID string, d time.Time) bool

type Matcher struct{ /* holds atomic.Pointer[Schedule] per feed */ }

func (m *Matcher) Swap(feedID string, s *Schedule)
func (m *Matcher) Match(u gtfsrt.RawUpdate) (Observation, error)
```

```go
// internal/ingest

type Observation struct {
    ServiceDate     time.Time
    FeedID          string
    TripID          string
    StopID          string
    FeedTS          time.Time
    // Nullable since migrations/0005: it can only come from the timetable,
    // and the feed never sends it. Not part of the key.
    StopSequence    *int32
    RouteID         string   // "" when unmatched
    DirectionID     *int16
    ArrivalDelayS   *int32
    DepartureDelayS *int32
    ObservedDelayS  *int32
    TripRel         int32
    StopTimeRel     int32
    Matched         bool
    VehicleID       string
}

// Key is the idempotency key and the change-filter key.
func (o Observation) Key() Key

// Filter is a concrete type, not an interface: there is one implementation
// and no second one in prospect.
type Filter struct{ /* ... */ }
func (f *Filter) Admit(Observation) bool
func (f *Filter) Forget(Key)
func (f *Filter) Stats() FilterStats

type Writer struct{ /* ... */ }
func NewWriter(pool *pgxpool.Pool, in <-chan Observation, cfg WriterConfig, log *slog.Logger) *Writer
// Run blocks until in is closed AND the final batch is flushed, or ctx is done.
func (w *Writer) Run(ctx context.Context) error
```

```go
// internal/store — every method takes a context and never retries internally.
func (s *Store) InsertObservations(ctx context.Context, obs []Observation) (inserted int64, err error)
func (s *Store) ActiveScheduleVersion(ctx context.Context, feedID string) (int64, error)
func (s *Store) LoadSchedule(ctx context.Context, versionID int64) (*match.Schedule, error)
func (s *Store) EnsurePartitions(ctx context.Context, from, to time.Time) ([]string, error)
func (s *Store) DropPartitionsBefore(ctx context.Context, d time.Time) ([]string, error)
func (s *Store) RollupHour(ctx context.Context, from, to time.Time, t Thresholds) (RollupResult, error)
```

---

## 8. Configuration & secrets

All configuration is environment variables, read once at startup into `config.Config`, validated, and never read again. A missing required variable or a failed validation exits with code 2 before any goroutine starts. The one exception is the feed catalogue, which is a JSON file because a list of objects does not fit an env var legibly. [DECIDED — revisit if per-environment feed overrides are needed; the next step would be `HEADWAY_FEEDS_FILE` plus a small overlay, not a config framework.]

| Variable | Type | Default | Required | What breaks if it is wrong |
|---|---|---|---|---|
| `TFNSW_API_KEY` | string | — | yes | Every feed request returns 401 and the service never ingests. Validated as non-empty only; a wrong key is indistinguishable from a missing one until the first poll. |
| `DATABASE_URL` | Postgres URL | — | yes | Startup fails at pool construction. Must include `sslmode=disable` for the Compose-local Postgres and `sslmode=require` if ever pointed at a remote database. |
| `HEADWAY_FEEDS_FILE` | path | `config/feeds.json` | no | Missing file exits at startup. Malformed JSON exits at startup with the offending line. |
| `HEADWAY_ENABLED_FEEDS` | comma list of feed ids | every feed with `"enabled": true` | no | When set it replaces the catalogue's `enabled` flags entirely, so a feed can be turned on for one deployment without editing a committed file. Startup logs the resolved list at INFO; an id not in the catalogue is a startup error, not a warning, because a typo would otherwise look like an upstream outage. |
| `FEED_POLL_INTERVAL` | duration | `15s` | no | Below `10s` wastes quota without gaining freshness (the feeds refresh every 15 s). Below `5s` across several feeds will trip the upstream throttle. Validated: must be ≥ `5s`. |
| `FEED_POLL_JITTER` | duration | `2s` | no | Zero makes all pollers fire in the same instant, producing a burst that can exceed the 5 requests-per-second throttle. |
| `FEED_HTTP_TIMEOUT` | duration | `20s` | no | Longer than `FEED_POLL_INTERVAL` lets slow requests pile up; the poller refuses to start a fetch while one is in flight, so a too-long timeout shows up as missed polls. |
| `FEED_RATE_LIMIT_RPS` | float | `4` | no | Above `5` trips the TfNSW throttle (HTTP 403, `X-Error-Detail: Account Over Rate Limit`). Keep headroom. |
| `FEED_DAILY_BUDGET` | int | `55000` | no | The service stops polling for the rest of the UTC day when the counter is reached and logs at ERROR. Set above the real quota and you get 403s instead. |
| `FEED_STALE_POLLS` | int | `8` | no | Consecutive successful polls with a non-advancing header timestamp before the feed is marked stale and its observations stop being admitted (§9.5). Eight polls is two minutes at the default interval. |
| `FEED_MAX_SKEW` | duration | `5m` | no | Skew beyond this is logged and measured, never corrected (§9.5). Too small and every poll logs a WARN. |
| `SCHEDULE_REFRESH_INTERVAL` | duration | `24h` | no | Too long and the timetable drifts from the feed, dropping the match rate over days. Too short wastes quota on a large zip. |
| `SCHEDULE_REFRESH_AT` | `HH:MM` local | `03:30` | no | Must be outside service peak; the load holds memory for the parsed bundle. |
| `SCHEDULE_KEEP_VERSIONS` | int | `3` | no | Older schedule versions are deleted once this many newer ones exist (§9.1). Below `2` there is no previous version left to activate when a freshly published bundle turns out to be bad; validation rejects it. |
| `SERVICE_DAY_OVERLAP_H` | int | `6` | no | How close to local midnight an update must be for yesterday to be a candidate service date when `TripDescriptor.start_date` is absent (§9.2). |
| `SERVICE_DATE_TOLERANCE` | duration | `6h` | no | How far an update may sit from a scheduled stop time and still resolve to that service date (§9.2). |
| `SERVICE_TIME_MAX_S` | int | `172800` | no | A stop time above this is rejected at load with the row number (§9.2). |
| `INGEST_QUEUE_SIZE` | int | `8192` | no | Must exceed the updates in **one poll**, because a poll submits all of them in a burst: measured at **3,939** for the trains feed alone on 2026-09-21, so the default leaves roughly 2× headroom. Below that, the surplus is dropped by design rather than queued — verified by observing exactly that in a test sized at 1,024. Re-check when Stage 3 adds the bus feed, which is far larger. Too large and a crash loses more rows and the process holds more memory. |
| `INGEST_BATCH_SIZE` | int | `500` | no | Larger batches amortise round-trips but lengthen the window of loss on a crash and lengthen shutdown. |
| `INGEST_FLUSH_INTERVAL` | duration | `2s` | no | This is the dominant term in freshness lag. Raising it to `10s` will visibly move the p95 freshness metric. |
| `FILTER_MIN_DELTA_S` | int | `0` | no | `0` writes on any change. Raising it to e.g. `30` cuts storage but quantises the delay history. |
| `FILTER_MAX_ENTRIES` | int | `500000` | no | The change filter's map is bounded; exceeding the bound evicts the oldest entries and causes redundant writes, not incorrect data. |
| `DELAY_RECONCILE_TOLERANCE_S` | int | `60` | no | When `delay` and `time` disagree by more than this against the schedule, `delay` wins (§9.5). |
| `ON_TIME_EARLY_S` | int | `-60` | no | Classification boundary. Changing it invalidates comparisons with previously computed rollups; recompute or note the date of the change. |
| `ON_TIME_LATE_S` | int | `300` | no | Same. See §16 — this is not TfNSW's official definition. |
| `ON_TIME_VERY_LATE_S` | int | `900` | no | Same. |
| `RETENTION_DAYS` | int | `14` | no | Too low and drill-down into raw data becomes impossible; too high and the disk fills. Rollups are never dropped. |
| `PARTITION_LOOKAHEAD_DAYS` | int | `3` | no | If this reaches zero, writes land in `observations_default`, which is an alert. |
| `MAINTENANCE_INTERVAL` | duration | `15m` | no | Longer intervals let the rollup fall behind, which delays retention, which grows the disk. |
| `HISTORY_MAX_DAYS` | int | `90` | no | Caps the history query range. Too high and a single request can scan the whole rollup table. |
| `CACHE_TTL` | duration | `45m` | no | Shorter than the longest trip and `/now` drops trips mid-journey. Longer and stale trips linger in the response. |
| `HTTP_ADDR` | host:port | `:8080` | no | Binding to a privileged port as non-root fails. |
| `HTTP_READ_TIMEOUT` | duration | `10s` | no | — |
| `HTTP_WRITE_TIMEOUT` | duration | `30s` | no | Must exceed the slowest history query or large responses truncate. |
| `HTTP_SHUTDOWN_GRACE` | duration | `20s` | no | Must exceed `INGEST_FLUSH_INTERVAL` plus the time to write one batch, or shutdown drops a batch. |
| `HTTP_RATE_LIMIT_RPS` | float | `50` | no | Per-IP. Set too low and the load test fails against itself. |
| `READY_MAX_FEED_AGE` | duration | `120s` | no | `/readyz` returns 503 when no feed has succeeded within this window (§7.1). Shorter than `FEED_POLL_INTERVAL` and readiness flaps. |
| `DB_MAX_CONNS` | int | `10` | no | On a 1 GB VM, Postgres plus a large pool will OOM. The writer needs one; the API needs the rest. |
| `LOG_LEVEL` | `debug`/`info`/`warn`/`error` | `info` | no | `debug` logs every decoded update and will fill the disk within a day. |
| `LOG_FORMAT` | `json`/`text` | `json` | no | `text` only for local development. |
| `METRICS_ENABLED` | bool | `true` | no | Stage 4. |
| `TZ` | IANA name | `Australia/Sydney` | no | Only affects log rendering. Service-day maths uses `servicetime.Loc` regardless, so changing `TZ` must not change stored data — there is a test for that. |

### 8.1 Feed catalogue

`config/feeds.json`. No secrets. Committed.

```json
{
  "feeds": [
    {
      "id": "sydneytrains",
      "label": "Sydney Trains",
      "realtime_url": "https://api.transport.nsw.gov.au/v2/gtfs/realtime/sydneytrains",
      "schedule_url": "https://api.transport.nsw.gov.au/v1/gtfs/schedule/sydneytrains",
      "route_type": 2,
      "enabled": true
    }
  ]
}
```

**Verify every URL before first use.** The TfNSW endpoint set has changed over time: the v1 Sydney Trains realtime and vehicle-position endpoints were announced for deprecation in May 2025 in favour of v2, and the Metro and Inner West Light Rail feeds likewise moved to v2. Bus, ferry and regional endpoints have remained on the v1 path with a per-operator suffix (for example `.../v1/gtfs/realtime/ferries/sydneyferries`). Do not add a feed to this file from memory; open the Realtime Trip Update dataset page on the TfNSW Open Data Hub, copy the operation path, and confirm with one `curl` before committing.

```
curl -sS -o /tmp/feed.pb -w '%{http_code} %{size_download}\n' \
  -H "Authorization: apikey $TFNSW_API_KEY" \
  https://api.transport.nsw.gov.au/v2/gtfs/realtime/sydneytrains
```

A 200 with a non-trivial body size means the path is right. A 403 with `X-Error-Detail: Account Over Rate Limit` means the path is right and you are polling too fast. A 404 means the path is wrong.

**Verified 2026-09-21** against the live API with a real key. The two paths are on *different API versions*, which is the trap this section exists to catch:

| Feed | Path | Result |
|---|---|---|
| Sydney Trains realtime | `https://api.transport.nsw.gov.au/v2/gtfs/realtime/sydneytrains` | `200`, `application/octet-stream`, 233,837 bytes, valid GTFS-realtime (header `gtfs_realtime_version "2.0"`, feed timestamp within two minutes of the request). |
| Sydney Trains schedule | `https://api.transport.nsw.gov.au/v1/gtfs/schedule/sydneytrains` | `200`, `application/zip`, 10,715,169 bytes, `content-disposition: attachment; filename=sydneytrains_GTFS_PROD_<YYYYMMDDHHMMSS>.zip`. |
| Sydney Trains schedule on **v2** | `https://api.transport.nsw.gov.au/v2/gtfs/schedule/sydneytrains` | `404`. The v2 move applied to realtime, not to the static bundle. |

Caching behaviour, same date, same key:

- The **realtime** response carries `cache-control: private` and **no `ETag` and no `Last-Modified`**. `HEAD` on it returns `502`. Conditional requests are therefore not possible against this feed, whatever the published description suggests.
- The **schedule** response carries `Last-Modified` (no `ETag`), so `If-Modified-Since` is worth sending on the daily 10.7 MB download.

### 8.2 Secrets

| Context | Where the key lives | Notes |
|---|---|---|
| Local development | `.env` at the repo root, loaded by Compose via `env_file`. | `.env` is in `.gitignore`. `.env.example` carries placeholders only. Never `export` the key in a shell that has history enabled without a leading space. |
| CI | GitHub Actions repository secret `TFNSW_API_KEY`. | CI never calls the live API — tests run against recorded fixtures. The secret exists only for an optional manually-triggered smoke workflow. |
| Deployed VM | `/opt/headway/.env`, mode `0600`, owned by the deploy user, referenced by `env_file` in Compose. | Not in the image, not in the Compose file, not in a build arg. |

- The key is never logged. `config.Secret` is a string type whose `String`, `GoString`, `MarshalText` and `LogValue` all render `[redacted]`, so no print verb and no `slog` attribute can leak it; `Secret.Reveal()` is the single, greppable way to get the real value. `TestConfig_Printed_DoesNotRevealSecrets` asserts that `%v`, `%+v`, `%q` and a JSON log line contain neither the API key nor the database password. `DATABASE_URL` is a `Secret` too, because it carries a password.
- The key is sent as `Authorization: apikey <KEY>`, never as a query parameter.
- Rotation: replace the value in `/opt/headway/.env` and `docker compose up -d`. No code change.

---

## 9. Hard problems

### 9.1 Matching realtime updates to a timetable that is republished underneath you

**Why it is hard.** The realtime feed refers to trips by `trip_id`, but the static bundle those ids come from is regenerated daily, and ids are not guaranteed stable across regenerations. A trip can be `ADDED` and have no schedule row at all. A trip can be `CANCELED`, in which case the stop-time updates may be absent, empty, or present with `NO_DATA`. `stop_sequence` may be missing, in which case the only anchor is `stop_id`, which is not unique within a trip if the trip visits a stop twice. And TfNSW's feeds are known to emit at least one `TripDescriptor.schedule_relationship` value that the current GTFS-realtime specification no longer defines — historically `REPLACEMENT` — so any code that switches exhaustively on a generated enum will either fail to compile against it or silently mis-bucket it. **Confirm the actual integer value against a captured fixture; do not take a value from this document or from the upstream `.proto` on trust.**

There is a second, related trap: the generated bindings in `github.com/MobilityData/gtfs-realtime-bindings` are built from the canonical `.proto`. TfNSW documents its own `.proto` files carrying TfNSW-assigned extensions (extension field numbers are mentioned in their release notes for the Sydney Trains/Metro v2 and light-rail feeds) for fields such as carriage-level occupancy. Protobuf ignores unknown fields on decode, so **the canonical bindings will decode the core trip-update fields correctly**; they simply will not surface the extensions. Headway does not use the extensions, so the canonical bindings are sufficient. If an extension field is ever needed, generate bindings from TfNSW's published `.proto` into `internal/gtfsrt/pb/` and vendor them — do not patch the upstream module.

**Chosen approach.** Version the schedule and match against a snapshot.

1. Each downloaded bundle becomes a `schedule_versions` row keyed by content hash. Identical content is a no-op.
2. Rows load under the new `version_id` while the old version stays active and readable.
3. On success the new version is activated in one transaction: `UPDATE ... SET active=false WHERE feed_id=$1 AND active; UPDATE ... SET active=true, activated_at=now() WHERE id=$2;` — the partial unique index makes a partial failure impossible.
4. `Schedule` is rebuilt in memory and published with `atomic.Pointer.Store`. In-flight matches finish against the old snapshot. The old snapshot is garbage-collected when the last reference drops.
5. Old versions are deleted from Postgres after `SCHEDULE_KEEP_VERSIONS` (default 3) newer versions exist, by `DELETE FROM schedule_versions WHERE id = $1` — the `ON DELETE CASCADE` clears the child tables.

**What the feed actually contains.** Measured on 2026-09-21 against a recorded peak-adjacent response (`testdata/sydneytrains_tripupdate_0001.pb`, 228,971 bytes, 488 entities, 3,939 updates). Everything in this block is from the fixture, not from documentation:

| Observation | Measurement | Consequence |
|---|---|---|
| **`stop_sequence` is never sent** | present on **0 of 3,836** stop-time updates | Match order 1 below can never fire on this feed. `trip_id` + `stop_id` is the *primary* path, not the fallback, and `stop_sequence` can only ever come from the timetable. See Open Question 11. |
| **`REPLACEMENT` (5) is routine** | `trip_rel`: `0`=3,201, `3`=10, **`5`=728** | 18.5 % of updates carry a value the current specification removed. The decision to store raw integers is not defensive — without it a fifth of this feed would be mis-bucketed or rejected. |
| **The `VehicleDescriptor` is present but empty** | on 488 of 488 trip updates; `vehicle_id` on **0** | `observations.vehicle_id` is always NULL for this feed, and the `vehicle_id` in §7.1's examples will be `null`. |
| **`direction_id` is never sent** | present on **0** updates | Every rollup row lands in `direction_id = -1`. The `direction` filter in §7.1 is unusable until direction comes from the timetable. |
| **Trips arrive with no stop-time updates** | 103 of 488 entities | These are the cancellation shape of §9.1 case 3, and they decode to one trip-level update each. |
| **`stop_time_rel` is uniformly `SCHEDULED`** | `0`=3,939 | No SKIPPED or NO_DATA in this sample. `n_skipped` may legitimately stay zero; do not treat that as a bug without a sample containing one. |
| **Route ids are operational codes** | `RTTA`, `ESI`, `IWL`, `APS`, `NSN`, `NTH`, `CCN`, `WST`, `SCO`, `BMT`, `CMB`, `SHL`, `CTY`, `OLY`, and a few `T3`/`T6` | The `T1-EXAMPLE` ids in §7.1 are illustrative, as that section says. Confirm against the static bundle at Stage 2 — `IWL` looks like Inner West Light Rail appearing in a feed named for Sydney Trains, and if the schedule bundle does not contain those routes the match rate will suffer. |
| **44 % of the payload is extension fields** | 101,575 of 228,971 bytes decode as unknown fields | Confirms the paragraph above: the canonical bindings decode every core field and silently skip TfNSW's extensions. Between two captures 15 s apart, the *only* thing that changed was extension data — 1,392 bytes of it — while every decoded field stayed identical. |

**Match resolution order,** first hit wins:

| Order | Key | Produces |
|---|---|---|
| 1 | `trip_id` + `stop_sequence` | Full match. Scheduled time known. **Never fires on the TfNSW trains feed, which omits `stop_sequence` entirely** — kept for feeds that send it. |
| 2 | `trip_id` + `stop_id`, where the trip visits that stop exactly once | Full match; `stop_sequence` taken from the schedule. |
| 3 | `trip_id` only (stop-level anchor missing) | Trip matched, stop not. `route_id` and `direction_id` populated, scheduled time unknown. |
| 4 | Nothing | `Matched=false`. Row still written with whatever delay the feed supplied. |

**Approaches rejected.**

- *Match on `(route_id, start_time, direction)` instead of `trip_id`.* Rejected: requires reconstructing the trip from its first departure, which fails for `ADDED` trips and for any trip whose first stop is skipped, and it needs a tolerance window that produces silent mis-matches around the clock-face.
- *Store a foreign key from `observations` to `trips`.* Rejected: unmatched observations would be unstorable, which throws away exactly the data that tells you the match rate is falling. `route_id` is denormalised into `observations` as plain text instead.
- *Query Postgres per update instead of an in-memory cache.* Rejected on measurement grounds before measuring: at four polls a minute across several feeds this is tens of thousands of point lookups a minute on a shared free-tier disk. The whole active schedule for the trains feed is a few hundred thousand stop-times, which fits comfortably in memory.
- *Reload the schedule in place (truncate and reinsert).* Rejected: there is a window during which the matcher sees a partial timetable and the match rate collapses to near zero. Versioning removes the window entirely.

**Edge cases that must be handled.** Each of these gets a test.

1. `trip_id` present in the feed but absent from the active schedule → order-4 fallback, `matched=false`, counter `headway_unmatched_total{reason="unknown_trip"}`.
2. `TripDescriptor.schedule_relationship` = `ADDED` → no schedule row exists by definition; `matched=false`, `reason="added"`. This must not count against the match-rate alert.
3. `schedule_relationship` = `CANCELED` with **no** `stop_time_update` entries → synthesise one observation per scheduled stop of that trip, with `trip_rel=3` and `observed_delay_s=NULL`. Without this, cancellations are invisible in the rollup.
4. `schedule_relationship` = `CANCELED` **with** stop-time updates present → use the updates, still marking `trip_rel=3`.
5. An unknown `schedule_relationship` integer (the deprecated-value case) → store the integer, classify as "other", never panic, never drop.
6. `StopTimeUpdate` with neither `arrival` nor `departure` → drop the update, increment `headway_updates_dropped_total{reason="no_time"}`.
7. `StopTimeEvent` with `time` but no `delay` → compute `delay = time − scheduled_time`; requires an order-1 or order-2 match. If unmatched, `observed_delay_s` is `NULL` and `matched=false`.
8. `StopTimeEvent` with `delay` but no `time` → use `delay` directly; no match needed for the delay, though one is still attempted for `route_id`.
9. `stop_sequence` missing and the trip visits `stop_id` twice (a loop service) → ambiguous; order-3 fallback with `reason="ambiguous_stop"`.
10. Gaps in `stop_sequence` (the feed sends stops 1, 2, 7, 8) → legal. Do not interpolate. Missing stops simply have no observation.
11. `stop_sequence` present but with no matching schedule row (the feed is ahead of the bundle) → order-2, then order-3.
12. Feed references a `stop_id` not in `stops` → still write the observation; `stop_id` is text, there is no foreign key. Counter `headway_unknown_stop_total`.
13. `DirectionID` absent → stored as `NULL` in `observations`, `-1` in the rollup tables.
14. The same trip appears twice in one feed message (duplicate entity) → the last one wins within a message; the primary key makes a second write a no-op anyway.
15. Schedule swap happens mid-batch → observations in the batch may have been matched against different versions. Acceptable: `version_id` is not stored on observations. Documented as a deliberate loss of provenance. [DECIDED — revisit if a "why did the match rate change" investigation ever needs it.]
16. Bundle download succeeds but the zip is truncated or a required file is missing → the load aborts, the old version stays active, an ERROR is logged, and the next scheduled refresh retries. The service keeps running on stale schedule data rather than stopping.
17. A bundle whose `calendar.txt` no longer covers today → `RunsOn` returns false for every service; the match still works by `trip_id`, only the "should this be running" derivation is affected. Log a WARN at load time if fewer than 50 % of services are active on the load date.

### 9.2 Service days, times past 24:00, and daylight saving

**Why it is hard.** GTFS stop times are offsets from the start of a *service day*, not clock times, and they may exceed 24 hours: a train departing at 01:10 on the night of a service that started the previous morning is `25:10:00` on the previous service date. Sydney observes daylight saving, so a service day is 23 hours long on one Sunday in October and 25 hours long on one Sunday in April, and on the April day the local wall clock reads 02:00–02:59 twice. Get this wrong and you silently attribute early-morning services to the wrong day, which corrupts every daily rollup and every retention decision.

**Chosen approach.**

- The reference instant for a service date is `ServiceDayStart(d) = noon on d in Australia/Sydney, minus 12 hours`. This is what GTFS specifies and it is correct on both transition days; computing "midnight local" directly is what breaks.
- Stop times are stored as integer seconds, not as `time`, because `time` cannot hold `25:10:00`.
- An absolute instant is `AtServiceOffset(d, s) = ServiceDayStart(d).Add(s * time.Second)`. Because `ServiceDayStart` returns a real instant and `Add` works in absolute time, this crosses DST transitions correctly.
- Assigning a service date to an incoming update: prefer `TripDescriptor.start_date` when the producer supplies it, since that is the producer's own answer. When absent, take the candidate dates from `CandidateServiceDates(headerTS)` — today, and yesterday if the local time is within `SERVICE_DAY_OVERLAP_H` (default 6) of midnight — and pick the first candidate whose schedule contains the `trip_id` with a stop time within `SERVICE_DATE_TOLERANCE` (default 6 h) of the update.
- The binary imports `_ "time/tzdata"`. The runtime image is distroless and carries no zoneinfo; without the import `time.LoadLocation("Australia/Sydney")` returns an error, and code that ignores that error runs in UTC and is wrong by ten or eleven hours. `servicetime` panics at `init` if the location cannot be loaded, which turns a silent data-corruption bug into an immediate crash.

**Approaches rejected.**

- *Store everything in UTC and derive the service date by truncating.* Rejected: wrong for every service running past midnight, which on the trains network is a nightly occurrence.
- *Store stop times as `interval`.* Rejected: `interval` arithmetic across DST in Postgres depends on the session timezone, which makes the correctness of a stored value depend on how it is read.
- *Set the container timezone to `Australia/Sydney` and use local time throughout.* Rejected: makes the service's behaviour depend on an environment variable, and `TZ` is exactly the sort of thing that differs between a laptop and a VM. `TZ` in Headway affects log rendering only, and there is a test that asserts flipping it does not change a computed service date.
- *Ignore DST because it is two days a year.* Rejected: those two days produce wrong data with no error, which is the worst failure mode available.

**Edge cases that must be handled.**

1. `"25:10:00"` parses to `90600`. `"24:00:00"` parses to `86400`. `"07:5:00"` (non-padded) parses; TfNSW feeds have been seen with non-padded hours in other GTFS producers, so tolerate it.
2. `"-01:00:00"` or a value above `SERVICE_TIME_MAX_S` (default 48 h) is rejected at load with the row number in the error.
3. The October transition: `AtServiceOffset(2026-10-04, 2*3600)`. The service day starts at 23:00 AEST on 10-03 (noon AEDT minus 12 h), so two hours in is 01:00 AEST, before the change; the nonexistent 02:00 is never produced and no special case is needed. There is a test asserting exactly this. (Superseded wording said 03:00 AEDT, which is what a midnight start would give — §15, 2026-09-23.)
4. The April transition: the service day is 25 hours. `AtServiceOffset(d, 25*3600)` lands on 01:00 the next calendar day, not 02:00. Test it.
5. The April transition: two distinct UTC instants render as `02:30 local`. Rollup buckets are keyed on `bucket_start timestamptz`, so both survive as separate rows; the API renders the offset (`+11:00` and `+10:00`) so a reader can tell them apart.
6. An update arriving at 00:30 local for a trip that started at 23:50 the previous service date → when `start_date` is absent, `CandidateServiceDates` returns today then yesterday, and today's copy of the trip fails `SERVICE_DATE_TOLERANCE` (it is ~23 h away), so yesterday is chosen.
7. `TripDescriptor.start_date` disagrees with the schedule (the producer says today, the trip only exists yesterday) → trust `start_date` and record `matched=false` rather than silently reassigning. Counter `headway_service_date_conflict_total`.
8. Retention and partition creation use service dates in `servicetime.Loc`, not `time.Now().UTC()`. A partition boundary computed in UTC is ten or eleven hours off and will drop a partition that is still being written to.
9. The transition Sunday needs 25 hours of partition coverage under a single `service_date`; since partitions are keyed on `service_date` (a `date`), not an instant, this is automatically correct. Note it in the test anyway so nobody "fixes" it.

### 9.3 Keeping the data inside a free-tier disk

**Why it is hard.** The naive design writes every `StopTimeUpdate` in every poll. A large feed can carry on the order of a thousand active trips at peak with tens of stop-time updates each; at four polls a minute that is on the order of 10⁵–10⁶ rows a minute, nearly all of them byte-identical to the previous poll. Even at 100 bytes a row that fills a free-tier disk in days. The numbers in this paragraph are order-of-magnitude reasoning, not measurements — measure them at Stage 3 and record the real figures in `docs/storage.md`.

**Chosen approach: four layers, in this order.**

1. **Change filter (the big win).** Keep `map[Key]lastValue` in memory where `Key` is `(service_date, feed_id, trip_id, stop_sequence)` and `lastValue` is `(observed_delay_s, stop_time_rel, trip_rel)`. Admit an observation only when the value differs, or differs by more than `FILTER_MIN_DELTA_S`. A vehicle running exactly to schedule produces one row per stop per day instead of one per stop per poll. Expect this to remove the large majority of candidate rows; the `headway_filtered_total` / `headway_admitted_total` ratio is the number to quote.
2. **Daily range partitioning.** `observations` is partitioned by `service_date`. Retention is `DROP TABLE`, which is instant and reclaims space immediately. `DELETE` on a large table does not return space to the filesystem without a `VACUUM FULL`, which needs a full second copy of the table — impossible on a tight disk.
3. **Hourly rollups.** `otp_stop_hourly` and `otp_route_hourly` are computed from completed hours and are the only thing the history endpoints read. They are small — bounded by stops × routes × hours — and are never dropped.
4. **Retention.** Drop partitions with `service_date < today − RETENTION_DAYS`, and only if `rollup_state.watermark` is past the end of that service date.

**Approaches rejected.**

- *TimescaleDB with native compression.* Rejected: an extension to install, pin and upgrade on a free-tier VM, for a compression ratio the change filter already beats by discarding the redundancy instead of compressing it. It is the honest answer to "what would you switch to", though — say so in an interview.
- *Write everything and aggregate later.* Rejected: the write rate, not the eventual size, is the binding constraint. A free-tier disk has limited IOPS and every redundant row costs a WAL write.
- *Store deltas only in a time-series store like ClickHouse or InfluxDB.* Rejected: a second database to operate, and the query pattern (join against a timetable) is relational.
- *`UNLOGGED` tables for `observations`.* Rejected: they do not survive a crash, and losing a day's raw data to an unclean shutdown defeats the point.
- *Postgres table compression via `toast`.* Not applicable: the rows are narrow, nothing reaches the TOAST threshold.

**Edge cases that must be handled.**

1. Filter map growth at end of service day — every trip id turns over daily. Evict entries whose `service_date` is more than one day old on every maintenance tick, and hard-cap at `FILTER_MAX_ENTRIES` with oldest-first eviction. Eviction causes redundant writes, never wrong data.
2. Process restart clears the filter. The first poll after a restart therefore writes a full snapshot: a spike of tens of thousands of rows. This is correct and idempotent — the primary key absorbs any overlap — but the writer must be able to absorb the burst without dropping. Budget `INGEST_QUEUE_SIZE` for it.
3. Retention must not run when the rollup is behind. Guard: `DropPartitionsBefore` reads `rollup_state.watermark` in the same transaction and returns an error rather than dropping.
4. A partition that does not exist yet → writes fall into `observations_default`. This works but destroys the retention story, since a default partition cannot be dropped without losing everything in it. `EnsurePartitions` runs at startup and on every maintenance tick, and a non-empty `observations_default` is an alert. **Observed 2026-09-21:** with no maintenance job yet built, all 4,586 rows of a live run landed in the default partition, exactly as this case describes. It is the expected state until `internal/rollup` exists (Stage 3) and is the reason `HeadwayDefaultPartitionUsed` is a paging alert.
5. `DROP TABLE` on a partition blocks behind any open transaction reading the parent. Retention runs with `SET lock_timeout = '5s'` and retries on the next tick rather than queueing behind a long analytical query.
6. Clock moving backwards (NTP step) making "today" earlier than the newest partition → `EnsurePartitions` is idempotent (`CREATE TABLE IF NOT EXISTS`) and retention uses `<`, so a backwards step delays a drop rather than causing one.
7. The rollup recomputing a bucket that has already been dropped from raw → the `ON CONFLICT DO UPDATE` would overwrite a good row with zeros. Guard: the rollup only processes hours whose service date still has a raw partition; otherwise it skips and logs at WARN.
8. Disk filling anyway → Postgres refuses writes and the writer errors. The service must keep serving reads. The writer logs at ERROR, increments `headway_write_failures_total`, and drops the batch rather than blocking the matcher forever. Dropping is the correct behaviour here; the alternative is a stalled process that also cannot serve reads.

### 9.4 Concurrency, backpressure and graceful shutdown

**Why it is hard.** Several pollers, one matcher path, one writer, and an HTTP server share a process. The API must stay responsive while the database is slow. The daily quota is a global resource shared across goroutines. And on shutdown, a batch that has been accepted from the channel but not yet written must be flushed — losing it is the single most embarrassing failure this design can have, because "what happens if the VM restarts mid-batch" is a question you will be asked.

**Chosen approach.**

- One goroutine per feed, each with its own jittered ticker. Jitter prevents a synchronised burst against the 5-requests-per-second throttle.
- A single shared token-bucket limiter at `FEED_RATE_LIMIT_RPS` (default 4, under the documented 5/s). Every request calls `limiter.Wait(ctx)`.
- A separate daily counter against `FEED_DAILY_BUDGET`, reset at UTC midnight. When exhausted, pollers sleep until reset and log at ERROR once.
- Decoding and matching happen on the poller's goroutine. They are CPU-bound and short; a worker pool here would be premature. Stage 3 introduces a bounded worker pool (`INGEST_WORKERS`, default `GOMAXPROCS`) between decode and match if profiling shows it is needed, and not before.
- One bounded channel `chan Observation` of `INGEST_QUEUE_SIZE`. This is the only backpressure mechanism.
- **The channel is never blocked on.** The producer does a `select` with a `default` branch: if the queue is full, drop the observation and increment `headway_dropped_total{reason="queue_full"}`. Blocking would back-pressure into the poller, which would miss polls, which loses data permanently; dropping loses one observation which the next poll will re-send. Dropping is strictly better here. [DECIDED — revisit if the drop counter is ever non-zero in steady state, which means the writer is too slow, not that the policy is wrong.]
- One writer goroutine. Batches flush on size or interval. A single writer means no write-write contention and no need for advisory locks.
- The API never touches the ingest path. `/now` reads the latest-state cache under an `RWMutex` held for microseconds; history reads go through the pool.

**Shutdown sequence, in exactly this order.** Any other order loses data.

```
SIGTERM / SIGINT
  1. cancel the poller context           → pollers stop scheduling; in-flight HTTP finishes or is cancelled
  2. http.Server.Shutdown(ctx)           → stop accepting, drain in-flight requests (HTTP_SHUTDOWN_GRACE)
  3. wait for all poller goroutines      → no new observations can be produced
  4. close(observationsChan)             → writer sees the close after draining
  5. wait for writer.Run to return       → final partial batch is flushed
  6. pool.Close()
  7. os.Exit(0)
```

Step 4 must happen only after step 3. Closing a channel that a producer still writes to panics; this ordering is enforced by a `sync.WaitGroup` for producers and is the subject of `TestShutdownFlushesPendingBatch`.

**Approaches rejected.**

- *`errgroup` with a shared context for everything.* Rejected: it cancels everything at once, so the writer's context dies before it can flush. The writer gets a separate context with its own timeout (`context.WithTimeout(context.Background(), HTTP_SHUTDOWN_GRACE)`), deliberately not derived from the shutdown context.
- *Unbounded channel or a slice buffer.* Rejected: an unbounded buffer converts a write stall into an OOM, which on a 1 GB fallback VM kills the process and loses everything.
- *Blocking send with a timeout.* Rejected: the timeout value is just a slower version of the drop, with a stall in between.
- *Several writer goroutines.* Rejected: no measured need, and it introduces deadlock ordering questions around the same primary key.
- *A WAL or on-disk spool for in-flight observations.* Rejected as over-engineering: the feed re-sends the same state fifteen seconds later, so at-least-once from the producer side is free.

**Edge cases that must be handled.**

1. `SIGKILL` or power loss → the in-flight batch is lost. Acceptable and documented: the next poll re-sends current state, and only the intermediate delay values in that fifteen-second window are lost. State this plainly rather than claiming exactly-once.
2. Postgres unreachable when the writer flushes → retry per §10, then drop the batch and continue. The process must not exit; an exiting process cannot serve reads and will restart-loop.
3. A poller's HTTP request hangs past `FEED_HTTP_TIMEOUT` → the context deadline fires, the request is abandoned, the next tick proceeds. A feed never has two requests in flight: the poll loop is sequential, so this is structural rather than a flag to maintain.
3a. A request in flight when **shutdown** cancels the context → surfaces as `context.Canceled` *or* `context.DeadlineExceeded`, depending on which deadline fires first, and neither is a failure of the feed. The poller asks its own context before classifying, because counting it would inflate the backoff and log a timeout that never happened. Regression test: `TestPoller_ShutdownDuringFetch_IsNotCountedAsAFailure`.
4. `limiter.Wait` blocks past the next tick → the tick is skipped, not queued. Use a ticker read with a `default` drop, not a backlog.
5. Shutdown while a schedule load is in progress → the load's context is cancelled, the partially loaded version is never activated, and the orphan `schedule_versions` row with `active=false` is cleaned up by the next successful load (delete versions older than the newest three, active or not).
6. The queue is full *during shutdown* → producers have already stopped at step 3, so this cannot happen. Assert it with a test.
7. A panic in a poller goroutine → a `recover` wrapper logs at ERROR with the stack and restarts that poller after a backoff. One bad feed must not take down ingestion for the others.
8. `GOMAXPROCS` on the deployment VM → the BinaryLane VM has 1 vCPU and the runtime already sees it; do not pin it. If the container is CPU-limited in Compose, set `GOMAXPROCS` to match, since the Go runtime does not read cgroup CPU limits.

### 9.5 Feed timestamps, clock skew and staleness

**Why it is hard.** There are at least four clocks: the vehicle's, the producer's (`FeedHeader.timestamp`), the TfNSW gateway's, and ours. They disagree. A `StopTimeEvent.time` is absolute Unix seconds from the producer; a `delay` is relative to a schedule we hold a possibly-different version of. And a feed that has stopped updating still returns HTTP 200 with a stale body — the request succeeds while the data is dead. "How do you handle clock skew between feeds" is a listed interview question; the answer needs to be concrete.

**Chosen approach.**

- `feed_ts` in `observations` is **always** `FeedHeader.timestamp`, never our clock. It is what makes replays idempotent.
- `ingested_at` is our clock. Freshness lag is `ingested_at − feed_ts`, measured at write time into `headway_freshness_lag_seconds`.
- Staleness is detected per feed: if `FeedHeader.timestamp` has not advanced across `FEED_STALE_POLLS` (default 8, i.e. two minutes at the default interval) consecutive successful responses, mark the feed stale, stop admitting its observations, and set `headway_feed_stale{feed_id}` to 1. A stale feed is an alert, not a crash.
- Skew is detected, not corrected: if `|FeedHeader.timestamp − now| > FEED_MAX_SKEW` (default 5 m), log at WARN with both values, record `headway_feed_skew_seconds{feed_id}`, and **still ingest**. Correcting a clock you do not control produces data that cannot be reconciled with the source.
- When both `delay` and `time` are present and disagree by more than `DELAY_RECONCILE_TOLERANCE_S` (default 60) against the schedule, prefer `delay`, because `delay` is what the producer computed against its own timetable and is therefore internally consistent. Record `headway_delay_disagreement_total`.

**Approaches rejected.**

- *Use our fetch time as the observation timestamp.* Rejected: it breaks idempotency (a replay of the same bytes produces different rows) and it inflates every freshness measurement by the poll interval.
- *Reject updates with a stale header.* Rejected: a briefly-stale feed still carries the last known truth, and dropping it creates gaps that look like outages in the rollup.
- *Normalise all timestamps to a monotonic internal sequence.* Rejected: you lose the ability to say "the feed said this at this time", which is the whole product.

**Edge cases that must be handled.**

1. `FeedHeader.timestamp` is zero or absent → fall back to `FetchedAt` and set a flag; increment `headway_missing_header_ts_total`. Idempotency degrades to "per fetch" for that feed.
2. `FeedHeader.timestamp` in the future by more than `FEED_MAX_SKEW` → ingest, warn, and never use it to compute a negative freshness lag; clamp the metric at zero.
3. The header timestamp goes *backwards* between polls (a producer failover) → treat as stale-but-valid; do not reject. The primary key means an older `feed_ts` for a key we already have simply inserts an additional row, which the rollup's `DISTINCT ON ... ORDER BY feed_ts DESC` ignores.
4. HTTP 200 with a zero-length body → not an error at the transport layer but is one here. Reject, count as a failed poll, do not reset the failure counter.
5. HTTP 200 with a body that is not valid protobuf (a gateway error page with a 200) → `proto.Unmarshal` fails; count and log the first 64 bytes as hex, not as a string.
6. HTTP 403 with `X-Error-Detail: Account Over Rate Limit` → back off and reduce the effective rate for that feed, distinct from a 403 for quota.
7. HTTP 403 with `X-Error-Detail: Account Over Quota Limit` → stop polling every feed until UTC midnight. The quota is per account, not per feed.
8. HTTP 401 → the key is wrong or expired. Log at ERROR once per minute, keep retrying at a reduced rate; do not exit, because the key may be rotated underneath a running process.
9. HTTP 304 → treat as a successful poll with no new data. Do not count it as stale. In practice this can only arise on the **schedule** download, via `If-Modified-Since`: the realtime feed returns no `ETag` and no `Last-Modified` at all (verified 2026-09-21, §8.1), so no conditional request can be formed for it. Handle 304 anyway — it costs three lines and the upstream may start sending validators.
10. A response with a valid header but zero entities → legal overnight. Do not mark stale on entity count; only on a non-advancing header timestamp.

---

## 10. Error handling & observability

### 10.1 Policy

| Concern | Policy |
|---|---|
| Feed HTTP retries | No in-request retry. The next tick is the retry. A retry inside the tick doubles quota consumption for no freshness gain. |
| Feed consecutive failures | Exponential backoff on the *interval*: `interval * 2^min(failures,4)` with full jitter, capped at 5 minutes. Reset on success. |
| Database writes | Retry up to 3 times with `200ms, 600ms, 1.8s` plus jitter, only for retryable errors: connection failures, `40001` serialization failure, `40P01` deadlock, `53300` too many connections. Never retry a constraint violation. |
| Database reads (API) | No retry. Return 500 fast; the client can retry. |
| Timeouts | Feed HTTP `FEED_HTTP_TIMEOUT` (20 s). Batch write 10 s. API handler 15 s via `http.TimeoutHandler`. Schedule download 5 m. Every database call takes a context with a deadline; there are no unbounded calls. |
| Partial batch failure | The batch is one statement (`INSERT ... ON CONFLICT DO NOTHING` via a `pgx.Batch`, or `CopyFrom` into a temporary table then an insert-select). It succeeds or fails whole. No per-row error handling. |
| Panic | `recover` at three places only: each poller goroutine, the writer goroutine, and the HTTP middleware. Log the stack at ERROR, increment `headway_panics_total`, continue. Nowhere else — a panic in `servicetime` should crash the process during development. |
| Graceful shutdown | See §9.4. |
| Fail-fast | Configuration errors, an unloadable timezone, and an unreachable database *at startup* exit with code 2. The same failures at runtime do not exit. |

### 10.2 Logging

`log/slog`, JSON handler, one line per event, to stdout. Docker captures it; no log files, no rotation to manage.

Standard fields on every line: `time` (RFC 3339 with offset), `level`, `msg` (lowercase, no trailing punctuation, no interpolated values), `component` (the package name).

| Level | Used for | Examples |
|---|---|---|
| `error` | Something is broken and a human should look. | Write failed after retries; schedule load failed; quota exhausted; panic recovered. |
| `warn` | Degraded but handled. | Feed stale; clock skew beyond tolerance; match rate below threshold; rollup skipped a bucket. |
| `info` | State changes only, not per-event. | Startup with resolved config (key redacted); schedule version activated; partition created or dropped; shutdown steps. |
| `debug` | Per-update detail. Off in production. | Each decoded entity; each filter decision. |

`info` must stay at a rate a human can read — roughly a line a minute in steady state. A per-poll `info` line is too much; per-poll goes to metrics.

Representative lines:

```json
{"time":"2026-09-21T11:04:03.221+10:00","level":"INFO","msg":"schedule version activated","component":"gtfsstatic","feed_id":"sydneytrains","version_id":41,"trip_count":2914,"stop_time_count":412885,"load_ms":18422}
{"time":"2026-09-21T11:04:18.005+10:00","level":"WARN","msg":"feed stale","component":"feed","feed_id":"buses","header_ts":"2026-09-21T11:01:02+10:00","consecutive_polls":8}
{"time":"2026-09-21T11:05:02.118+10:00","level":"ERROR","msg":"batch write failed","component":"ingest","attempts":3,"rows":500,"err":"write observations: dial tcp 172.18.0.2:5432: connect: connection refused"}
{"time":"2026-09-21T11:05:44.900+10:00","level":"INFO","msg":"http request","component":"api","request_id":"01JBQ8Z2M4K9","method":"GET","path":"/v1/lines/T1-EXAMPLE/now","status":200,"duration_ms":7,"bytes":4102,"remote":"203.0.113.9"}
```

Access logs are `info`. A 5xx also emits one `error` line carrying the same `request_id`, so the JSON message returned to the client can be looked up.

### 10.3 Metrics

Prometheus, exposed on `/metrics`. Stage 4, but the names are fixed now so dashboards and this document do not drift.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `headway_feed_requests_total` | counter | `feed_id`, `outcome` (`ok`,`not_modified`,`rate_limited`,`quota`,`unauthorized`,`timeout`,`error`) | Every upstream request. |
| `headway_feed_request_duration_seconds` | histogram | `feed_id` | Upstream latency. |
| `headway_feed_age_seconds` | gauge | `feed_id` | `now − FeedHeader.timestamp` at last successful poll. |
| `headway_feed_stale` | gauge | `feed_id` | 1 when stale. |
| `headway_feed_skew_seconds` | gauge | `feed_id` | Signed difference between header timestamp and local clock. |
| `headway_updates_decoded_total` | counter | `feed_id` | Stop-time updates decoded. |
| `headway_updates_dropped_total` | counter | `feed_id`, `reason` | Dropped in decode or match. |
| `headway_matched_total` | counter | `feed_id`, `order` (`1`..`3`) | Matches by resolution order. |
| `headway_unmatched_total` | counter | `feed_id`, `reason` | Order-4 outcomes. |
| `headway_match_rate` | gauge | `feed_id` | 15-minute sliding ratio, excluding `reason="added"`. |
| `headway_filtered_total` | counter | `feed_id` | Suppressed by the change filter. |
| `headway_admitted_total` | counter | `feed_id` | Passed the change filter. |
| `headway_queue_length` | gauge | — | Current channel occupancy. |
| `headway_dropped_total` | counter | `reason` | Dropped because the queue was full. |
| `headway_rows_written_total` | counter | — | Rows actually inserted (not attempted). |
| `headway_write_batch_duration_seconds` | histogram | — | Batch write latency. |
| `headway_write_failures_total` | counter | `retryable` | Failed batches after retries. |
| `headway_freshness_lag_seconds` | histogram | `feed_id` | `ingested_at − feed_ts`. The headline number. |
| `headway_schedule_version` | gauge | `feed_id` | Active `version_id`. |
| `headway_schedule_load_duration_seconds` | histogram | `feed_id` | — |
| `headway_rollup_duration_seconds` | histogram | — | — |
| `headway_rollup_lag_seconds` | gauge | — | `now − rollup_state.watermark`. |
| `headway_partitions` | gauge | — | Count of daily partitions. |
| `headway_default_partition_rows` | gauge | — | Should always be 0. |
| `headway_http_requests_total` | counter | `route`, `status` | `route` is the ServeMux pattern, never the raw path — a raw path would explode cardinality. |
| `headway_http_request_duration_seconds` | histogram | `route` | — |
| `headway_panics_total` | counter | `component` | — |

### 10.4 Alerts

One alert is required by the Definition of Done; these are the candidates, in priority order. Each needs a `docs/runbook.md` entry saying what to do.

| Alert | Condition | Severity | First response |
|---|---|---|---|
| `HeadwayIngestStopped` | `rate(headway_rows_written_total[10m]) == 0` for 15 m during service hours | page | Check `/v1/admin/stats`; check feed outcomes; check the disk. |
| `HeadwayFeedStale` | `headway_feed_stale == 1` for 10 m | warn | Usually upstream. Check the TfNSW API status page before touching anything. |
| `HeadwayQuotaExhausted` | `increase(headway_feed_requests_total{outcome="quota"}[1h]) > 0` | page | Reduce `FEED_POLL_INTERVAL` or disable a feed; the quota resets daily. |
| `HeadwayMatchRateLow` | `headway_match_rate < 0.8` for 30 m | warn | The schedule is probably stale or the bundle changed shape. Check `schedule_versions.loaded_at`. |
| `HeadwayFreshnessDegraded` | `histogram_quantile(0.95, headway_freshness_lag_seconds) > 60` for 15 m | warn | Writer is behind. Check `headway_queue_length` and disk IOPS. |
| `HeadwayDefaultPartitionUsed` | `headway_default_partition_rows > 0` | page | `EnsurePartitions` is not running. Fix before retention runs. |
| `HeadwayDiskLow` | node disk free < 15 % | page | Reduce `RETENTION_DAYS` and run `cmd/maintain`. |

---

## 11. Testing strategy

### 11.1 What is tested how

| Package | Level | Approach |
|---|---|---|
| `servicetime` | unit, exhaustive | Table-driven. Both DST transition dates are hard-coded fixtures. This package has the highest test-to-code ratio in the repo and that is correct. |
| `gtfsrt` | unit | Decode recorded `.pb` fixtures from `testdata/`. Assert counts, assert that a truncated file returns an error and does not panic. |
| `gtfsstatic` | unit | Parse `testdata/gtfs_mini.zip`, a hand-built 10-trip bundle. Includes a trip with `25:10:00` stop times, a `calendar_dates` exception, a BOM on the first file, and an unknown extra column. |
| `match` | unit | Construct a `Schedule` in code, feed `RawUpdate` values, assert the resolution order and the delay. Every numbered edge case in §9.1 is a named subtest. |
| `ingest` (filter) | unit | Pure function over a sequence of observations. |
| `ingest` (writer) | integration | Real Postgres. Assert idempotency by writing the same batch twice and checking the row count. |
| `store` | integration | Real Postgres. Migrations, partition creation, retention guard, rollup arithmetic. |
| `rollup` | integration | Insert known observations, run the job, assert the bucket values by hand-computed expectation. |
| `api` | unit | `httptest` against a fake store and a pre-populated cache. Assert the JSON shape and every error code in §7.2. |
| `config` | unit | Assert every required variable produces a clear error when absent, and every validated range rejects its boundary. |
| shutdown | integration | `TestShutdownFlushesPendingBatch`: fill the queue, signal shutdown, assert every enqueued row landed. |

### 11.2 Faking external dependencies

- **TfNSW realtime:** never called from tests. `cmd/fixturedump` fetches a live response once and writes it to `testdata/` with a timestamped name. Fixtures are committed. They are binary protobuf, a few hundred kilobytes each; keep the count small and prune old ones.
- **Derived fixtures:** the cancelled/added/past-midnight cases are produced by loading a real fixture, editing the decoded message in a small Go program, and re-serialising. Keep that program as `cmd/fixturedump` subcommands so the fixtures are reproducible.
- **TfNSW static:** `testdata/gtfs_mini.zip` is hand-built and checked in. Never download a real bundle in a test.
- **HTTP:** `httptest.Server` serving fixture bytes, with cases for 200, 304, 401, 403-rate, 403-quota, 500, a truncated body, and a hang past the timeout.
- **Postgres:** a real Postgres, not a mock. Locally via Compose; in CI via a GitHub Actions service container. Integration tests are behind `//go:build integration` and skip when `DATABASE_URL_TEST` is unset. Each test runs in its own schema (`CREATE SCHEMA test_<random>; SET search_path`) and drops it in `t.Cleanup`, so tests can run in parallel.
- **Clock:** every component that needs time takes a `func() time.Time`. No `time.Now()` outside `main` and `obs`. This is what makes the DST tests possible.

### 11.3 CI

`.github/workflows/ci.yml`, on every push and pull request:

1. `actions/setup-go` with the version from `go.mod`.
2. `go build ./...`
3. `go vet ./...`
4. `gofmt -l .` — fails if non-empty.
5. `staticcheck ./...` — added at Stage 2, not before.
6. `go test -race -count=1 ./...` (unit only).
7. Start a `postgres:18-alpine` service container; run `go test -race -tags=integration -count=1 ./...` with `DATABASE_URL_TEST` pointed at it.
8. `go test -coverprofile=coverage.out ./...` and print the total. No coverage gate in v1 — a gate on a project this size encourages testing the wrong things. [DECIDED — revisit at Stage 4.]
9. Cross-compile check: `GOOS=linux GOARCH=amd64 go build ./cmd/headway`. This catches the single most likely deployment failure.

`.github/workflows/docker.yml`, on a tag: build `linux/amd64` and push to GitHub Container Registry.

---

## 12. Milestones

Each stage ends in something that runs and can be demonstrated. **Stage 2 is the acceptable stopping point** — at the end of Stage 2 the project is presentable as a finished piece of work, and stages 3 and 4 are enhancements.

### Stage 1 — MVP: one feed, stored and served (target: weeks 1–2)

- [x] `go mod init`, repository skeleton per §5, `Makefile` with `run`, `test`, `migrate`.
- [x] Register for a TfNSW Open Data account and obtain an API key.
- [x] Confirm the Sydney Trains realtime endpoint path with `curl` per §8.1 and record the working URL in `config/feeds.json`. Both paths verified 2026-09-21; the schedule bundle is on v1, not v2.
- [x] `internal/config`: load and validate every variable in §8; `.env.example` complete.
- [x] `internal/store`: pool, embedded migration runner, all four migrations applied on start. Verified against Postgres 18.6: eight integration tests, `make migrate` idempotent.
- [x] `internal/feed`: one poller, shared limiter, conditional requests where the upstream supports them (schedule only — §9.5 case 9), all the HTTP status cases in §9.5. Verified against the live feed: four polls in 50 s at 15 s + jitter, ordered shutdown, 4 requests spent.
- [x] `internal/gtfsrt`: decode to `RawUpdate`. All nil handling here. 93.2 % covered; the feed-shape findings are recorded in §9.1.
- [x] `cmd/fixturedump`: capture two consecutive live responses into `testdata/`, plus `derive` for the cancelled, added, past-midnight and truncated fixtures.
- [x] `internal/ingest`: bounded channel, change filter, batch writer with `ON CONFLICT DO NOTHING`. 95.8 % covered. Wired into `main` 2026-09-23 through `match.Unmatched`: a 50 s live run wrote 3,887 rows from 4 polls, 0 failed, 0 dropped, final batch flushed on SIGINT.
- [x] `internal/cache`: latest-state cache. Per-feed snapshots swapped under an `RWMutex`, 100 % covered, race test verified to fail without the lock.
- [x] `internal/api`: `/v1/lines/{id}/now` (unmatched-only fields), `/healthz`, `/readyz`. 98.5 % covered; verified live against a real route (18 active trips, feed age 7 s).
- [x] Ordered graceful shutdown per §9.4. All six steps in `main`; a live SIGINT drained HTTP, flushed the writer and exited 0 with 0 rows lost.
- [ ] `deploy/Dockerfile` (amd64, distroless) and `deploy/docker-compose.yml`.
- [ ] BinaryLane VM provisioned; `deploy/vm-bootstrap.md` written while doing it, not after.
- [ ] Deployed and reachable. Screenshot of a live `/v1/lines/{id}/now` response in the README.
- **Demo:** "this URL shows what the T1 is doing right now, and it has been running since Tuesday."

### Stage 2 — Schedule matching, history, CI (stopping point)

- [x] `internal/servicetime` with full DST test coverage. Do this before the matcher. Done in Stage 1, since no observation can be stored without a service date: 40 tests and subtests, 96.7 % covered, both transitions as fixtures.
- [ ] `internal/gtfsstatic`: download, hash, unzip, parse, insert, activate; version retention.
- [ ] `migrations/0001` static tables populated; `schedule_versions` partial unique index verified by a test that tries to double-activate.
- [ ] `internal/match`: schedule cache with atomic swap; resolution orders 1–4; delay derivation per §9.4's reconcile rule.
- [ ] Every §9.1 edge case has a named subtest and passes.
- [ ] `migrations/0003`: rollup tables; `internal/rollup` hourly job with the retention guard.
- [ ] `/v1/stops/{id}/history` and `/v1/lines/{id}/history`.
- [ ] `/v1/lines`, `/v1/stops/{id}/now`, `/v1/admin/stats`.
- [ ] `.github/workflows/ci.yml` complete including the Postgres service container and the amd64 cross-compile.
- [ ] Match rate measured and recorded. If below 90 %, fix before moving on.
- [ ] README: architecture diagram, the numbers from §13, how to run it.
- **Demo:** "here is how often the T1 was late at Strathfield last week, by hour."

### Stage 3 — All modes, scale, retention, load test

- [ ] Add bus, metro, light rail and ferry feeds to `config/feeds.json`, each verified with `curl` first.
- [ ] Quota budget table in `docs/` showing requests/day against `FEED_DAILY_BUDGET` for the enabled set.
- [ ] Per-feed schedule versions (the loader already supports it; confirm multi-feed activation is independent).
- [ ] Bounded worker pool between decode and match, sized by `INGEST_WORKERS`, **only if** profiling shows the poller goroutine is the bottleneck. If it is not, write down that it is not and skip it.
- [ ] Daily partition creation and retention running on the maintenance tick; `observations_default` verified empty.
- [ ] `cmd/maintain` as a standalone entry point for manual runs.
- [ ] Storage measured before and after rollups; `docs/storage.md` written with real numbers.
- [ ] Load test with k6 or hey at 50 RPS against `/now` and `/history`; `docs/loadtest.md` with p50/p95/p99.
- [ ] Profile under load (`pprof` behind `HTTP_ADDR` on a separate, non-public port) and record one thing that was fixed as a result.
- **Demo:** "it ingests five feeds, it drops old partitions on its own, and here is the latency curve at 50 RPS."

### Stage 4 — Observability and front end

- [ ] `internal/obs/metrics.go` with every metric in §10.3.
- [ ] `/metrics` endpoint; Grafana Cloud free tier scraping it.
- [ ] One dashboard: ingest rate, freshness p95, match rate, queue length, partition count, API p95.
- [ ] One alert wired end to end — `HeadwayIngestStopped` — with a `docs/runbook.md` entry.
- [ ] `web/`: Next.js, one page, a line selector and a stop history chart hitting the live API.
- [ ] Optional: custom domain and TLS via Caddy in Compose.
- **Demo:** "here is the dashboard, and here is what happens when I stop the container."

---

## 13. Metrics

These are the numbers that go in the README and on the resume. Anything marked "baseline TBD" gets filled in from a real measurement, with the date and the machine, and is never estimated.

| What | How to measure | Target |
|---|---|---|
| Updates ingested per minute | `rate(headway_updates_decoded_total[5m]) * 60`, peak hour | **First measurement 2026-09-21, 13:00 Sunday (off-peak), trains feed only: ~3,940 updates per poll, ~15,700 per minute at four polls.** Peak-hour figure still TBD. |
| Rows written per minute | `rate(headway_rows_written_total[5m]) * 60` | **First measurement 2026-09-21 (off-peak): ~670 rows/min in steady state**, after the cold-filter burst of 3,913 rows on the first poll. |
| Change-filter suppression ratio | `headway_filtered_total / (headway_filtered_total + headway_admitted_total)` | ≥ 0.90. **First measurement 2026-09-21: 3,939 of 3,939 updates (100 %) were byte-identical across two polls 15 s apart** — delays, relationships and absolute predicted times all unchanged, with no keys added or removed. One off-peak Sunday sample, so treat it as an upper bound rather than the steady-state figure, and re-measure at peak in Stage 3. **A live five-poll run the same day measured 95.7 % in steady state** (673 admitted of 15,819 submitted, excluding the cold-filter first poll), against the ≥ 0.90 target. It does establish that the producer republishes on a 15 s header cadence while changing core content far less often. |
| Freshness lag p50 | `histogram_quantile(0.5, headway_freshness_lag_seconds)` | ≤ 10 s |
| Freshness lag p95 | `histogram_quantile(0.95, ...)` | ≤ 30 s |
| API p95 latency, `/now` | k6/hey at 50 RPS for 5 min | ≤ 100 ms |
| API p95 latency, `/history` (30 d, hourly) | same run | ≤ 300 ms |
| API p99 latency, `/now` | same run | ≤ 250 ms |
| Storage per day, raw | `pg_total_relation_size('observations_YYYY_MM_DD')` | **First measurement 2026-09-21: 276.9 bytes/row** (632 kB heap + 568 kB indexes over 4,586 rows, in `observations_default`). Projected from the off-peak write rate: ~270 MB/day, ~1.9 GB at the deployed 7 days (§16 q6), ~3.7 GB at the 14-day default, against the ≤ 10 GB target. Treat as a floor: one feed, off-peak, and a small table whose index overhead does not yet amortise. Re-measure per §12 Stage 3. |
| Storage per day, rollups only | Size delta of `otp_*_hourly` per day | baseline TBD |
| Storage reduction from rollups | `1 − (rollup bytes / raw bytes)` for the same day | baseline TBD; report honestly |
| Total database size at steady state | `pg_database_size('headway')` after `RETENTION_DAYS` have elapsed | ≤ 10 GB |
| Match rate | `headway_match_rate`, trains feed, excluding `ADDED` | ≥ 0.90 |
| Upstream requests per day | `increase(headway_feed_requests_total[24h])` | ≤ `FEED_DAILY_BUDGET` |
| Test count and coverage | `go test ./... -coverprofile` total | baseline TBD; report the number, not a grade |
| Uptime | `time() - process_start_time_seconds`, plus a note of the longest unbroken run | ≥ 7 days for the Definition of Done |
| Dropped observations | `headway_dropped_total` | 0 in steady state |

For the resume bullets, fill the placeholders from this table and **do not round upward**. "Processing 4,200 updates per minute" is a better line than "processing 10,000+" if 4,200 is what it does, because the follow-up question is always "how did you measure that".

---

## 14. Conventions for Claude

Rules for writing code in this repository.

### Naming

- Packages: one word, lowercase, no underscores, no `utils`, no `common`, no `helpers`. If a package needs one of those names it needs a different boundary.
- Exported identifiers get doc comments starting with the identifier name. Unexported ones get a comment only when the *why* is not obvious.
- Acronyms keep their case: `HTTPClient`, `tripID`, `feedID`, `routeID`, `stopID`. Never `Http`, never `Id`.
- Test names: `TestThing_Condition_Expectation`, e.g. `TestMatch_TripIDMissing_FallsBackToStopID`. Subtests use a plain sentence.
- SQL: lowercase `snake_case` identifiers, uppercase keywords. Tables plural, columns singular. Duration columns end in `_s`.
- Env vars: `SCREAMING_SNAKE_CASE`, prefixed `HEADWAY_` only where the unprefixed name would be ambiguous (`DATABASE_URL` and `TZ` stay unprefixed by convention).
- Files: one concept per file, named after it. `matcher.go` holds `Matcher`. No `misc.go`.

### Comments

- Comment density is low by default and high in three places: `internal/servicetime` (every non-obvious time calculation gets a sentence saying why), every SQL statement over five lines (a leading comment saying what the query answers), and every deliberate deviation from the obvious approach (a comment naming what was rejected).
- Never comment *what* a line does. Comment *why* it is that way.
- A comment that restates the function signature is deleted.
- `// TODO:` is banned. Either do it, or add it to §16, or write the decision in §15. An untracked TODO is how a project rots.

### Errors

- Wrap with context at every boundary crossing, using `%w`:
  ```go
  if err := s.insert(ctx, rows); err != nil {
      return fmt.Errorf("insert observations (%d rows, service_date=%s): %w", len(rows), d, err)
  }
  ```
- Error strings are lowercase, no trailing punctuation, and describe the operation that failed, not the error that occurred.
- Include the identifying values (feed id, trip id, row count, service date) in the wrap. An error without them costs a debugging session.
- Sentinel errors are package-level `var ErrX = errors.New("...")` and are compared with `errors.Is`. Typed errors for anything carrying data, compared with `errors.As`.
- Never `panic` outside `init` and `main`. Never `log.Fatal` outside `main`.
- Never discard an error with `_`. If it is truly ignorable, say why in a comment on the same line.

### Commits

Conventional Commits, scope = package name:

```
feat(match): resolve stop by stop_id when stop_sequence is absent
fix(servicetime): use noon-minus-12h for service day start across DST
docs(project): record the decision to drop on queue-full
test(gtfsrt): add truncated-payload fixture
chore(deps): bump pgx to v5.9.2
refactor(ingest): extract change filter from the writer
```

Body, when the change is non-obvious: what was tried and rejected. Reference the PROJECT.md section (`See §9.3`). One logical change per commit; a commit that touches five packages needs a reason in the body.

### When to ask before acting

Ask first; do not proceed on assumption:

- Any change to `migrations/` other than adding a new numbered file. Never edit an applied migration.
- Adding a direct dependency.
- Changing anything in §7 that a client could depend on (paths, field names, error codes).
- Changing a default in §8 that affects stored data (`ON_TIME_*`, `RETENTION_DAYS`, `FILTER_MIN_DELTA_S`).
- Any refactor touching more than three files or moving a package boundary.
- Introducing a new goroutine, channel, or lock.
- Anything in the Non-goals list (§2).

Do not ask; just do it:

- Adding a test.
- Fixing a failing test that is testing the right thing.
- Renaming an unexported identifier within one file.
- Adding a field to an internal struct that is not persisted or serialised.
- Improving an error message.
- Formatting and lint fixes.

### Keeping this file true

- **When a decision in this file turns out to be wrong, change the file.** Do not write code that quietly contradicts it. A divergence between the code and PROJECT.md is a bug in PROJECT.md.
- Every decision made while coding — including small ones like a buffer size or a retry count — gets a row in §15 with the date, the reason, and what was rejected. The Decision Log is append-only; superseding a decision means a new row that names the old one, not an edit.
- Anything blocked on the human gets a row in §16 with the specific question and the default that will be used until answered.
- New domain terms get a line in §17 the moment they are used anywhere above.
- Update §13 with a real number as soon as one exists, and delete the "baseline TBD".
- At the start of a session, read this file and say in one line what state the project is in and what the next unchecked milestone task is. `CLAUDE.md` carries this rule and the rest of the every-session checklist; when a rule in §14 changes, change it there too.

---

## 15. Decision Log

Append-only. To reverse a decision, add a row that names the one it supersedes.

| Date | Decision | Reason | Alternatives rejected |
|---|---|---|---|
| 2026-09-21 | Go for the service. | Concurrency model fits N pollers; one static binary; it is the learning goal. | Node/TypeScript (fine for I/O-bound work, no static binary, weaker story for the target roles); Rust (slower to write, not the goal); Python (too slow for the decode volume). |
| 2026-09-21 | PostgreSQL 18 as the only datastore. | Native range partitioning and `ON CONFLICT` cover the whole design with no extension. | TimescaleDB (extension to operate; rollups beat compression here); ClickHouse (right at 100× volume); SQLite (single-writer contention); Supabase free tier (500 MB cap and project pausing are wrong for continuous ingestion). |
| 2026-09-21 | pgx v5 with `pgxpool`, minimum v5.9.2. | COPY protocol for batch writes; 5.9.2 fixed a SQL-injection issue in the simple protocol. | `database/sql` + `lib/pq` (no COPY, lib/pq is in maintenance mode). |
| 2026-09-21 | Standard-library `ServeMux` for routing. | Go 1.22+ supports method and wildcard patterns, which is the whole requirement. | chi, gin, echo (a dependency to justify for zero benefit at this route count). |
| 2026-09-21 | No ORM; hand-written SQL in `internal/store`. | The queries are the interesting part; an ORM hides partition pruning and `ON CONFLICT` behaviour. | GORM, sqlc (sqlc is defensible; rejected to keep the build toolchain to `go` alone). |
| 2026-09-21 | Hand-rolled embedded migration runner over `embed.FS`. | No external binary in the image or CI; the runner is ~80 lines. | golang-migrate, goose (a dependency and a CLI to install for a project with four migrations). |
| 2026-09-21 | One process for pollers, writer and API. | The API reads memory and rollups; it does not contend with the writer. Two processes would need a shared cache. | Separate ingest and API binaries (premature; revisit only if the API's CPU use interferes). |
| 2026-09-21 | Version the static schedule; match against an atomically swapped in-memory snapshot. | Removes the window where a reload leaves a partial timetable and the match rate collapses. | Truncate-and-reload (a visible correctness gap); per-update database lookups (tens of thousands of point reads a minute). |
| 2026-09-21 | Store raw `schedule_relationship` integers, not enums. | TfNSW emits at least one value the current spec no longer defines; a constrained type would reject or mis-bucket it. | Postgres `ENUM` / Go typed constants with an exhaustive switch. |
| 2026-09-21 | Content hash (`sha256`) is the natural key for a schedule bundle. | ETag and Last-Modified are not reliably stable or reliably changing. | ETag-only comparison; download-date comparison. |
| 2026-09-21 | Observations natural key = `(service_date, feed_id, trip_id, stop_sequence, feed_ts)`; `ON CONFLICT DO NOTHING`. | Makes at-least-once ingestion safe; doubles as the per-trip read index; includes the partition key, which Postgres requires. | Surrogate `bigserial` plus a unique index (same bytes, no benefit); dedupe in application memory only (lost on restart). |
| 2026-09-21 | Change filter before the queue: write only when delay or status changes. | It is the only thing that makes the storage budget work; compression would compress redundancy instead of removing it. | Write everything and aggregate later (write rate, not size, is the constraint); sampling (loses the transitions, which are the signal). |
| 2026-09-21 | `observations` partitioned by RANGE on `service_date`, daily, with a DEFAULT partition. | Retention becomes `DROP TABLE`, which is instant and returns space. A default partition means a write never fails. | `DELETE` + `VACUUM FULL` (needs a full table copy); partition by `feed_ts` (queries are by service date, and after-midnight trips would land in the wrong day). |
| 2026-09-21 | Rollup buckets keyed on `bucket_start timestamptz` (UTC hour), not local hour. | The April DST transition makes local hour 02 ambiguous. | Local-hour integer buckets; bucketing by service date only (too coarse for the product question). |
| 2026-09-21 | Rollup takes the last observation per `(service_date, feed_id, trip_id, stop_sequence)` via `DISTINCT ON`. | A trip updated fifty times must not outweigh one updated twice. | Averaging all observations; counting every update. |
| 2026-09-21 | `ON CONFLICT DO UPDATE` on rollup insert. | Late-arriving data must be able to correct a bucket. | `DO NOTHING` (first write wins, which is wrong for late data). |
| 2026-09-21 | Retention refuses to drop a partition unless `rollup_state.watermark` has passed it. | Otherwise a rollup outage silently destroys history. | Time-based retention with no guard. |
| 2026-09-21 | Drop observations when the ingest queue is full; never block the producer. | Blocking back-pressures into the poller, which misses polls and loses data permanently; a dropped observation is re-sent 15 s later. | Blocking send; unbounded buffer (OOM on a 1 GB fallback VM); blocking with timeout (a slower drop). |
| 2026-09-21 | Shutdown order: cancel pollers → drain HTTP → wait for producers → close channel → wait for writer → close pool. | Closing the channel while a producer is alive panics; cancelling the writer's context with the rest loses the final batch. | A single `errgroup` context for everything. |
| 2026-09-21 | Writer gets a context derived from `context.Background()`, not the shutdown context. | It must outlive the cancellation that triggered shutdown in order to flush. | Shared context. |
| 2026-09-21 | Feed poll interval 15 s, global limiter at 4 req/s, daily budget 55,000. | The feeds refresh every 15 s, so faster polling buys nothing. The documented default account plan is 60,000 requests/day at 5/s; 4/s and 55,000 leave headroom for restarts and the schedule downloads. | Polling at 5 s (three times the quota for no freshness); per-feed limiters (the quota is per account). |
| 2026-09-21 | `feed_ts` is always `FeedHeader.timestamp`, never our fetch time. | Makes replays idempotent and makes the freshness metric meaningful. | Fetch time (breaks idempotency, inflates freshness by the poll interval). |
| 2026-09-21 | Detect clock skew; never correct it. | Corrected data cannot be reconciled with the source. | Offset correction per feed. |
| 2026-09-21 | Prefer `delay` over `time`-minus-schedule when both are present and disagree. | `delay` is internally consistent with the producer's own timetable. | Prefer absolute `time`; average the two. |
| 2026-09-21 | Store unmatched observations rather than dropping them. | The unmatched rate is the health signal for the matcher; dropping them hides the failure. | Foreign key to `trips` (would make them unstorable). |
| 2026-09-21 | Service day start = noon local minus 12 hours, per the GTFS definition. | Correct on both DST transition days; "midnight local" is not. | Midnight local; UTC truncation. |
| 2026-09-21 | Stop times stored as integer seconds, not `time` or `interval`. | `time` cannot hold `25:10:00`; `interval` arithmetic across DST depends on the session timezone. | `time`, `interval`. |
| 2026-09-21 | `import _ "time/tzdata"`; `servicetime` panics at init if the location will not load. | The distroless runtime image has no zoneinfo; a silent UTC fallback corrupts every service date. | Installing `tzdata` in the image (works, but couples correctness to the image); returning an error (callers would ignore it). |
| 2026-09-21 | Runtime image `gcr.io/distroless/static-debian12:nonroot`, built for `linux/arm64`. | No shell, non-root, tiny; the Oracle Always Free VM is Ampere arm64. | Alpine (musl surprises if CGO is ever needed); scratch (no CA certificates for TLS to the TfNSW API). |
| 2026-09-21 | Config from env vars; the feed catalogue from a committed JSON file. | A list of objects does not fit an env var legibly, and the feed list is not a secret. | All-env (unreadable); YAML/TOML (another parser for one file); a config framework. |
| 2026-09-21 | Logs are JSON to stdout via `log/slog`; no files, no rotation. | Docker captures stdout; rotation is one fewer thing to operate. | zerolog/zap (no measured need); file logging. |
| 2026-09-21 | On-time window defaults to −60 s to +300 s, configurable. | A concrete default is needed to build against. It is **not** TfNSW's published definition — see §16. | Adopting a published definition without verifying it; leaving it undefined. |
| 2026-09-21 | Retention default 14 days for raw; rollups kept indefinitely. | Two weeks is enough for drill-down; rollups are small enough to keep forever. | 30 days raw (storage risk before measurement); 7 days (too little for a week-over-week comparison). |
| 2026-09-21 | `/healthz` does not check dependencies; `/readyz` does. | A Postgres blip must not cause a container restart loop. | A single health endpoint that checks everything. |
| 2026-09-21 | Integration tests run against a real Postgres in a GitHub Actions service container, behind a build tag. | Mocking Postgres would not test partitioning, `ON CONFLICT`, or the DDL. | testcontainers-go (a dependency; the service container is free); sqlmock. |
| 2026-09-21 | Fixtures are recorded live responses committed to `testdata/`; tests never call the live API. | Deterministic, offline, and does not consume quota in CI. | Live calls in CI (flaky, quota-consuming, needs a secret). |
| 2026-09-21 | Vehicle positions are not ingested in v1. | Trip updates alone answer the product question; vehicle positions double the quota cost. | Ingesting both from the start. |
| 2026-09-21 | Stage 2 is the acceptable stopping point. | At the end of Stage 2 the project demonstrates matching, history, deployment and CI, which is the whole story. | Stage 1 (no schedule matching, the hard part); Stage 3. |
| 2026-09-21 | Module path `github.com/zigzaggoose/headway`; `go` directive `1.27`, toolchain 1.27.1. | The directive is the minor version so any 1.27.x toolchain builds it; pinning the patch in `go.mod` would force CI and the laptop to match exactly for no benefit. | `go 1.27.1` in the directive; a vanity module path (breaks `go install` from the repo URL). |
| 2026-09-21 | Package directories are created when their first real file is written, not up front as empty placeholders. | Git does not track empty directories, and placeholder files that exist only to hold a directory open are files nobody deleted later. §5 is the destination, not the first commit. | `.gitkeep` in every package directory; stub `.go` files per package (would not compile, or would compile to nothing). |
| 2026-09-21 | Migrations are applied by the service at startup; `make migrate` runs `cmd/headway -migrate-only` rather than a separate `cmd/migrate`. | Definition of Done #8 requires `docker compose up` to migrate with no manual SQL, so startup must migrate anyway; a second entry point would be a second code path to the same runner. The flag is unimplemented until `internal/store` lands, which is the next Stage 1 item. | `cmd/migrate` (a fourth binary for one call); `psql -f` from the Makefile (manual SQL, contradicts DoD #8). |
| 2026-09-21 | `migrations/0001`–`0003` transcribed verbatim from §6.2; `0004_indexes.sql` intentionally holds only a comment. | Keeping the DDL byte-identical to the design document means a divergence is visible in a diff rather than discovered in production. An empty 0004 reserves the number so the first measured index does not have to renumber. | Writing the DDL fresh from the prose; deferring 0004 until an index exists (the number would then collide with any other migration added first). |
| 2026-09-21 | `.env.example` carries every variable with its default, not only the two required ones. | §8 is the reference, but a developer copies the example file; a short example silently hides the knobs and the defaults drift apart. | Two-line example listing only `TFNSW_API_KEY` and `DATABASE_URL`. |
| 2026-09-21 | Secrets are a `Secret` string type with redacting `String`/`GoString`/`MarshalText`/`LogValue`, not a `redact()` helper called at log sites. Supersedes the `redact()` helper named in §8.2. | A helper has to be remembered at every call site and a new log line is exactly where it gets forgotten; a type makes leaking the value require an explicit `Reveal()`. | `redact()` in `obs/log.go`; keeping secrets as plain strings and relying on review. |
| 2026-09-21 | `config.Load` collects every problem and returns them joined, rather than failing on the first. | A misconfigured deployment should learn all of its mistakes in one startup attempt; failing on the first turns a five-variable mistake into five restarts. | Fail-fast on the first error (fewer lines, worse to operate). |
| 2026-09-21 | A value that fails to parse falls back to its default and records an error. | The rest of validation stays meaningful, so one unparseable duration does not hide four other mistakes behind cascading zero-value errors. | Zero value on parse failure (produces nonsense follow-on errors); abandoning validation at the first parse failure. |
| 2026-09-21 | The feed catalogue is parsed with `DisallowUnknownFields`. | A misspelled key would otherwise be discarded in silence, leaving a feed with an empty URL or an unset `enabled` flag — the exact failure the catalogue exists to prevent. | Lenient decoding; a JSON schema (a dependency for one file). |
| 2026-09-21 | `HEADWAY_ENABLED_FEEDS` overrides the catalogue's `enabled` flags rather than intersecting with them. | The env var is the per-deployment control and the file is the default; intersecting would make enabling a feed need both a commit and an env change. | Intersection (needs two edits to turn a feed on); env-var-only (loses the committed default). |
| 2026-09-21 | A validation rule exists only where violating it breaks something concrete; the §8 "what breaks if it is wrong" column is the test. Cross-field rules enforced: batch ≤ queue, shutdown grace > flush interval, readiness age > poll interval, on-time bands strictly ordered, pool ≥ 2. | Rejecting merely unusual values turns configuration into a fight; rejecting values that guarantee data loss (a dropped final batch, writes into the default partition) turns a silent production failure into a startup error. | Validating every §8 guidance note as a hard bound; validating nothing beyond parseability. |
| 2026-09-21 | `make run` sources `.env` in the shell rather than the binary loading it. | Twelve-factor config and no dotenv dependency, while a laptop run still matches what Compose hands the container via `env_file`. | `godotenv` in the binary (a dependency, and a file read that production never does); exporting by hand before every run. |
| 2026-09-21 | The Sydney Trains **schedule** bundle is fetched from `/v1/gtfs/schedule/sydneytrains`; only the realtime feed is on `/v2`. | Verified against the live API: the v2 schedule path returns 404 and the v1 path returns a 10.7 MB zip. The v2 migration applied to realtime endpoints, not to static bundles. | Assuming both are on the same version, which is what this document said until it was checked. |
| 2026-09-21 | No conditional requests on the realtime feed; `If-Modified-Since` on the schedule download only. | The realtime response carries no `ETag` and no `Last-Modified`, and `HEAD` returns 502, so there is nothing to condition on. The schedule response carries `Last-Modified`, where it saves a 10.7 MB transfer a day. | Sending `If-None-Match` regardless (a header the server ignores, and a 304 branch that can never be reached on that path); a `HEAD` probe before each poll (502, and it would double the request count against the quota). |
| 2026-09-21 | The content hash stays the authority for "is this bundle new", with `Last-Modified` as a bandwidth optimisation only. | `Last-Modified` reflects when the file was published, not whether its contents differ; the hash answers the question the loader actually asks. Confirms the decision already made above. | Trusting `Last-Modified` alone to skip a load. |
| 2026-09-21 | The poller calls a `Handler` on its own goroutine instead of emitting to a `chan FeedResponse`. Supersedes the channel in §4.2 (2). | §9.4 already puts decode and match on the poller's goroutine, so the channel would have been a hop between two points in the same goroutine — a buffer to size, a second shutdown ordering problem, and a place for responses to queue behind a slow consumer. The one bounded channel in the design stays where backpressure belongs, in front of the writer. | A channel per feed (buffer sizing and shutdown ordering for no gain); one shared response channel (serialises every feed through a single consumer). |
| 2026-09-21 | The token bucket and the daily budget live in one hand-rolled `Limiter` rather than `golang.org/x/time/rate` plus a counter. | Both limits are per account and must be checked together: with two independent limiters there is no single answer to "may this request go out", and the daily counter could admit a request the bucket then delays past midnight. The bucket underneath is about thirty lines. Direct dependencies remain at zero. | `x/time/rate` plus a separate counter (a dependency, and a race between the two limits). |
| 2026-09-21 | Jitter is added to the poll interval, never subtracted. | `FEED_DAILY_BUDGET` is computed from `FEED_POLL_INTERVAL`; jitter centred on the interval would poll faster than budgeted half the time. Spreading later, never earlier, keeps the daily request count bounded by the interval. | Symmetric jitter (over-spends quota); no jitter (every poller fires in the same instant and trips the per-second throttle). |
| 2026-09-21 | An unrecognised 403 is treated as the rate limit, not the quota stop. | The two differ only by `X-Error-Detail`, and the errors are asymmetric: reading a quota stop as a rate limit burns the rest of the day on retries, while reading a rate limit as a quota stop idles every feed until UTC midnight for what seconds of backoff would fix. The recoverable reading is the safe default. | Treating an unknown 403 as quota (hours of avoidable downtime); failing the poller (a 403 is not fatal). |
| 2026-09-21 | A limiter reservation is consumed when granted, even if the context is cancelled before the request goes out. | Returning the token needs a second lock round-trip on a path that only runs during shutdown, and over-counting the quota errs toward fewer upstream requests, which is the safe direction. | Reservation cancellation (`x/time/rate`'s `Reservation.Cancel` shape) for a path that runs once per process exit. |
| 2026-09-21 | The first poll happens immediately at startup, not after one interval. | A restart should produce data at once, and the change filter is empty after a restart anyway, so that first fetch is the one that repopulates it (§9.3 case 2). | Waiting one interval (a poll interval of silence after every deploy). |
| 2026-09-21 | `Poller.Run` returns nil on context cancellation, and classifies a fetch error against its own context before blaming the feed. | A clean shutdown is not a failure; the caller knows whether it asked for one. Returning `ctx.Err()` would make every normal shutdown log an error, and counting a cancelled fetch as a timeout would inflate the backoff. | Returning `ctx.Err()`; classifying purely on the error value (cannot tell our cancellation from an upstream stall). |
| 2026-09-21 | Migrations are embedded through a root `embed.go` in `package headway`, and the runner takes an `fs.FS`. | `//go:embed` cannot reach outside its own directory, and §5 puts `migrations/` at the repository root where a reader looks for them. Taking an `fs.FS` rather than reaching for the global also lets the runner's own failure modes be tested against `fstest.MapFS` with no database at all. | Moving `migrations/` under `internal/store` (hides the schema from anyone browsing the repo); reading migrations from disk at runtime (a bind mount, and Definition of Done #8 says no manual steps). |
| 2026-09-21 | An applied migration is checksummed, and a changed file fails startup rather than re-running or being ignored. | §14 says never edit an applied migration. Without a checksum that is a convention people break by accident; with one it is a failed startup naming the file and the date it was applied. | Trusting the version number alone (an edited file diverges silently from the schema it produced); re-running changed migrations (destructive and unordered). |
| 2026-09-21 | A migration numbered below one already applied is rejected. | It would run against a schema its author never saw. This is the failure mode of two branches each adding "the next" migration and both merging. | Applying it anyway in version order (runs late against an unexpected schema); ignoring it (the file exists and does nothing, which is worse). |
| 2026-09-21 | Each migration runs in its own transaction, and the `schema_migrations` row is written inside it. | Half-applied DDL recorded as applied cannot be recovered without hand-editing the database. One transaction per migration also means a failure stops at a known point rather than rolling back work that succeeded. | One transaction for all migrations (a failure at the end discards a large successful DDL run); recording outside the transaction (the two can disagree). |
| 2026-09-21 | Migrations take a Postgres advisory lock for the duration of the run. | `make migrate` and a starting container can collide, and concurrent `CREATE TABLE` produces an error at best. This is not the distributed locking §2 rejects — nothing coordinates ingestion, and exactly one instance still writes. | No lock (a race between a deploy and a manual run); a lock table (reinvents an advisory lock, and needs its own migration). |
| 2026-09-21 | `store.Retry` retries only three SQLSTATEs (40001, 40P01, 53300) plus errors `pgconn.SafeToRetry` accepts, for three attempts after the first. | A constraint violation fails identically on the second attempt, so retrying it spends the backoff for nothing and delays the error the caller needs. `SafeToRetry` is pgx's own answer to whether a statement could already have taken effect, which is the question that matters for a write. | Retrying every error (turns a bug into a slow bug); retrying on error text (breaks on a Postgres upgrade). |
| 2026-09-21 | A `DATABASE_URL` that pgx rejects produces a fixed error with no detail. | pgx puts the connection string in that error and the connection string carries the password; `config.Load` has already checked that it parses as a postgres URL, so the detail that is dropped is small. | Wrapping pgx's error (leaks the password into any log that catches a startup failure). |
| 2026-09-21 | Each integration test runs in its own `test_<nanos>_<rand>` schema and drops it afterwards. | Tests can then run in parallel against one database, and a failed test leaves nothing behind for the next one to trip over. | A shared schema with truncation between tests (serialises the suite, and a panic leaves state behind); a database per test (slow to create). |
| 2026-09-21 | The decoder unmarshals with `proto.UnmarshalOptions{AllowPartial: true}`. | GTFS-realtime is proto2 and marks `FeedMessage.header` and `TripUpdate.trip` **required**, so a strict unmarshal makes one malformed entity destroy the whole response — on this feed, 3,939 updates lost because of one. Absence is already handled field by field, and a wire-format error still fails. Regression test: `TestDecode_OneMalformedEntity_DoesNotCostTheRest`. | Strict unmarshal (one bad entity costs a whole poll); decoding entities individually (the wire format does not allow it without re-implementing the parser). |
| 2026-09-21 | `RawUpdate.TripLevel` marks an update describing a whole trip rather than one stop. | 103 of 488 entities in the live sample carry no stop-time updates at all, which is how a cancellation arrives. Without this they decode to nothing and cancellations are invisible in the rollup (§9.1 case 3). The matcher expands one into an observation per scheduled stop. | Dropping them (loses every cancellation); synthesising stops in the decoder (it has no schedule, and must not have one). |
| 2026-09-21 | An update with no times is dropped only when it is a plain SCHEDULED stop on a normal trip; SKIPPED, NO_DATA, and any stop on a cancelled or added trip are kept. Refines §9.1 case 6. | The relationship is itself the information. Dropping every timeless update, as the original rule read, would make `n_skipped` and `n_cancelled` permanently zero in the rollup — the two columns that exist to count exactly these. | Dropping all timeless updates (silently zeroes two rollup columns); keeping all of them (a scheduled stop with no time repeats what the timetable already says). |
| 2026-09-21 | `DecodedFeed.Dropped` is `map[DropReason]int`, not the `int` §4.2 specified. | The metric is `headway_updates_dropped_total{reason}` (§10.3); a single count would have to be re-derived to be useful, and the reason is what tells a stale schedule apart from a malformed feed. | A plain count (loses the label the alert needs). |
| 2026-09-21 | Schedule-relationship constants are untyped-by-design `int32` names, not a Go enum type. | Confirms the earlier decision with evidence: 18.5 % of live updates carry `REPLACEMENT` (5), which the current specification removed. A typed enum with an exhaustive switch would mis-bucket or reject nearly a fifth of this feed. | A Go enum type with a `default` case (works, but invites an exhaustive switch in the next package). |
| 2026-09-21 | Observations are keyed on `stop_id` rather than `stop_sequence`; `stop_sequence` becomes a nullable column filled from the timetable. Answers Open Question 11; applied as `migrations/0005`, leaving `0002` untouched. | The feed never sends `stop_sequence` (0 of 3,836 measured), so it can only come from the timetable — which an unmatched observation has no access to. Unmatched observations must be storable because the unmatched rate is the matcher's health signal. | A sentinel `stop_sequence` for unmatched rows (every unmatched stop on a trip collides, and in Stage 1 that is every stop); a synthetic per-message ordinal (shifts when the producer omits earlier stops, so the same stop changes key between polls and the filter stops suppressing). |
| 2026-09-21 | A batch is one `INSERT ... SELECT * FROM unnest(...) ON CONFLICT DO NOTHING` statement. | One statement means the batch succeeds or fails whole with no per-row error handling (§10.1), and one round trip rather than one per row. `CopyFrom` is faster still but cannot express `ON CONFLICT`, and idempotency is worth more here than the difference. | `pgx.Batch` of single-row inserts (a round trip's worth of parsing per row); `CopyFrom` into a temporary table then insert-select (two statements and a temporary table per batch, for a gain worth measuring only if the writer ever becomes the bottleneck). |
| 2026-09-21 | An observation dropped by a full queue is forgotten by the change filter. | The filter records a value as written when it admits it. If a full queue then discards it, leaving that record in place suppresses the *next* identical observation too — so one full queue loses the value until it next changes, not for one poll. Forgetting costs one redundant write and bounds the damage to the observation actually dropped. | Filtering after the queue (a redundant observation would then occupy queue space, which is exactly what the filter exists to prevent); leaving the record (turns a transient drop into a lasting gap). |
| 2026-09-21 | The change filter's key holds the service date as a formatted `YYYY-MM-DD` string, not a `time.Time`. | A `time.Time` carries a location pointer and a monotonic reading, so two values naming the same day can compare unequal. In a map key that is a filter which silently stops suppressing — the most expensive possible failure of this component. | `time.Time` in the key (unequal for equal days); truncating to midnight UTC (wrong: a service date is a local-calendar concept, not an instant). |
| 2026-09-21 | Filter eviction removes a tenth of the map, oldest first, when the bound is reached. | The scan is then amortised over many admissions instead of running on every one past the bound. Eviction costs a redundant write the next time that key appears and never produces a wrong row, so approximate is good enough. | Exact LRU (a linked list and a second map, to decide which redundant write to pay for); evicting one entry per admission (a full sort per observation at the bound). |
| 2026-09-21 | Ingest counters are read through `Stats()` snapshots — atomics in the writer, mutex-guarded in the filter — and the four writer counters are not read at one instant. | `/v1/admin/stats` and the metrics collectors read these while the writer goroutine is mutating them, which was an actual data race found by review and reproduced under `-race`. Nothing depends on the counters agreeing with each other, and taking a lock to make them agree would put the reporting path inside the write path. | Plain counters (the race); a mutex around all four (reporting contends with writing); a single seqlock (complexity for a consistency nobody needs). |
| 2026-09-21 | `Filter.Forget` does not decrement the admitted counter. | "Admitted" means "passed the change filter", which a dropped observation did; the queue refusing it afterwards is a separate event with its own counter (§10.3 has both). Decrementing would also undercount whenever the entry being forgotten belongs to a later admission than the one that was dropped. | Decrementing (drifts out of line with reality and conflates two metrics). |
| 2026-09-22 | Deployment target is an Azure B2pts v2 (Ampere arm64, 2 vCPU, 1 GiB) on an Azure for Students subscription, Australia East. Supersedes the Oracle Cloud Always Free target in §3 and the fallback in §16 q6. | Oracle Always Free capacity was never available to this account. B2pts v2 is arm64, so nothing about the build, the runtime image or `make cross` changes, and it is free-tier eligible for 12 months. | Google Cloud e2-micro and Azure B1s (both x86 — would have forced `GOARCH=amd64` and contradicted the runtime-image row above); Hetzner CAX11 (arm64, 4 GiB, ~€3.79/mo — more RAM and no expiry, but not free and not already paid for). |
| 2026-09-23 | Deployment target is a BinaryLane Standard 1 GB VM in Sydney, x86-64, AUD 4.90/month ex GST. Supersedes the 2026-09-22 Azure row and, for architecture only, the 2026-09-21 runtime-image row: builds now target `linux/amd64`. §13's database-size target drops from 20 GB to 10 GB to fit a 20 GB disk alongside the OS and images. | Oracle Always Free rejected the signup (debit card, no credit card available). Azure for Students is disabled, not billed, when its credit runs out, which silently stops a capture meant to run indefinitely. A paid VM around AUD 5/month was acceptable. BinaryLane is the cheapest option checked on 2026-09-23 and needs no signup approval. Switching to amd64 costs only the `make cross` target now, because `deploy/` does not exist yet. The measured ~270 MB/day puts 7 days of one feed at ~1.9 GB. | Oracle PAYG (card rejected); Azure for Students (expiry cliff); Hetzner CAX11 (arm64, 4 GB, but €5.99/month after the June 2026 increase, ~AUD 11, and in Europe); Hetzner CX23 (€5.49 + IPv4, Europe); BinaryLane 2 GB (AUD 9.80, needed only for more than one feed). |
| 2026-09-23 | `ServiceDayStart` is noon minus twelve hours as GTFS specifies, which on the October transition is 23:00 the previous evening and on the April transition 01:00. §7.3's comment and §9.2 case 3 had assumed local midnight on the October day and are corrected. `CandidateServiceDates` takes the overlap as a parameter and decides ownership by `ServiceDayStart`, not the calendar date. | The rule was already chosen in §9.2; the examples contradicted it. With a midnight-based owner, 23:00–24:00 before the October change and 00:00–01:00 on the April change are attributed to the wrong service date. The overlap is a parameter because components never read config (§8). | Local midnight as the reference (contradicts GTFS and the chosen approach); calendar-date ownership (wrong for two hours a year); reading `SERVICE_DAY_OVERLAP_H` from a package variable. |
| 2026-09-23 | Stage 1 stores unmatched observations through `match.Unmatched`, which skips and counts what it cannot key: trip-level updates (103–152 per poll, the cancellations), updates with no `stop_id`, and an unreadable `start_date`. Without `start_date` the service date is the newest `CandidateServiceDates` entry. | Rows start accumulating now, not at Stage 2, and the feed is not archived anywhere else. Trip-level updates need the timetable to expand into stops (§9.1 case 3). A bad `start_date` is counted rather than replaced, because §9.2 case 7 trusts it. The conversion lives in `internal/match` because §7.3 puts RawUpdate → Observation there; `main` stays wiring only. | Waiting for the matcher before storing anything (loses days of data); a sentinel stop id such as `~trip` for trip-level rows (a fake key the matcher would have to undo); falling back to the clock on a bad `start_date`. Known cost: between 00:00 and 06:00 a trip without `start_date` that began the previous evening gets today's service date until the matcher can check the schedule. |
| 2026-09-23 | The latest-state cache replaces a feed's whole snapshot on every poll and applies `CACHE_TTL` to the feed, on read, instead of expiring entries individually with a sweep goroutine. A trip's next stop is its first stop-time update in feed order. | A trip missing from the latest poll has finished or been withdrawn; per-entry expiry would keep showing it for up to `CACHE_TTL`. Whole-snapshot replacement needs no goroutine and no per-trip bookkeeping, and readers take the lock only to copy map pointers. The feed never sends `stop_sequence`, so feed order is the only unmatched signal for which stop is next. | Per-trip TTL plus a one-minute sweep (§4.2 as first written: stale trips, one more goroutine); merging each poll into the previous state (same staleness). |
| 2026-09-23 | Four additions to §7, approved by the user: a `not_found` error code for unknown paths; `summary.early`; a 1000 cap on `/now` `limit`; `reasons` beside the envelope on a `/readyz` 503. | §7.2 requires the envelope on every non-2xx and no existing code fit an unknown path. Without `early` the status buckets would not sum to `active_trips`. `limit` needs a range to be "out of range"; 1000 matches `/v1/lines`. The readiness reasons need a home that keeps the envelope's shape. | Letting the mux answer 404/405 in plain text; folding early trips into `on_time`; an unbounded limit. |
| 2026-09-23 | Stage 1 API semantics without a schedule: a route is known iff it is in a fresh feed snapshot; readiness checks the database and feed freshness only; feed freshness for readiness is the newest snapshot in the latest-state cache rather than poller state. No per-request timeout or per-IP limiter yet. | Nothing else can answer "does this route exist" until the schedule loader; readiness from the cache needs no new shared state in the pollers. `/now` reads memory, so a handler timeout guards nothing until the history endpoints query Postgres. | A 200 with empty trips for any unknown id (hides typos); exporting poller stats for readiness (more cross-goroutine state for the same answer). Per-IP limiting and `http.TimeoutHandler` land with the history endpoints in Stage 2. |

---

## 16. Open Questions

Each needs the human's input. Each has a default that will be used until it is answered, so nothing is blocked.

| # | Question | Default until answered |
|---|---|---|
| 1 | **Is the TfNSW account plan actually the default one?** The published documentation describes a default "Bronze Plan" with a 60,000-request daily quota and a 5-requests-per-second rate limit. Confirm this is what the key actually has, since exceeding it returns HTTP 403 rather than a soft failure, and the whole poll-interval budget depends on it. | Assume 60,000/day and 5/s. Run with `FEED_RATE_LIMIT_RPS=4` and `FEED_DAILY_BUDGET=55000`. |
| 2 | **What is the definition of "late" that this project should report?** TfNSW publishes its own on-time performance definitions, and they differ by mode (suburban trains use a tolerance measured in minutes; other modes differ). Headway should either match the official definition, so the numbers are comparable, or state plainly that it uses its own. Look up the current published definition before claiming parity. | Use Headway's own: early < −60 s, on time −60 s to +300 s, late > +300 s, very late > +900 s. State in the README that this is Headway's definition, not TfNSW's. |
| 3 | **Arrival or departure?** On-time performance can be measured at arrival (what a passenger waiting at the destination sees) or departure (what a passenger boarding sees). They differ at terminus stops and at stops with long dwell times. | `observed_delay_s` = `departure_delay` when present, else `arrival_delay`, else `NULL`. Rationale: most stops in the feed are intermediate and the boarding passenger is the primary user. Both raw values are stored, so the choice is reversible without re-ingesting. |
| 4 | **How do cancellations count in the on-time percentage?** Excluding them flatters the number; counting them as "not on time" conflates two different failures. | Excluded from `on_time_pct` and reported as a separate `n_cancelled` column, which the API returns alongside. The README says so explicitly. |
| 5 | **Which feeds at Stage 3, and in what order?** Each feed costs quota and adds edge cases. Buses is by far the largest. | Order: metro, ferries, light rail, then buses last. Enable them one at a time and check the daily request count after each. |
| 6 | **ANSWERED 2026-09-23 — see the Decision Log.** Supersedes the 2026-09-22 answer (Azure). Oracle Always Free rejected signup with a debit card; the Azure for Students plan was dropped for its expiry cliff. | **Decided:** BinaryLane Standard 1 GB, Sydney, Ubuntu 24.04, x86-64. 1 GB RAM and 20 GB disk are the constraints, so: one feed, `RETENTION_DAYS=7`, `DB_MAX_CONNS=5`, Postgres `shared_buffers=128MB`, and a 2 GB swapfile. Paid by debit card or by prepaid PayPal funds; with PayPal an empty balance suspends the server and the capture stops, so record the top-up date in `deploy/vm-bootstrap.md`. Resize to the 2 GB plan (AUD 9.80) if Stage 3 enables a second feed. |
| 7 | **Retention window.** 14 days is a guess made before measuring. | 14 days as the `RETENTION_DAYS` default; the 1 GB deployment overrides it to 7 (q6). Revisit once `docs/storage.md` has a real bytes-per-day figure. |
| 8 | **Custom domain?** Roughly $15–25 AUD/year, plus TLS to configure. | No domain. Serve on the VM's IP over HTTP for the demo, or put Caddy in front if a domain is bought later. Not on the Stage 1–3 critical path. |
| 9 | **Should vehicle positions be ingested at all?** They would allow a map and a "where is the train" view, at roughly double the quota cost and a second table. | No. Explicitly a non-goal for v1 (§2). Revisit only after Stage 4 is complete. |
| 11 | **ANSWERED 2026-09-21 — see the Decision Log.** The observations primary key included `stop_sequence`, and the feed never sends one. Order-1 and order-2 matches can take it from the timetable, but an unmatched update (order 4) has no `stop_sequence` and no way to get one — and the column is `NOT NULL` and part of the key. Writing unmatched observations is what tells us the match rate is falling (§15), so they must be storable. The options: a sentinel `stop_sequence` for unmatched rows, which collides when one trip has several unmatched stops; swapping `stop_id` for `stop_sequence` in the key, which collides when a trip visits one stop twice; or adding a synthetic per-update ordinal. This changes `migrations/0002`, so it needs a decision before the matcher is written. | **Decided:** key on `(service_date, feed_id, trip_id, stop_id, feed_ts)`, `stop_sequence` nullable, applied in `migrations/0005`. The cost — a loop service losing its second visit within one feed timestamp — is asserted by `TestWriter_LoopService_LosesTheSecondVisitInOneFeedTimestamp` rather than left to be discovered. |
| 10 | **Is the repository public?** It affects whether GitHub Actions minutes are free and whether the API key can ever appear in a CI log. | Public. Therefore: no secrets in CI logs, no live API calls in CI, and `.env` stays in `.gitignore` from the first commit. |

---

## 17. Glossary

| Term | Definition |
|---|---|
| **AEST / AEDT** | Australian Eastern Standard Time (UTC+10) and Australian Eastern Daylight Time (UTC+11). Sydney switches between them twice a year. |
| **ADDED** | A GTFS-realtime `TripDescriptor.schedule_relationship` value meaning the trip is not in the static timetable. Such trips can never be matched; they are excluded from the match-rate metric. |
| **Backpressure** | Letting a slow consumer slow a fast producer. In Headway the bounded channel is the only backpressure point, and it drops rather than blocks. |
| **Bronze Plan** | The name TfNSW's documentation gives to the default API account plan, documented as 60,000 requests per day at 5 per second. See Open Question 1. |
| **CANCELED** | A `TripDescriptor.schedule_relationship` value meaning a scheduled trip will not run. Spelled with one L in the specification. |
| **Change filter** | Headway's component (5) that suppresses an observation whose delay and status are unchanged since the last write for the same trip and stop. The main storage-saving mechanism. |
| **COPY** | The PostgreSQL bulk-load protocol, exposed by pgx as `CopyFrom`. Faster than multi-row `INSERT` for batches. |
| **Default partition** | A partition of a partitioned table that receives rows matching no other partition. Present so writes never fail; a non-empty one is an alert. |
| **Freshness lag** | `ingested_at − feed_ts`: how long after the producer stamped the data it became queryable in Headway. The headline latency metric. |
| **GTFS** | General Transit Feed Specification. The static timetable format: a zip of CSV files (`trips.txt`, `stop_times.txt`, `calendar.txt`, …). |
| **GTFS-realtime / GTFS-R** | The realtime companion to GTFS: protocol-buffer messages fetched over HTTP, containing trip updates, vehicle positions and alerts. Headway consumes trip updates only. |
| **GTFS bundle** | One downloaded static GTFS zip. TfNSW publishes a "complete" bundle for all operators and separate per-mode bundles for operators that support realtime; Headway uses the latter. |
| **Idempotent write** | A write that can be repeated without changing the result. Here, `INSERT ... ON CONFLICT DO NOTHING` against the observations natural key. |
| **Match rate** | The share of realtime stop-time updates resolved to a scheduled trip. The primary health signal for the matcher. |
| **NO_DATA** | A `StopTimeUpdate.schedule_relationship` value meaning no realtime information is available for that stop. |
| **Observation** | Headway's unit of stored data: one delay measurement for one stop of one trip at one feed timestamp. |
| **On-time performance (OTP)** | The share of observations whose delay falls inside the on-time window. See Open Question 2 for the definition used. |
| **Partition pruning** | The planner's ability to skip partitions that cannot contain matching rows, given a predicate on the partition key. The reason every query filters on `service_date`. |
| **Protocol Buffers / protobuf** | Google's binary serialisation format. GTFS-realtime feeds are protobuf, so the response body is not human-readable. |
| **Quota limit** | TfNSW's per-account daily request cap. Exceeding it returns HTTP 403 with `X-Error-Detail: Account Over Quota Limit`. |
| **REPLACEMENT** | A `TripDescriptor.schedule_relationship` value removed from the current GTFS-realtime specification that TfNSW's documentation indicates the Sydney Trains feed still emits. The reason relationships are stored as raw integers. Verify its numeric value against a fixture. |
| **Rollup** | A pre-aggregated hourly summary of observations, written to `otp_stop_hourly` and `otp_route_hourly`. The only thing history endpoints read. |
| **Schedule version** | One immutable snapshot of a static GTFS bundle in the database, identified by content hash. Exactly one is active per feed. |
| **Service day / service date** | The GTFS notion of an operating day, running from noon-minus-twelve-hours on that date until the last service completes, which may be after midnight. Not a calendar day and not a UTC day. |
| **SKIPPED** | A `StopTimeUpdate.schedule_relationship` value meaning the vehicle will not call at that stop. |
| **Stale feed** | A feed whose `FeedHeader.timestamp` has not advanced across several consecutive successful polls. The request succeeds; the data is dead. |
| **stop_sequence** | The 1-based (in practice, monotonically increasing) position of a stop within a trip. May have gaps. The preferred stop anchor when present. |
| **StopTimeUpdate** | The GTFS-realtime message describing a predicted arrival or departure at one stop of one trip. Headway's raw input unit. |
| **TCB** | Transport Connected Bus. The label TfNSW uses for its regional bus services in some feed documentation. |
| **TfNSW** | Transport for NSW, the agency publishing the feeds, via its Open Data Hub at `opendata.transport.nsw.gov.au`. |
| **Throttle limit** | TfNSW's per-second request cap. Exceeding it returns HTTP 403 with `X-Error-Detail: Account Over Rate Limit`, distinct from the daily quota. |
| **Token bucket** | The rate-limiting algorithm used by the shared feed limiter: requests consume tokens that refill at a fixed rate, allowing a small burst. |
| **TripUpdate** | The GTFS-realtime message wrapping a `TripDescriptor` and its `StopTimeUpdate` list. |
| **Trip instance** | A specific running of a trip on a specific service date. Identified here by `(service_date, feed_id, trip_id)`, because `trip_id` alone repeats daily. |
| **Watermark** | The timestamp in `rollup_state` marking how far the rollup has progressed. Retention will not drop past it. |
