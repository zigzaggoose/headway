# HANDOFF.md

Session state as of **2026-09-24**, commit `2b97bab` (pushed, running on the VM).
Delete or rewrite this file when it stops being true. Permanent rules live in
`CLAUDE.md`; design lives in `PROJECT.md` (§12 milestones, §15 decisions, §16
open questions).

## Start here

```sh
open -a Docker && docker start headway-dev-pg
make lint && make test && make test-integration && make cross
ssh root@119.42.55.16 'curl -s localhost:8081/v1/admin/stats'   # production health
```

**Check exit codes, never piped output** (`657d87b` shipped a failing test that
way). zsh does not word-split `$var`, and a heredoc whose body contains its own
terminator line (`EOF`) ends early — pick a terminator the body cannot contain.

## Where the project is

| Stage | State |
|---|---|
| 1 — MVP | **Complete.** Live at `https://transitlateagain.dev` since 2026-09-24 11:53. |
| 2 — Matching, history, CI | **Complete.** |
| 3 — All modes, scale, load test | Complete except **storage over a full day** (below). |
| 4 — Observability, front end | **Complete.** |

## Production

BinaryLane Standard 1 GB, Sydney, Ubuntu 24.04, no backups. Everything that was
run is in `deploy/vm-bootstrap.md`; update with `git pull` and `docker compose
... up -d --build` from `/opt/transitlateagain`.

- **SSH is key-only** (`~/.ssh/id_ed25519` on the laptop). The emailed root
  password works only in BinaryLane's web console.
- **`/opt/transitlateagain/.env`** (0600): the API key, a generated `POSTGRES_PASSWORD`,
  `RETENTION_DAYS=7`, `DB_MAX_CONNS=5`, `HTTP_RATE_LIMIT_RPS=10`.
- **Admin stats are private**: `ADMIN_ADDR` is published on the host's
  127.0.0.1:8081 only; the public port answers `/v1/admin/stats` with 404.
- **Per-IP connection limits** on 8080 in `DOCKER-USER` (`deploy/firewall.sh`,
  `transitlateagain-firewall.service`, verified after a reboot). ufw governs SSH only —
  Docker's published ports bypass it.
- Measured at first start: ready in under 80 s, 0 dropped, 0 failed,
  `default_partition_rows` 0; 236 MB service + 181 MB Postgres, ~220 MB free.

## Due next

1. **Storage over a full day (last Stage 3 box).** The first complete service
   day is the `observations_2026_09_25` partition. It is complete early on
   2026-09-26 and its last hour rolls up about 3 h later, so measure on the
   afternoon of **2026-09-26** with the SQL at the end of `docs/storage.md`
   (run it through `docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml exec postgres psql -U headway`). Fill in
   `docs/storage.md` and the two "baseline TBD" rows of §13.
2. **Parramatta light rail** (`lightrail-parramatta`) has published an empty
   feed all day on 2026-09-24 — a 15-byte header, no entities (one raw fetch).
   Polls succeed, so nothing is wrong on our side. If it is still empty after a
   few days, record it in §9.1.

## Stage 4: complete (2026-09-24)

- **Site:** https://transitlateagain.dev — `web/`, Next.js 16 static export on
  Cloudflare **Pages** (project `transitlateagain`, root `web`, build
  `npm run build`, output `out`, `NODE_VERSION=22`), rebuilt on every push.
- **API:** https://api.transitlateagain.dev — the VM behind Cloudflare, Full
  (strict), origin cert, only Cloudflare's ranges admitted. CORS allows
  exactly `https://transitlateagain.dev` (`HTTP_CORS_ORIGIN`).
- **Metrics:** `/metrics` on the admin port → Alloy → Grafana Cloud. Dashboard
  from `deploy/grafana-dashboard.json` (a V1 dashboard resource; classic JSON
  is called "old format" by Grafana 12.2+). Alert
  `TransitLateAgainIngestStopped`, runbook in `docs/runbook.md`; delivery
  tested, not fired by stopping ingestion (the user's choice).
- **Every VM Compose command needs both files:**
  `docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml ...`
- **Secrets pasted into this session's transcript:** the Alloy write token
  (the read-only one is deleted). Rotate if the transcript is shared.
- `/v1/admin/stats` and `/metrics`: `ssh root@119.42.55.16 curl -s localhost:8081/...`

## Found and fixed this session

- **Schedule refreshes always failed after the first load** (`868941c`): TfNSW
  answers an unchanged `If-Modified-Since` with 502, not 304. Downloads are
  unconditional now; the content hash makes an unchanged bundle a no-op.

- **§9.1 case 18**: the producers keep finished trips and served stops, and
  `/now` reported 21 % ghost trips, all "on time". The cache now drops a stop 5
  minutes past its prediction (against the feed timestamp). After the fix, no
  trip's next stop was more than 10 minutes past. History was never affected.

## Things that are true and easy to forget

- **TfNSW answers an unchanged `If-Modified-Since` with 502, not 304.** That was
  the "trains schedule endpoint returns 502 often" of 2026-09-23; schedule
  downloads have been unconditional since 2026-09-24 (§15).
- **Trains never sends `stop_sequence`, `start_date`, `direction_id` or a vehicle
  id.** 275 of 354 train trip updates carry delays only, no absolute times.
- **Schedule relationships stay raw integers.** `REPLACEMENT` (5) is ~15–18 % of
  train updates.
- **The dev database's `public` schema has real data.** Catalogue tests scope to
  their own schema; load tests go in a throwaway database.
- **The API key was pasted into an early session transcript**; worth rotating if
  that transcript is shared. Rotating means editing `/opt/transitlateagain/.env` too.
