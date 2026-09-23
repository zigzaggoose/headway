package ingest

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/headway/internal/store"
)

// insertObservations is one statement for a whole batch: the arrays are
// unnested server-side into rows. One statement means the batch succeeds or
// fails whole, with no per-row error handling to get wrong (§10.1), and one
// round trip rather than one per row.
//
// ON CONFLICT DO NOTHING against the natural key is what makes at-least-once
// ingestion safe: replaying the same feed response writes nothing the second
// time (§15).
const insertObservations = `
INSERT INTO observations (
    service_date, feed_id, trip_id, stop_id, feed_ts,
    stop_sequence, route_id, direction_id,
    arrival_delay_s, departure_delay_s, observed_delay_s,
    trip_rel, stop_time_rel, matched, vehicle_id, scheduled_at)
SELECT * FROM unnest(
    $1::date[], $2::text[], $3::text[], $4::text[], $5::timestamptz[],
    $6::integer[], $7::text[], $8::smallint[],
    $9::integer[], $10::integer[], $11::integer[],
    $12::smallint[], $13::smallint[], $14::boolean[], $15::text[],
    $16::timestamptz[])
ON CONFLICT DO NOTHING`

// WriterConfig is the batching policy from §8.
type WriterConfig struct {
	BatchSize     int
	FlushInterval time.Duration
	// WriteTimeout bounds one batch write. Every database call takes a
	// deadline; there are no unbounded calls (§10.1).
	WriteTimeout time.Duration
}

// Writer drains the bounded channel and writes batches to Postgres. Exactly
// one writer runs, which is why there is no write-write contention and no need
// for advisory locks (§15).
type Writer struct {
	pool *pgxpool.Pool
	in   <-chan Observation
	cfg  WriterConfig
	log  *slog.Logger

	// Read by /v1/admin/stats and the metrics collectors while this goroutine
	// is writing, so they are atomic rather than plain counters. Reading them
	// unsynchronised was a data race in the one place an operator looks when
	// something is wrong.
	written   atomic.Uint64
	failed    atomic.Uint64
	batches   atomic.Uint64
	conflicts atomic.Uint64
}

// WriterStats is a snapshot for an operator. Each counter is read atomically,
// though not all four at one instant: nothing depends on them agreeing, and a
// lock here would put the reporting path inside the write path.
type WriterStats struct {
	Written   uint64
	Failed    uint64
	Batches   uint64
	Conflicts uint64
}

// Stats reports what the writer has done. Safe to call while it is running.
func (w *Writer) Stats() WriterStats {
	return WriterStats{
		Written:   w.written.Load(),
		Failed:    w.failed.Load(),
		Batches:   w.batches.Load(),
		Conflicts: w.conflicts.Load(),
	}
}

// NewWriter wires a writer to its input channel.
func NewWriter(pool *pgxpool.Pool, in <-chan Observation, cfg WriterConfig, log *slog.Logger) *Writer {
	// A zero interval panics time.NewTicker, and a zero batch size flushes on
	// every row. config validates both, but a caller building a WriterConfig
	// by hand should get a working writer rather than a panic at Start.
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	return &Writer{pool: pool, in: in, cfg: cfg, log: log.With("component", "ingest")}
}

// Run blocks until in is closed and the final batch is flushed, or ctx is
// done.
//
// The context must NOT be the one that shutdown cancels: the writer has to
// outlive that cancellation in order to flush what it has already accepted.
// Losing an accepted batch is the single failure §9.4 refuses to allow.
func (w *Writer) Run(ctx context.Context) error {
	batch := make([]Observation, 0, w.cfg.BatchSize)
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		w.writeBatch(ctx, batch, reason)
		batch = batch[:0]
	}

	for {
		select {
		case o, ok := <-w.in:
			if !ok {
				// The producers have stopped and the channel is drained. This
				// is step 5 of the shutdown sequence, and the flush below is
				// the whole point of the ordering.
				flush("shutdown")
				w.log.Info("writer stopped", "rows_written", w.written.Load(), "batches", w.batches.Load())
				return nil
			}
			batch = append(batch, o)
			if len(batch) >= w.cfg.BatchSize {
				flush("size")
			}

		case <-ticker.C:
			flush("interval")

		case <-ctx.Done():
			// Only reached if the writer's own deadline expires, which means
			// shutdown has already waited longer than HTTP_SHUTDOWN_GRACE.
			flush("deadline")
			return ctx.Err()
		}
	}
}

