# HANDOFF.md

Session state as of **2026-09-24**, commit `b760785` (pushed, CI green). Delete or
rewrite this file when it stops being true. Permanent rules live in `CLAUDE.md`;
design lives in `PROJECT.md` (§12 milestones, §15 decisions, §16 open questions).

## Start here tomorrow

```sh
open -a Docker                      # the daemon does not survive a reboot
docker start headway-dev-pg         # or the docker run in README.md if it is gone
make lint && make test && make test-integration && make cross
```

All four should pass; CI runs the same steps. `make lint` includes staticcheck
(pinned v0.8.1, via `go run`; the first run downloads it). If `test-integration`
skips, `DATABASE_URL_TEST` is unset — it lives in `.env`, which `make` sources and
a bare `go test` does not.

**Check exit codes, never piped output.** `657d87b` was pushed with a failing test
because `make test-integration | grep | tail` hid the status. Run the target and
`echo $?`, or redirect to a file and grep that.

Nothing is left running: no `headway` process, only the dev Postgres container.

## Where the project is

| Stage | State |
|---|---|
| 1 — MVP | Code complete. **Open: the VM and the deployed demo** (user deferred paying). |
| 2 — Matching, history, CI | **Complete.** |
| 3 — All modes, scale, load test | **Complete except storage over a full day**, which needs 24 h on the VM. |
| 4 — Observability, front end | Not started. Needs the user's go-ahead (below). |

What runs today (`make run`, or `make up` for the Compose stack):

- **Five feeds** polled every 15 s: `sydneytrains`, `metro` (v2 endpoints),
  `sydneyferries`, `lightrail-cbdandsoutheast`, `lightrail-parramatta`. `buses` is
  in `config/feeds.json` with `"enabled": false` by the user's choice — too big for
  a 1 GB VM (numbers in `docs/quota.md`). Quota: 28,805 requests/day, 52 % of budget.
- **Timetables** load at startup (the stored version is published to the matcher
  *before* the pollers start) and daily at 03:30 Sydney; each feed's version is
  independent.
- **Match rates** live: trains 99.6–99.7 %; metro, ferries, Parramatta 100 %; CBD
  light rail 89.7 % — every miss is a cancellation of a `…-R:X` trip variant absent
  from its bundle, a data quirk rather than a matcher bug (§9.1 table).
- **Storage**: daily partitions; hourly rollups bucketed by **scheduled** hour
  (`observations.scheduled_at`, migration 0006), each hour rolled up **3 h after it
  ends** (`rollupSettle` in `cmd/headway/main.go`), plus exact all-routes rows per
  stop (`route_id = '~all'`). Retention drops a partition only once rolled up.
- **API**: `/v1/lines`, `/v1/lines/{id}/now`, `/v1/stops/{id}/now`, both history
  endpoints, `/v1/admin/stats`, `/healthz`, `/readyz`. Per-IP rate limit
  (`HTTP_RATE_LIMIT_RPS`, 429). Opt-in profiling on `PPROF_ADDR` (loopback only).
- **`headway -maintain-once`** / `make maintain`: one maintenance tick by hand.

The measured numbers are in the README and `PROJECT.md` §13; the load test and
the fix it led to are in `docs/loadtest.md`; storage so far is in `docs/storage.md`.

## Decisions waiting on the user

1. **Stage 4 or deploy first?** Asked at the end of the session, not yet answered.
   - Stage 4 needs **a new direct dependency**, `prometheus/client_golang`
     (`CLAUDE.md`: ask first, and add a §15 row saying what it replaces), and a
     Next.js/TypeScript page in `web/`.
   - Deploying closes Stage 1 and starts the 24 h storage measurement for Stage 3.
2. **The VM**: BinaryLane Standard 1 GB, Sydney, Ubuntu 24.04, about AUD 5.39 a
   month with GST, paid by debit card or prepaid PayPal funds. With PayPal, an
   empty balance suspends the server and stops the capture — record the top-up
   date. Deferred by the user; raise it again.

## Deploying, when the user says go

1. Create the VM, install Docker, clone the repository to `/opt/headway`.
2. Write `/opt/headway/.env` (mode 0600): `TFNSW_API_KEY`, a fresh
   `POSTGRES_PASSWORD` (`openssl rand -hex 24`), `RETENTION_DAYS=7`,
   `DB_MAX_CONNS=5` (§16 q6).
3. `docker compose --env-file .env -f deploy/docker-compose.yml up -d`. Compose
   already sets `stop_grace_period: 45s`, log rotation and `shared_buffers=128MB`.
4. Add a 2 GB swapfile (§16 q6).
5. **Write `deploy/vm-bootstrap.md` from the commands actually run**, as they run.
6. Check `/readyz`, and `/v1/admin/stats` (`default_partition_rows` must be 0).
   Take a `/v1/lines/{id}/now` screenshot for the README.
7. After 24 h, fill in `docs/storage.md` and §13 with the SQL at the end of that
   file, and tick the last Stage 3 box.

## Things that are true and easy to forget

- **TfNSW's trains schedule endpoint returns 502 often.** The loader retries every
  15 minutes and the matcher keeps the stored version. That ERROR line is real,
  but it is not an outage.
- **Trains never sends `stop_sequence`, `start_date`, `direction_id` or a vehicle
  id.** The other four feeds do send `start_date` and `stop_sequence`.
- **Schedule relationships stay raw integers.** `REPLACEMENT` (5) is ~15–18 % of
  train updates. `status: "cancelled"` means `trip_rel == 3`.
- **The dev database's `public` schema has real partitions and data.** Tests that
  inspect the catalogue must scope to their own schema via
  `'observations'::regclass`.
- **Load tests go in a throwaway database** (`headway_load` was created, then
  dropped), never the dev one, with `HTTP_RATE_LIMIT_RPS` raised.
- **zsh doesn't word-split `$var`** — use `${=var}` or arrays — and `?` inside
  `${var/pat/rep}` is a glob. Both caused failed commands this session.
- **The API key is real and in `.env`** (gitignored, mode 0600). It was pasted into
  an early session transcript; worth rotating if that transcript is shared.
- `hey` was installed to `/tmp/claude-501/hey` and won't survive a reboot. To
  reinstall, from outside the repo: `GOBIN=/tmp/x go install github.com/rakyll/hey@v0.1.4`.

## Decisions the user made (all in §15)

`not_found` error code, `summary.early`, `limit` caps, `reasons` beside the
envelope; history percentiles approximate when combining rows (unfiltered stop
history is now exact through the `~all` rows); `status: "cancelled"`; rollups by
scheduled hour; buses off; the rate limiter and `PPROF_ADDR`.

**Ask before**: any other change to §7, a new dependency, a new goroutine, channel
or lock, or editing an applied migration.
