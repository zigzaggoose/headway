//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestHistory_ReadsRollupsForTheSubjectAndFilters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := func(q HistoryQuery) History {
		t.Helper()
		h, err := s.History(ctx, q)
		if err != nil {
			t.Fatalf("history %+v: %v", q, err)
		}
		return h
	}
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	week := HistoryQuery{StopID: "S1", From: day, To: day.AddDate(0, 0, 1)}

	if h := q(week); h.ScheduleLoaded || h.Known {
		t.Fatalf("with no schedule: %+v, want neither loaded nor known", h)
	}

	for _, sql := range []string{
		`INSERT INTO schedule_versions (id, feed_id, sha256, active) VALUES (1, 'f', 'a', true), (2, 'f', 'b', false)`,
		`INSERT INTO stops (version_id, stop_id, name) VALUES (1, 'S1', 'Central'), (2, 'OLD', 'Gone')`,
		`INSERT INTO routes (version_id, route_id, short_name, route_type) VALUES (1, 'R1', 'T8', 2)`,
	} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, r := range []struct {
		hour      int
		stop, rte string
		dir       int16
	}{{9, "S1", "R1", 0}, {8, "S1", "R1", 1}, {8, "S1", "R2", 0}, {8, "S2", "R1", 0}, {30, "S1", "R1", 0},
		{8, "S1", AllRoutes, -1}, {9, "S1", AllRoutes, -1}} {
		b := day.Add(time.Duration(r.hour) * time.Hour)
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO otp_stop_hourly (bucket_start, service_date, stop_id, route_id, direction_id, n_obs, n_early, n_on_time, n_late, n_very_late, n_skipped, n_cancelled, delay_p50_s)
			VALUES ($1, $2, $3, $4, $5, 3, 0, 3, 0, 0, 0, 0, 12)`, b, day, r.stop, r.rte, r.dir); err != nil {
			t.Fatalf("insert stop row: %v", err)
		}
		if r.stop == "S1" && r.rte != AllRoutes {
			if _, err := s.pool.Exec(ctx, `
				INSERT INTO otp_route_hourly (bucket_start, service_date, route_id, direction_id, n_obs, n_early, n_on_time, n_late, n_very_late, n_skipped, n_cancelled)
				VALUES ($1, $2, $3, $4, 3, 0, 3, 0, 0, 0, 0) ON CONFLICT DO NOTHING`, b, day, r.rte, r.dir); err != nil {
				t.Fatalf("insert route row: %v", err)
			}
		}
	}

	h := q(week)
	if !h.ScheduleLoaded || !h.Known || h.Name != "Central" {
		t.Fatalf("stop S1: %+v", h)
	}
	// Unfiltered, a stop's history is its all-routes rows: one an hour.
	if len(h.Rows) != 2 || !h.Rows[0].BucketStart.Equal(day.Add(8*time.Hour)) || !h.Rows[1].BucketStart.Equal(day.Add(9*time.Hour)) {
		t.Errorf("stop rows = %+v; want the two all-routes rows in range, oldest first", h.Rows)
	}
	onlyDir := week
	zero := int16(0)
	onlyDir.Direction = &zero
	if n := len(q(onlyDir).Rows); n != 2 {
		t.Errorf("direction filter alone: %d rows, want the two per-route rows with direction 0 and never the all-routes rows", n)
	}
	if h.Rows[0].P50 == nil || *h.Rows[0].P50 != 12 || h.Rows[0].NOnTime != 3 {
		t.Errorf("row values = %+v", h.Rows[0])
	}

	withRoute := week
	withRoute.RouteID = "R1"
	if n := len(q(withRoute).Rows); n != 2 {
		t.Errorf("route filter: %d rows, want 2", n)
	}
	withDir := withRoute
	one := int16(1)
	withDir.Direction = &one
	if n := len(q(withDir).Rows); n != 1 {
		t.Errorf("route and direction filter: %d rows, want 1", n)
	}

	line := q(HistoryQuery{RouteID: "R1", From: day, To: day.AddDate(0, 0, 1)})
	if !line.Known || line.Name != "T8" || len(line.Rows) != 2 {
		t.Errorf("route R1: known %v name %q rows %d; want the short name and two rows", line.Known, line.Name, len(line.Rows))
	}

	if h := q(HistoryQuery{StopID: "OLD", From: day, To: day.AddDate(0, 0, 1)}); h.Known {
		t.Error("a stop only in an inactive version was reported known")
	}
}

func TestMaintenance_ReportsPartitionsDefaultRowsAndWatermark(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx, embedded(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	m, err := s.Maintenance(ctx)
	if err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if m.OldestPartition != nil || m.Partitions != 0 || m.DefaultRows != 0 || m.DatabaseBytes == 0 {
		t.Errorf("fresh database: %+v", m)
	}
	for _, sql := range []string{
		`CREATE TABLE observations_2026_09_21 PARTITION OF observations FOR VALUES FROM ('2026-09-21') TO ('2026-09-22')`,
		`CREATE TABLE observations_2026_09_20 PARTITION OF observations FOR VALUES FROM ('2026-09-20') TO ('2026-09-21')`,
		`INSERT INTO observations (service_date, feed_id, trip_id, stop_id, feed_ts, trip_rel, stop_time_rel, matched) VALUES ('2026-10-30', 'f', 't', 's', now(), 0, 0, false)`,
	} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if m, err = s.Maintenance(ctx); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if m.Partitions != 2 || m.OldestPartition == nil || m.OldestPartition.Format(time.DateOnly) != "2026-09-20" || m.DefaultRows != 1 {
		t.Errorf("maintenance = %+v", m)
	}
}
