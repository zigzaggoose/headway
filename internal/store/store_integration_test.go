//go:build integration

package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/transitlateagain"
	"github.com/zigzaggoose/transitlateagain/internal/config"
)

// Integration tests run against a real Postgres, not a mock: mocking would not
// exercise declarative partitioning, ON CONFLICT, or the DDL itself, which is
// most of what this package is (§15).
//
// Each test gets its own schema so they can run in parallel, and drops it
// afterwards.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST is not set")
	}

	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), rand.IntN(1000))
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := Open(ctx, config.Secret(dsn+sep+"search_path="+schema), 4, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func embedded(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(transitlateagain.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	return sub
}

func TestMigrate_AppliesEveryMigrationThenIsANoOp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	ran, err := s.Migrate(ctx, embedded(t))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(ran) < 4 {
		t.Fatalf("applied %v, want every migration", ran)
	}

	// Startup applies migrations on every boot, so the second run must be a
	// no-op rather than an error (Definition of Done #8).
	again, err := s.Migrate(ctx, embedded(t))
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second run applied %v, want nothing", again)
	}
}

func TestMigrate_CreatesTheSchemaTheDesignDescribes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, table := range []string{
		"schedule_versions", "routes", "stops", "trips", "stop_times",
		"calendar", "calendar_dates",
		"observations", "observations_default",
		"otp_stop_hourly", "otp_route_hourly", "rollup_state",
	} {
		var exists bool
		err := s.pool.QueryRow(ctx,
			`SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL`, table).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}

	// observations must be RANGE partitioned on service_date: retention is a
	// DROP TABLE, and that only works if the partitioning is real (§9.3).
	var strategy, key string
	err := s.pool.QueryRow(ctx, `
		SELECT p.partstrat, pg_get_partkeydef(c.oid)
		FROM pg_partitioned_table p
		JOIN pg_class c ON c.oid = p.partrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = 'observations' AND n.nspname = current_schema()`).Scan(&strategy, &key)
	if err != nil {
		t.Fatalf("observations is not a partitioned table: %v", err)
	}
	if strategy != "r" {
		t.Errorf("partition strategy = %q, want range", strategy)
	}
	if key != "RANGE (service_date)" {
		t.Errorf("partition key = %q", key)
	}

	// The watermark row must exist, or retention has nothing to check itself
	// against and could drop a partition that was never rolled up.
	var watermark time.Time
	if err := s.pool.QueryRow(ctx, `SELECT watermark FROM rollup_state WHERE name = 'hourly'`).Scan(&watermark); err != nil {
		t.Errorf("rollup_state seed row missing: %v", err)
	}
}

// §6.2: exactly one active schedule version per feed, enforced by the database
// rather than by application code.
func TestMigrate_OneActiveScheduleVersionPerFeed_IsEnforced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	insert := `INSERT INTO schedule_versions (feed_id, sha256, active) VALUES ('sydneytrains', $1, true)`
	if _, err := s.pool.Exec(ctx, insert, []byte("hash-one")); err != nil {
		t.Fatalf("first active version rejected: %v", err)
	}
	if _, err := s.pool.Exec(ctx, insert, []byte("hash-two")); err == nil {
		t.Error("a second active version for the same feed was accepted")
	}

	// The same content twice is also a mistake: the hash is the natural key.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO schedule_versions (feed_id, sha256, active) VALUES ('sydneytrains', $1, false)`,
		[]byte("hash-one")); err == nil {
		t.Error("a duplicate content hash for the same feed was accepted")
	}
}

// §6.2: the observations primary key is the idempotency key. Replaying the
// same feed response must not double-count.
func TestMigrate_ObservationsKey_MakesReplaysIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const insert = `
		INSERT INTO observations
		    (service_date, feed_id, trip_id, stop_sequence, feed_ts, stop_id, trip_rel, stop_time_rel, matched)
		VALUES ('2026-09-21', 'sydneytrains', 'trip-1', 11, '2026-09-21T01:04:03Z', '2000341', 0, 0, true)
		ON CONFLICT DO NOTHING`

	for range 3 {
		if _, err := s.pool.Exec(ctx, insert); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("three identical writes produced %d rows, want 1", n)
	}

	// With no daily partition yet, the row lands in the default partition.
	// That is by design — a write never fails — and a non-empty default
	// partition is an alert, not a normal condition (§9.3 case 4).
	var inDefault int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM observations_default`).Scan(&inDefault); err != nil {
		t.Fatalf("count default: %v", err)
	}
	if inDefault != 1 {
		t.Errorf("default partition holds %d rows, want the 1 with no matching partition", inDefault)
	}
}

// §14: never edit an applied migration. The checksum turns that convention
// into a failed startup rather than a silent divergence between what the
// schema is and what the files say it is.
func TestMigrate_EditedAfterApplying_IsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	before := fstest.MapFS{"0001_thing.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE thing (id int)`)}}
	if _, err := s.Migrate(ctx, before); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	after := fstest.MapFS{"0001_thing.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE thing (id bigint)`)}}
	_, err := s.Migrate(ctx, after)
	if err == nil {
		t.Fatal("an edited migration was accepted")
	}
	if !strings.Contains(err.Error(), "has changed since it was applied") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A migration numbered below one that has already run would execute against a
// schema its author never saw.
func TestMigrate_BackdatedMigration_IsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Migrate(ctx, fstest.MapFS{
		"0002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE second (id int)`)},
	}); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	_, err := s.Migrate(ctx, fstest.MapFS{
		"0001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE first (id int)`)},
		"0002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE second (id int)`)},
	})
	if err == nil {
		t.Fatal("a backdated migration was accepted")
	}
	if !strings.Contains(err.Error(), "already run") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A failed migration must leave nothing behind: half-applied DDL recorded as
// applied is unrecoverable without hand-editing the database.
func TestMigrate_FailedMigration_IsNotRecorded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	broken := fstest.MapFS{
		"0001_ok.sql":     &fstest.MapFile{Data: []byte(`CREATE TABLE ok (id int)`)},
		"0002_broken.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE fine (id int); CREATE TABLE ok (id int)`)},
	}
	if _, err := s.Migrate(ctx, broken); err == nil {
		t.Fatal("a failing migration was accepted")
	}

	var applied int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count: %v", err)
	}
	if applied != 1 {
		t.Errorf("%d migrations recorded, want only the one that succeeded", applied)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass(current_schema() || '.fine') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check: %v", err)
	}
	if exists {
		t.Error("the first statement of the failed migration was committed")
	}
}

func TestOpen_PingsAndReportsAReachableDatabase(t *testing.T) {
	s := testStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}
}
