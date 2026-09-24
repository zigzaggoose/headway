//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestTripStops_NewestObservationPerStop_OfThatTripOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	t0 := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		date          time.Time
		feed, trip    string
		stop          string
		at            time.Duration
		delay         *int32
		tripRel, sRel int32
	}{
		{day, "trains", "T-1", "A", 0, ptr32(60), 0, 0},
		{day, "trains", "T-1", "A", time.Minute, ptr32(400), 0, 0}, // the newer word on A
		{day, "trains", "T-1", "B", 0, nil, 0, 1},                  // skipped, no delay
		{day, "trains", "T-1", "E", 0, ptr32(7), 0, 0},
		{day, "trains", "T-2", "A", 2 * time.Minute, ptr32(-500), 0, 0},
		{day.AddDate(0, 0, -1), "trains", "T-1", "C", 0, ptr32(1), 0, 0},
		{day, "metro", "T-1", "D", 0, ptr32(2), 0, 0},
	} {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO observations (service_date, feed_id, trip_id, stop_id, feed_ts, observed_delay_s, trip_rel, stop_time_rel, matched)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)`,
			r.date, r.feed, r.trip, r.stop, t0.Add(r.at), r.delay, r.tripRel, r.sRel); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := s.TripStops(ctx, day, "trains", "T-1")
	if err != nil {
		t.Fatalf("trip stops: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d stops %+v, want A, B and E only: not T-2, not the day before, not another feed", len(got), got)
	}
	if a := got["A"]; a.DelayS == nil || *a.DelayS != 400 {
		t.Errorf("A = %+v, want the newer delay 400, not E's 7 through a shared pointer", a)
	}
	if b := got["B"]; b.DelayS != nil || b.StopTimeRel != 1 {
		t.Errorf("B = %+v, want no delay and stop_time_rel 1; a nil must not inherit A's value", b)
	}
}

func ptr32(v int32) *int32 { return &v }
