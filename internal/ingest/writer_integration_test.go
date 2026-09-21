//go:build integration

package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/headway"
	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/gtfsrt"
	"github.com/zigzaggoose/headway/internal/store"
)

// testPool gives each test its own schema with every migration applied, so the
// writer is exercised against the real partitioned table and the real key.
func testPool(t *testing.T) *pgxpool.Pool {
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
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := store.Open(ctx, config.Secret(dsn+sep+"search_path="+schema), 4, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)

	sub, err := fs.Sub(headway.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if _, err := s.Migrate(ctx, sub); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s.Pool()
}

func rows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func sample(trip, stop string, delay int32, ts time.Time) Observation {
	d := delay
	dir := int16(0)
	return Observation{
		ServiceDate:     time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		FeedID:          "sydneytrains",
		TripID:          trip,
		StopID:          stop,
		FeedTS:          ts,
		RouteID:         "T1-EXAMPLE",
		DirectionID:     &dir,
		ArrivalDelayS:   &d,
		DepartureDelayS: &d,
		ObservedDelayS:  &d,
		TripRel:         0,
		StopTimeRel:     0,
		Matched:         true,
	}
}

func newTestPipeline(t *testing.T, pool *pgxpool.Pool, cfg PipelineConfig) *Pipeline {
	t.Helper()
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 1024
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 50 * time.Millisecond
	}
	if cfg.FilterMaxEntries == 0 {
		cfg.FilterMaxEntries = 10000
	}
	p := NewPipeline(pool, cfg, slog.New(slog.DiscardHandler))
	p.Start()
	return p
}

func TestWriter_WritesABatchAndIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	batch := make([]Observation, 0, 200)
	for i := range 200 {
		batch = append(batch, sample("trip-1", fmt.Sprintf("stop-%d", i), int32(i), ts))
	}

	w := NewWriter(pool, nil, WriterConfig{BatchSize: 500, FlushInterval: time.Second}, slog.New(slog.DiscardHandler))
	w.writeBatch(context.Background(), batch, "test")
	if w.Stats().Written != 200 {
		t.Fatalf("wrote %d rows, want 200 (failed=%d)", w.Stats().Written, w.Stats().Failed)
	}

	// Replaying the same feed response must write nothing the second time.
	w.writeBatch(context.Background(), batch, "replay")
	if got := rows(t, pool); got != 200 {
		t.Errorf("%d rows after a replay, want 200", got)
	}
	if w.Stats().Conflicts != 200 {
		t.Errorf("conflicts = %d, want 200", w.Stats().Conflicts)
	}
}

// The columns that are genuinely unknown must be NULL, not an empty string:
// §6.2 says route_id is NULL when unmatched, and every query would otherwise
// have to special-case "".
func TestWriter_UnknownValues_AreStoredAsNull(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	unmatched := Observation{
		ServiceDate: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		FeedID:      "sydneytrains",
		TripID:      "trip-unknown",
		StopID:      "stop-1",
		FeedTS:      ts,
		TripRel:     5, // REPLACEMENT, which the specification removed
		StopTimeRel: 0,
		Matched:     false,
	}

	w := NewWriter(pool, nil, WriterConfig{}, slog.New(slog.DiscardHandler))
	w.writeBatch(context.Background(), []Observation{unmatched}, "test")
	if w.Stats().Written != 1 {
		t.Fatalf("wrote %d rows (failed=%d)", w.Stats().Written, w.Stats().Failed)
	}

	var routeID, vehicleID *string
	var stopSeq, observed *int32
	var dir *int16
	var tripRel int16
	var matched bool
	err := pool.QueryRow(context.Background(), `
		SELECT route_id, vehicle_id, stop_sequence, observed_delay_s, direction_id, trip_rel, matched
		FROM observations WHERE trip_id = 'trip-unknown'`).
		Scan(&routeID, &vehicleID, &stopSeq, &observed, &dir, &tripRel, &matched)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for name, v := range map[string]any{
		"route_id": routeID, "vehicle_id": vehicleID, "stop_sequence": stopSeq,
		"observed_delay_s": observed, "direction_id": dir,
	} {
		if !isNil(v) {
			t.Errorf("%s = %v, want NULL", name, v)
		}
	}
	// The relationship the specification removed must survive as its integer.
	if tripRel != 5 {
		t.Errorf("trip_rel = %d, want 5 preserved verbatim", tripRel)
	}
	if matched {
		t.Error("matched = true for an unmatched observation")
	}
}

