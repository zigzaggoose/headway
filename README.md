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

docker run -d --name headway-dev-pg -p 5432:5432 \
  -e POSTGRES_USER=headway -e POSTGRES_PASSWORD=headway -e POSTGRES_DB=headway \
  postgres:18-alpine

make migrate             # apply migrations; the service also does this on start
make run                 # poll, decode, serve
```

`make test` runs the unit tests; `make test-integration` also runs the ones
needing a database, and skips them when `DATABASE_URL_TEST` is unset. Both URLs
in `.env.example` already point at the container above. `make lint` is gofmt
and go vet.

`make up` will replace the `docker run` line once `deploy/docker-compose.yml`
exists (Stage 1, §12). Every configuration variable is documented in
`.env.example` and in `PROJECT.md` §8.

## Stack

Go 1.27, PostgreSQL 18, pgx v5, the standard library's `ServeMux` and
`log/slog`. One process, one database, one VM. `PROJECT.md` §3 says what each
was chosen over and why.
