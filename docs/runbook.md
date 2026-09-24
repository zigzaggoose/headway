# Runbook

What to do when an alert fires. Commands run on the VM (`ssh root@119.42.55.16`)
from `/opt/transitlateagain`, where every Compose command needs both files:

```sh
C="docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml"
```

The dashboard is `deploy/grafana-dashboard.json`, imported into Grafana Cloud.

## TransitLateAgainIngestStopped

**Condition:** no row written for 15 minutes, including when the metric is
missing altogether (the service, Alloy or the whole VM is down):

```promql
(sum(increase(transitlateagain_rows_written_total[15m])) or vector(0)) == 0
```

**Why it matters:** TfNSW does not archive the realtime feed. Every minute of
this is data lost for good.

1. **Is the service up?**
   `$C ps` — all of `postgres`, `transitlateagain` and `alloy` should be `Up`.
   A container restarting in a loop: `$C logs --tail 50 transitlateagain`.
   Anything stopped: `$C up -d`.

2. **Is it the metrics path, not ingest?** If the service is up, ask it:
   `curl -s localhost:8081/v1/admin/stats` — `ingest.written` climbing means
   rows are being written and the fault is Alloy or Grafana. Check Alloy:
   `$C logs --tail 30 alloy` (a 401 means the Grafana token was revoked).

3. **Are the feeds answering?** In the same stats, each `feeds[].feed_age_s`
   should be under a minute. On the dashboard, *Upstream requests by
   outcome*: `quota` means the day's budget is spent (it resets at midnight
   UTC; reduce `FEED_POLL_INTERVAL` or disable a feed), `unauthorized` means
   `TFNSW_API_KEY` in `.env` is wrong or rotated, `error` or `timeout` across
   every feed usually means TfNSW is down — check their status page before
   touching anything.

4. **Is the database writing?** `ingest.failed` climbing means batches are
   failing: `$C logs transitlateagain | grep "batch write failed"`.
   `df -h /` — a full disk stops Postgres. Free space by dropping raw data
   early: `$C run --rm -e RETENTION_DAYS=3 transitlateagain -maintain-once`.
   Rollups are never dropped.

5. **Is the queue full?** `ingest.dropped` climbing with `queue_len` near
   `queue_cap` means the writer cannot keep up; the database is the suspect
   (step 4).

**Test it:** `$C stop transitlateagain`, wait for the alert (15 minutes plus
the evaluation interval), `$C start transitlateagain`, confirm it resolves.
That window is lost data, so do it deliberately and rarely.