// writeBatch writes one batch, retrying only what is worth retrying. A failure
// drops the batch rather than blocking the matcher forever: the feed re-sends
// current state fifteen seconds later, and a stalled process cannot serve
// reads either (§9.3 case 8).
func (w *Writer) writeBatch(ctx context.Context, batch []Observation, reason string) {
	started := time.Now()
	cols := columnsOf(batch)

	var inserted int64
	err := store.Retry(ctx, w.log, "write observations", func(ctx context.Context) error {
		writeCtx, cancel := context.WithTimeout(ctx, w.cfg.WriteTimeout)
		defer cancel()

		tag, err := w.pool.Exec(writeCtx, insertObservations,
			cols.serviceDate, cols.feedID, cols.tripID, cols.stopID, cols.feedTS,
			cols.stopSequence, cols.routeID, cols.directionID,
			cols.arrivalDelay, cols.departureDelay, cols.observedDelay,
			cols.tripRel, cols.stopTimeRel, cols.matched, cols.vehicleID, cols.scheduledAt)
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		w.failed.Add(uint64(len(batch)))
		w.log.Error("batch write failed",
			"rows", len(batch),
			"reason", reason,
			"retryable", store.Retryable(err),
			"err", err.Error())
		return
	}

	w.batches.Add(1)
	w.written.Add(uint64(inserted))
	// The difference is rows the natural key already held: a replay, or two
	// pollers' worth of the same feed timestamp. Expected, and worth counting.
	w.conflicts.Add(uint64(len(batch)) - uint64(inserted))

	w.log.Debug("batch written",
		"rows", len(batch),
		"inserted", inserted,
		"reason", reason,
		"duration_ms", time.Since(started).Milliseconds())
}

// columns is a batch transposed: sixteen arrays rather than n structs, which
// is the shape unnest wants.
type columns struct {
	serviceDate    []time.Time
	feedID         []string
	tripID         []string
	stopID         []string
	feedTS         []time.Time
	stopSequence   []*int32
	routeID        []*string
	directionID    []*int16
	arrivalDelay   []*int32
	departureDelay []*int32
	observedDelay  []*int32
	tripRel        []int16
	stopTimeRel    []int16
	matched        []bool
	vehicleID      []*string
	scheduledAt    []*time.Time
}

func columnsOf(batch []Observation) columns {
	n := len(batch)
	c := columns{
		serviceDate:    make([]time.Time, n),
		feedID:         make([]string, n),
		tripID:         make([]string, n),
		stopID:         make([]string, n),
		feedTS:         make([]time.Time, n),
		stopSequence:   make([]*int32, n),
		routeID:        make([]*string, n),
		directionID:    make([]*int16, n),
		arrivalDelay:   make([]*int32, n),
		departureDelay: make([]*int32, n),
		observedDelay:  make([]*int32, n),
		tripRel:        make([]int16, n),
		stopTimeRel:    make([]int16, n),
		matched:        make([]bool, n),
		vehicleID:      make([]*string, n),
		scheduledAt:    make([]*time.Time, n),
	}
	for i, o := range batch {
		c.serviceDate[i] = o.ServiceDate
		c.feedID[i] = o.FeedID
		c.tripID[i] = o.TripID
		c.stopID[i] = o.StopID
		c.feedTS[i] = o.FeedTS
		c.stopSequence[i] = o.StopSequence
		c.routeID[i] = nilIfEmpty(o.RouteID)
		c.directionID[i] = o.DirectionID
		c.arrivalDelay[i] = o.ArrivalDelayS
		c.departureDelay[i] = o.DepartureDelayS
		c.observedDelay[i] = o.ObservedDelayS
		c.tripRel[i] = int16(o.TripRel)
		c.stopTimeRel[i] = int16(o.StopTimeRel)
		c.matched[i] = o.Matched
		c.vehicleID[i] = nilIfEmpty(o.VehicleID)
		c.scheduledAt[i] = o.ScheduledAt
	}
	return c
}

// nilIfEmpty keeps "unknown" out of the database as NULL rather than as an
// empty string that every query would then have to special-case.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
