# Quota budget

TfNSW's default plan allows 60,000 requests a day and 5 a second, per account
(`PROJECT.md` §16 q1). Transit Late Again stops itself at `FEED_DAILY_BUDGET` (55,000) and
rate-limits at `FEED_RATE_LIMIT_RPS` (4), leaving headroom for `curl` by hand.

## Enabled set, 2026-09-23

Realtime feeds poll every `FEED_POLL_INTERVAL` (15 s): 4 a minute, 5,760 a day
each. A schedule bundle is downloaded once a day, plus a retry every 15 minutes
while the download fails.

| Feed | Realtime / day | Schedule / day | Bundle | Realtime response |
|---|---:|---:|---:|---:|
| `sydneytrains` | 5,760 | 1 (up to 96 on a failing day) | 11.3 MB | 150–230 kB |
| `metro` | 5,760 | 1 | 1.3 MB | 32 kB |
| `sydneyferries` | 5,760 | 1 | 0.9 MB | 13 kB |
| `lightrail-cbdandsoutheast` | 5,760 | 1 | 1.1 MB | 41 kB |
| `lightrail-parramatta` | 5,760 | 1 | 0.2 MB | 14 kB |
| **Total** | **28,800** | **5 (worst case 480)** | | |

**28,805 requests a day in the normal case, 29,280 in the worst** — 52–53 % of
`FEED_DAILY_BUDGET`. Measured: 45 requests in a two-minute run with all five
feeds and their first schedule downloads (2026-09-23), which is the rate above.

The worst case is not hypothetical: the Sydney Trains schedule endpoint answered
502 for most of the evening of 2026-09-23.

## Not enabled: buses

`buses` is in `config/feeds.json` with `"enabled": false`, by the user's decision
(`PROJECT.md` §15, 2026-09-23). It would add 5,760 requests a day (34,565 in
total, still inside the budget); quota is not why it is off. Measured at 23:23
on 2026-09-23: a 98.5 MB bundle holding 3,565,936 stop times, and 20,576 updates
per realtime poll against 2,419 for trains. That is roughly 2.3 GB of raw rows a
day and ~330 MB of timetable in memory, which the 1 GB / 20 GB VM cannot hold.

## Rate

Four realtime polls a minute across five feeds is one request every three
seconds on average; the limiter's 4/s only matters in the burst at startup,
when every poller and the schedule loader start together.
