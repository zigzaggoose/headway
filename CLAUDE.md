# CLAUDE.md

Persistent instructions for working in this repository.

`PROJECT.md` is the design document and the source of truth: 1,700 lines covering
the data model, the HTTP contract, the hard problems, and an append-only Decision
Log. This file is the subset you need *every* session. Section references like
§9.3 point into `PROJECT.md` — follow them when a task touches that area.

---

## What this is

**Headway** polls the Transport for NSW GTFS-realtime feeds every 15 s, decodes the
protobuf, matches each stop-time update against the published timetable, and stores
the resulting delay observations in PostgreSQL. It answers two questions over HTTP:
what is happening on this line right now, and how has this stop or line performed
over the last N days. TfNSW publishes realtime data and timetable data but not
on-time performance, and does not archive the realtime feed — Headway captures it.

One process, one database, one VM. Everything runs in `cmd/headway`:

```
poller (1/feed) → decoder → matcher → change filter → bounded chan → batch writer → Postgres
                                            ↓                                            ↑
                                    latest-state cache → HTTP API ← rollup tables ← maintenance job
```

The numbered components and their single responsibilities are §4.2. Respect the
boundaries: the decoder knows nothing about the schedule, the matcher knows nothing
about SQL, the API never writes.

## Stack

Go 1.27 · PostgreSQL 18 · `net/http.ServeMux` · `log/slog` · Docker · deployed to
`linux/amd64`.

Three direct dependencies, and §3.1 caps v1 at six:

| | |
|---|---|
| `github.com/jackc/pgx/v5` | driver + pool; `v5.9.2` is the minimum (SQL-injection fix) |
| `github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs` | generated protobuf structs |
| `google.golang.org/protobuf` | `proto.Unmarshal` |

**Adding a direct dependency requires asking first**, plus a Decision Log row saying
what it replaced and why the standard library was not enough.

## Commands

The database must be running first:

```sh
docker run -d --name headway-dev-pg -p 5432:5432 \
  -e POSTGRES_USER=headway -e POSTGRES_PASSWORD=headway -e POSTGRES_DB=headway \
  postgres:18-alpine
```

Docker Desktop's daemon does not survive a reboot — `open -a Docker` if the socket
is missing.

| Command | What |
|---|---|
| `make run` | Run the service. Sources `.env`. |
| `make migrate` | Apply migrations and exit. Startup migrates too. |
| `make test` | Unit tests, `-race`, no cache. |
| `make test-integration` | Adds the `//go:build integration` tests. Needs a database; skips without `DATABASE_URL_TEST`. |
| `make lint` | `go vet` + `gofmt -l`. Both must be clean. |
| `make cross` | `GOOS=linux GOARCH=amd64` build. Catches the most likely deployment failure. |
| `make fixtures` | Record live feed fixtures. Consumes API quota — by hand only. |

There is no typechecker step beyond `go build` and `go vet`.

**Before any commit: `make lint && make test && make cross`.** Add
`make test-integration` when the change touches `internal/store` or `internal/ingest`.

## Where code goes

```
cmd/headway/      wiring only: config → components → signals. No logic.
cmd/fixturedump/  records testdata fixtures. The only thing that calls the live API.
embed.go          //go:embed migrations/*.sql. No logic.
internal/config/  env → validated Config. Fails fast. Never read after startup.
internal/feed/    HTTP client, token-bucket limiter, one poller per feed.
internal/gtfsrt/  protobuf → RawUpdate. EVERY nil check in the codebase lives here.
internal/ingest/  Observation, change filter, bounded channel, batch writer.
internal/store/   pool, migrations, hand-written SQL. No ORM.
migrations/       NNNN_name.sql, applied in order, checksummed.
testdata/         recorded .pb fixtures. Tests never call the live API.
```

Not yet built: `internal/api`, `internal/rollup`, `internal/obs`, `deploy/`, `web/`. §5 is the destination layout. Create a package when its first real file is written — no
placeholder files.

## Conventions

- Packages: one word, lowercase. No `utils`, `common`, `helpers`.
- Acronyms keep case: `HTTPClient`, `tripID`, `feedID`, `routeID`, `stopID`. Never `Id`.
- Tests: `TestThing_Condition_Expectation`. Subtests are plain sentences.
- SQL: lowercase `snake_case`, uppercase keywords, tables plural, duration columns end `_s`.
- Errors: wrap with `%w` at every boundary, include the identifying values (feed id,
  trip id, row count, service date). Lowercase, no trailing punctuation.
- **Never discard an error with `_`** without a same-line comment saying why.
- **Never `panic` outside `init`/`main`; never `log.Fatal` outside `main`.**
  `recover` belongs in exactly three places: each poller goroutine, the writer, and
  the HTTP middleware.
- Comments say *why*, never *what*. High density in three places only: service-day
  arithmetic, SQL over five lines, and any deliberate deviation from the obvious
  approach.
- **`// TODO:` is banned.** Either do it, add it to §16 Open Questions, or write the
  decision in §15.
- Commits: Conventional Commits, scope = package (`fix(ingest): ...`). Body says what
  was tried and rejected, referencing sections (`See §9.3`).