func isNil(v any) bool {
	switch p := v.(type) {
	case *string:
		return p == nil
	case *int32:
		return p == nil
	case *int16:
		return p == nil
	}
	return false
}

// §9.1: a trip visiting one stop twice within a single feed timestamp collides
// on the key and the second visit is discarded. This is the known cost of
// Open Question 11's answer, and it is asserted rather than left to be
// discovered.
func TestWriter_LoopService_LosesTheSecondVisitInOneFeedTimestamp(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	first := sample("loop-trip", "circular-quay", 60, ts)
	seq1, seq2 := int32(3), int32(11)
	first.StopSequence = &seq1
	second := sample("loop-trip", "circular-quay", 300, ts)
	second.StopSequence = &seq2

	w := NewWriter(pool, nil, WriterConfig{}, slog.New(slog.DiscardHandler))
	w.writeBatch(context.Background(), []Observation{first, second}, "test")

	if got := rows(t, pool); got != 1 {
		t.Errorf("%d rows for a loop service, want 1 (the documented collision)", got)
	}
	// A later feed timestamp stores the second visit fine: the loss is only
	// within one timestamp.
	third := sample("loop-trip", "circular-quay", 300, ts.Add(15*time.Second))
	third.StopSequence = &seq2
	w.writeBatch(context.Background(), []Observation{third}, "test")
	if got := rows(t, pool); got != 2 {
		t.Errorf("%d rows after a later timestamp, want 2", got)
	}
}

// The failure §9.4 refuses to allow: an accepted observation that never
// reaches the database because the process was shutting down.
func TestShutdownFlushesPendingBatch(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	// A batch size far above what is submitted, and a flush interval far
	// longer than the test: nothing can reach the database except the flush
	// that Close forces.
	p := newTestPipeline(t, pool, PipelineConfig{
		QueueSize:     1024,
		BatchSize:     10000,
		FlushInterval: time.Hour,
	})

	const n = 500
	for i := range n {
		if !p.Submit(sample("trip-1", fmt.Sprintf("stop-%d", i), int32(i), ts)) {
			t.Fatalf("observation %d was not queued", i)
		}
	}
	if got := rows(t, pool); got != 0 {
		t.Fatalf("%d rows before shutdown; the test is not exercising the flush", got)
	}

	if err := p.Close(10 * time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := rows(t, pool); got != n {
		t.Errorf("%d rows after shutdown, want every accepted observation (%d)", got, n)
	}
}

func TestPipeline_FlushesOnBatchSizeAndOnInterval(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	p := newTestPipeline(t, pool, PipelineConfig{BatchSize: 50, FlushInterval: 50 * time.Millisecond})
	t.Cleanup(func() { _ = p.Close(5 * time.Second) })

	for i := range 50 {
		p.Submit(sample("trip-size", fmt.Sprintf("stop-%d", i), int32(i), ts))
	}
	waitFor(t, func() bool { return rows(t, pool) == 50 }, "a full batch to flush on size")

	// One more observation cannot fill a batch, so only the interval can write it.
	p.Submit(sample("trip-interval", "stop-1", 1, ts))
	waitFor(t, func() bool { return rows(t, pool) == 51 }, "a partial batch to flush on the interval")
}

// §9.4: the queue drops rather than blocks, and a dropped observation must be
// re-admitted next time rather than suppressed as already written.
func TestPipeline_FullQueue_DropsAndForgets(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	// A queue of one and no writer running: the second submit has nowhere to go.
	p := NewPipeline(pool, PipelineConfig{
		QueueSize: 1, BatchSize: 1000, FlushInterval: time.Hour, FilterMaxEntries: 1000,
	}, slog.New(slog.DiscardHandler))

	if !p.Submit(sample("t", "s1", 10, ts)) {
		t.Fatal("the first observation was not queued")
	}
	dropped := sample("t", "s2", 20, ts)
	if p.Submit(dropped) {
		t.Fatal("the queue accepted more than its capacity")
	}
	if got := p.Stats().Dropped; got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}

	// Start the writer, drain, then re-submit the dropped observation: it must
	// be admitted, not suppressed as unchanged.
	p.Start()
	waitFor(t, func() bool { return rows(t, pool) >= 0 && len(p.ch) == 0 }, "the queue to drain")
	if !p.Submit(dropped) {
		t.Error("a dropped observation was suppressed on its next appearance; the value would be lost for good")
	}
	_ = p.Close(5 * time.Second)
}

