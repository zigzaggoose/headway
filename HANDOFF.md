# HANDOFF.md

Session state as of **2026-09-24, evening**, commit `5e9d4c1` (pushed, CI green,
running on the VM). Rewrite this file when it stops being true. Permanent rules
live in `CLAUDE.md`; design in `PROJECT.md` (§12 milestones, §15 decisions,
§16 open questions). Every deployment step ever run is in
`deploy/vm-bootstrap.md`.

## Start here

```sh
open -a Docker && docker start headway-dev-pg        # the daemon does not survive a reboot
make lint && make test && make test-integration && make cross
curl -s https://api.transitlateagain.dev/readyz        # production, from outside
ssh root@119.42.55.16 'curl -s localhost:8081/v1/admin/stats'   # production, from inside
```

**Check exit codes, never piped output** (`657d87b` shipped a failing test that
way). zsh does not word-split `$var`; a heredoc whose body contains its own
terminator line ends early; BSD `sed` has no `\b` (use `perl -pi -e`).

## Where the project is

| Stage | State |
|---|---|
| 1 — MVP | **Complete.** Live since 2026-09-24 11:53. |
| 2 — Matching, history, CI | **Complete.** |
| 3 — All modes, scale, load test | Complete except **storage over a full day** (due 2026-09-26, below). |
| 4 — Observability, front end | **Complete.** |

The project was renamed from **Headway** to **Transit Late Again** on
2026-09-24 (`b7b1059`). The old name survives on purpose in: the Postgres role
and database (`headway`), the Compose volume (`headway_pgdata`), the VM's
hostname, and every §15 row written before the rename.

## What runs where

| | Address | Notes |
|---|---|---|
| Site | https://transitlateagain.dev | `web/`, Next.js 16 static export on **Cloudflare Pages** (project `transitlateagain`: root `web`, `npm run build`, output `out`, `NODE_VERSION=22`). Rebuilt on every push to `main`. |
| API | https://api.transitlateagain.dev | The VM behind Cloudflare in Full (strict). CORS allows exactly `https://transitlateagain.dev`. 10 req/s per visitor, keyed on `CF-Connecting-IP`. |
| VM | `ssh root@119.42.55.16` | BinaryLane Standard 1 GB, Sydney, Ubuntu 24.04, no backups. Key-only SSH (`~/.ssh/id_ed25519`); the emailed root password works only in BinaryLane's web console. Only SSH is open to the world; 443 is open to Cloudflare's ranges only. |
| Admin, `/metrics` | `localhost:8081` on the VM | Never public. Alloy scrapes it over the Compose network. |
| Metrics | Grafana Cloud stack, region `prod-au-southeast-1` | Dashboard imported from `deploy/grafana-dashboard.json`; alert `TransitLateAgainIngestStopped` emails the account owner; runbook `docs/runbook.md`. |
| DNS, TLS | Cloudflare (nameservers set at Namecheap) | Records: `api` A → 119.42.55.16 (proxied); the root is a Pages custom domain; MX/TXT are Namecheap email forwarding. Origin cert in `/opt/transitlateagain-tls` on the VM, expires 2041-09-20; its key never left the VM. |

**Every Compose command on the VM needs both files**, from `/opt/transitlateagain`:

```sh
C="docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml"
git pull && $C up -d --build        # deploy
```

Memory with all of it running: service ~230 MB, Postgres ~142 MB, Alloy
~79 MB, ~230 MB available, 2 GB swap barely used.

## Secrets: where they are, never in git

| Secret | Local `.env` (gitignored, 0600) | VM `/opt/transitlateagain/.env` (0600) |
|---|---|---|
| `TFNSW_API_KEY` | yes | yes |
| `POSTGRES_PASSWORD` | — (dev DB is `headway`/`headway`) | yes, generated on the VM |
| `GRAFANA_REMOTE_WRITE_URL`, `GRAFANA_USERNAME`, `GRAFANA_TOKEN` | yes, since 2026-09-24 | yes, read by Alloy |
| Origin TLS key | — | `/opt/transitlateagain-tls/origin.key`, owner 65532, 0400 |

The Grafana token belongs to the access policy `transitlateagain-alloy-write`,
scope **metrics:write** only, so it cannot query (and a read-only token answers
remote write with `401 invalid scope requested`). Test it without printing it:

```sh
set -a; . ./.env; set +a
curl -s -o /dev/null -w '%{http_code}\n' -u "$GRAFANA_USERNAME:$GRAFANA_TOKEN" -X POST "$GRAFANA_REMOTE_WRITE_URL"   # 400 = authenticated
```

**Pasted into session transcripts, so rotate if one is ever shared:** the TfNSW
API key (an early session) and the Grafana write token (this one). Rotating
either means editing both `.env` files, then `$C up -d` on the VM.

## Due next

1. **Storage over a full day, the last unticked box in the plan.** The first
   complete service day is `observations_2026_09_25`: complete early on
   2026-09-26, its last hour rolled up ~3 h later, so measure on the
   **afternoon of 2026-09-26** with the SQL at the end of `docs/storage.md`
   (`ssh root@119.42.55.16`, then `cd /opt/transitlateagain && $C exec postgres psql -U headway`).
   Fill in `docs/storage.md` and the two "baseline TBD" rows of §13, and tick
   the Stage 3 box.
2. **§13 freshness p50** is unmeasured, and the p95 (17.7 s) is a six-minute
   first window. The dashboard's freshness panel over a full day gives both;
   record them with the date.
3. **The `DOCKER-USER` allowlist** in `deploy/firewall.sh` is Cloudflare's IPv4
   ranges as of 2026-09-24. If Cloudflare changes them
   (https://www.cloudflare.com/ips-v4), update the list and rerun the script.

## Found and fixed this session

- **§9.1 case 18:** the producers keep finished trips and served stops, so
  `/now` called 21 % of trips active with a next stop hours past. The cache
  drops a stop 5 minutes after its prediction, against the feed timestamp.
- **Schedule refreshes failed after the first load:** TfNSW answers an
  unchanged `If-Modified-Since` with 502, not 304. Downloads are unconditional.
- **Grafana 12.2+ calls classic dashboard JSON "old format"**; the dashboard
  file is a V1 resource (`dashboard.grafana.app/v1`).
- **Adding a site to Cloudflare imports the registrar's parking records**; the
  first symptom was a 522 with nothing reaching the VM.

## Things that are true and easy to forget

- **Trains never send `stop_sequence`, `start_date`, `direction_id` or a vehicle
  id**, and 275 of 354 train updates carry delays only. So trains match by
  stop name (order 2) and the other feeds by sequence (order 1).
- **Schedule relationships stay raw integers.** `REPLACEMENT` (5) is ~15–18 % of
  train updates.
- **Parramatta light rail published an empty feed** (a 15-byte header) on the
  morning of 2026-09-24 and was back by ~14:30. Nothing on our side.
- **"Empty Train" is a real TfNSW headsign** for out-of-service runs.
- **The dev database's `public` schema has real data.** Catalogue tests scope
  to their own schema; load tests go in a throwaway database.
- **`web/AGENTS.md` and `web/CLAUDE.md` are generated by `next dev`** and say to
  read `web/node_modules/next/dist/docs/` before writing Next.js code: version
  16 differs from older training data.
- **Nothing is left running on the laptop** except the dev Postgres container.