## Architectural constraints

These are load-bearing. Violating one silently corrupts data or loses it.

- **Timestamps.** `feed_ts` is *always* the feed's own `FeedHeader.timestamp`, never
  our clock — that is what makes replays idempotent. `ingested_at` is our clock.
- **Service dates** are Australia/Sydney service days, computed as noon minus twelve
  hours, *not* midnight and *not* a truncated UTC date. Stop times are integer
  seconds and may exceed 86400. §9.2.
- **`import _ "time/tzdata"`** must stay in `main`. The distroless runtime image has
  no zoneinfo; without it every DST calculation silently falls back to UTC.
- **Schedule relationships are raw `int32`**, never a Go enum with an exhaustive
  switch. 18.5 % of live updates carry `REPLACEMENT` (5), which the current spec
  removed.
- **The ingest channel is never blocked on.** Full queue → drop and count. Blocking
  back-pressures into the poller, which loses data permanently.
- **The writer's context must not derive from the shutdown context.** It has to
  outlive cancellation to flush. Shutdown order is fixed: cancel pollers → drain
  HTTP → wait for producers → close channel → wait for writer → close pool.
- **The API never writes and never blocks the ingest path.**
- **Writes are idempotent**: `ON CONFLICT DO NOTHING` against the natural key
  `(service_date, feed_id, trip_id, stop_id, feed_ts)`.
- **Retention is `DROP TABLE` on a daily partition**, never `DELETE`. Never drop a
  partition the rollup watermark has not passed.
- **Anything read across goroutines is atomic or mutex-guarded**, exposed as a
  `Stats()` snapshot. This was a real bug once.
- Every database call takes a context with a deadline. No unbounded calls.
- Components take `func() time.Time`, never call `time.Now()` outside `main`.

## Testing expectations

New code lands with tests in the same commit.

- Unit tests are table-driven, offline, deterministic. Tests **never** call the live
  API — use `testdata/*.pb`.
- Anything touching SQL gets a `//go:build integration` test against a real Postgres,
  in its own `test_<nanos>_<rand>` schema, dropped in `t.Cleanup`. Mocks do not
  exercise partitioning or `ON CONFLICT`.
- Every numbered edge case in §9.1/§9.2/§9.5 that you implement gets a named subtest.
- Run with `-race`. Concurrent code needs a test that reads shared state *while* it
  is being written; the obvious tests miss those races.
- A bug fix lands with a regression test that fails without it.
- Assert the real property, not an easier one nearby. If a test passes for the wrong
  reason, it is worse than no test.

## Security

- **The API key is a `config.Secret`**, whose `String`/`GoString`/`MarshalText`/
  `LogValue` all render `[redacted]`. `DATABASE_URL` too — it carries a password.
  `Reveal()` is the single, greppable way to get the real value.
- **Never put a secret in an error.** pgx puts the connection string in its parse
  errors, so those are deliberately not wrapped.
- The key travels as `Authorization: apikey <KEY>`, never a query parameter. Feed
  URLs must be https.
- **`.env` is gitignored and must never be committed.** The repository is public.
  No secrets in CI logs, no live API calls in CI.
- Verify before pushing anything credential-adjacent: grep the staged files, not just
  the ignore rules.

## Do not

- Do not edit an applied migration. Add a new numbered file. The runner checksums
  applied migrations and will refuse to start.
- Do not add an ORM, a router, a logging library, Kafka/NATS, Kubernetes, or
  Terraform. §2 rejects them by name.
- Do not build anything in §2 Non-goals — trip planning, map rendering, auth,
  prediction, service alerts, non-NSW agencies. A request for one is a change to
  `PROJECT.md` first, not a branch.
- Do not hard-code `route_id` or `stop_id` values outside tests. Real ids come from
  the TfNSW bundle, and the examples in §7 are illustrative.
- Do not call `GetX()` on a generated protobuf struct outside `internal/gtfsrt`.
- Do not start Stage 4 work (`web/`, Prometheus) before Stage 4.
- Do not round a measurement upward. "4,200 updates/min" beats "10,000+" because the
  follow-up is always "how did you measure that".

## Every session

1. **Read `PROJECT.md`**, then say in one line what state the project is in and what
   the next unchecked milestone task is. Read `HANDOFF.md` if present.
2. **When a decision in `PROJECT.md` turns out to be wrong, change the file.** Code
   that quietly contradicts it is a bug in `PROJECT.md`. Divergence is not allowed to
   accumulate.
3. **Every decision made while coding gets a §15 row** — date, reason, alternatives
   rejected. Append-only: supersede with a new row naming the old one, never an edit.
4. **Tick the §12 milestone checkbox** when an item is genuinely done, and replace
   §13 "baseline TBD" with real measurements as soon as one exists.
5. **Ask before**: changing `migrations/` other than adding a numbered file; adding a
   dependency; changing anything in §7 a client could depend on; changing a §8 default
   that affects stored data; refactors touching more than three files; introducing a
   new goroutine, channel, or lock.
6. **Do not ask**: adding a test, fixing a failing test that tests the right thing,
   renaming an unexported identifier, improving an error message, formatting.