func TestPipeline_Close_IsSafeToCallTwice(t *testing.T) {
	pool := testPool(t)
	p := newTestPipeline(t, pool, PipelineConfig{})
	if err := p.Close(5 * time.Second); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := p.Close(5 * time.Second); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// A batch that fails must not stall the pipeline: the writer logs, counts, and
// carries on, because a stalled process cannot serve reads either (§9.3 c8).
func TestWriter_FailedBatch_IsCountedAndDropped(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	if _, err := pool.Exec(context.Background(), `DROP TABLE observations CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	w := NewWriter(pool, nil, WriterConfig{}, slog.New(slog.DiscardHandler))
	w.writeBatch(context.Background(), []Observation{sample("t", "s", 1, ts)}, "test")

	if w.Stats().Failed != 1 {
		t.Errorf("failed = %d, want 1", w.Stats().Failed)
	}
	if w.Stats().Written != 0 {
		t.Errorf("written = %d, want 0", w.Stats().Written)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Replaying a recorded feed response writes it once, at the scale the service
// actually runs at: 3,939 updates from a real capture, submitted twice through
// cold pipelines as a restart would. §9.3 case 2 says the burst after a
// restart is correct and idempotent; this asserts it on real data rather than
// on a handful of synthetic rows.
func TestPipeline_ReplayingARecordedResponse_WritesItOnce(t *testing.T) {
	pool := testPool(t)
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sydneytrains_tripupdate_0001.pb"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoded, err := gtfsrt.Decode("sydneytrains", body, time.Now())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Updates) < 1000 {
		t.Fatalf("fixture has %d updates; this test is about scale", len(decoded.Updates))
	}

	// Two independent pipelines, each with an empty filter, exactly as two
	// runs of the process would have.
	//
	// The queue is the production default. One poll submits every update in a
	// burst, so a queue smaller than a poll's worth drops the difference by
	// design — INGEST_QUEUE_SIZE has to exceed the updates in one poll, and
	// this fixture is what "one poll" actually means: 3,939 of them.
	submitAll := func() *Pipeline {
		p := newTestPipeline(t, pool, PipelineConfig{
			QueueSize: 8192, BatchSize: 500, FlushInterval: 20 * time.Millisecond,
		})
		for _, u := range decoded.Updates {
			p.Submit(observationFrom(u))
		}
		if err := p.Close(20 * time.Second); err != nil {
			t.Fatalf("close: %v", err)
		}
		if dropped := p.Stats().Dropped; dropped != 0 {
			t.Fatalf("%d observations were dropped by a queue sized for a whole poll", dropped)
		}
		return p
	}

	// Every (trip_id, stop_id) in a single feed message is distinct, so every
	// decoded update must become a row: anything less means the key is
	// colliding on data the feed considers separate.
	first := submitAll()
	afterFirst := rows(t, pool)
	if afterFirst != len(decoded.Updates) {
		t.Fatalf("%d updates produced %d rows; the key is discarding distinct observations",
			len(decoded.Updates), afterFirst)
	}
	if first.Stats().Failed != 0 {
		t.Fatalf("%d rows failed to write", first.Stats().Failed)
	}

	second := submitAll()
	afterSecond := rows(t, pool)

	if afterSecond != afterFirst {
		t.Errorf("replay added %d rows; the natural key is not absorbing it", afterSecond-afterFirst)
	}
	if second.Stats().Conflicts == 0 {
		t.Error("the replay reported no conflicts, so it cannot have written the same rows")
	}
	t.Logf("%d updates -> %d rows; replay wrote 0 new rows and reported %d conflicts",
		len(decoded.Updates), afterFirst, second.Stats().Conflicts)
}

// observationFrom is the unmatched conversion of §9.1 order 4: no schedule is
// loaded, so the delay is whatever the producer supplied. The real conversion
// lives in internal/match from Stage 2.
func observationFrom(u gtfsrt.RawUpdate) Observation {
	o := Observation{
		ServiceDate:     u.HeaderTS.Truncate(24 * time.Hour),
		FeedID:          u.FeedID,
		TripID:          u.TripID,
		StopID:          u.StopID,
		FeedTS:          u.HeaderTS,
		RouteID:         u.RouteID,
		ArrivalDelayS:   u.ArrivalDelay,
		DepartureDelayS: u.DepartureDelay,
		TripRel:         u.TripRel,
		StopTimeRel:     u.StopTimeRel,
		VehicleID:       u.VehicleID,
	}
	switch {
	case u.DepartureDelay != nil:
		o.ObservedDelayS = u.DepartureDelay
	case u.ArrivalDelay != nil:
		o.ObservedDelayS = u.ArrivalDelay
	}
	if u.TripLevel {
		o.StopID = "~trip"
	}
	return o
}
