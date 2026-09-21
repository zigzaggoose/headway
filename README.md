# Headway

On-time performance for the Sydney transport network. Headway polls the
Transport for NSW GTFS-realtime feeds, matches every stop-time update against
the published timetable, and stores the delay it observes, so it can answer two
questions: what is happening on this line right now, and how has this stop or
line performed over the last N days.

TfNSW publishes realtime data and timetable data but not on-time performance,
and the realtime feed is not archived anywhere queryable. Headway captures it.

`PROJECT.md` is the design document and the source of truth for this repository.

## Status

Stage 1 in progress: the skeleton, the module, the migrations and the Makefile
are in place. No feed is polled yet. `PROJECT.md` §12 tracks the milestones.

## Running it

```sh
cp .env.example .env     # then put a real TFNSW_API_KEY in it
make up                  # postgres + headway via docker compose
make test                # unit tests
make lint                # gofmt + go vet
```

`make run` runs the service directly against a local Postgres instead. Every
configuration variable is documented in `.env.example` and in `PROJECT.md` §8.

## Stack

Go 1.27, PostgreSQL 18, pgx v5, the standard library's `ServeMux` and
`log/slog`. One process, one database, one VM. `PROJECT.md` §3 says what each
was chosen over and why.
