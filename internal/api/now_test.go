package api

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/ingest"
	"github.com/zigzaggoose/headway/internal/match"
	"github.com/zigzaggoose/headway/internal/store"
)

// matched is a timetable-matched observation at a known stop_sequence.
func matched(route, trip, stop string, seq int32, delay *int32, rel int32) ingest.Observation {
	o := obs(route, trip, stop, delay, nil, rel)
	o.StopSequence = &seq
	o.Matched = true
	return o
}

func noSchedule() harness {
	return harness{h: NewHandler(Options{
		Cache:     cache.New(time.Hour, func() time.Time { return now }),
		Schedules: func() []*match.Schedule { return nil },
		OnTime:    thresholds,
		Now:       func() time.Time { return now },
		Log:       slog.New(slog.DiscardHandler),
		History:   func(context.Context, store.HistoryQuery) (store.History, error) { return store.History{}, nil },
	})}
}

func TestDataEndpoints_NoSchedule_AreNotReady(t *testing.T) {
	h := noSchedule()
	for _, path := range []string{"/v1/lines", "/v1/lines/R1/now", "/v1/stops/s1/now"} {
		t.Run(path, func(t *testing.T) {
			rec, body := h.get(t, path)
			assertEnvelope(t, rec, body, 503, "not_ready")
		})
	}
}

func TestLines_ListsTheScheduleRoutes(t *testing.T) {
	h := newHarness(t, nil)

	_, body := h.get(t, "/v1/lines")
	lines := body["lines"].([]any)
	if body["count"] != 3.0 || body["schedule_version_id"] != 41.0 || len(lines) != 3 {
		t.Fatalf("got %v", body)
	}
	if first := lines[0].(map[string]any); first["route_id"] != "F1" || first["route_type"] != 4.0 || first["feed_id"] != "trains" || first["long_name"] != "Manly" {
		t.Errorf("first line = %v; want sorted by short name", first)
	}

	_, body = h.get(t, "/v1/lines?mode=2&limit=1")
	if body["count"] != 1.0 || body["lines"].([]any)[0].(map[string]any)["route_id"] != "R1" {
		t.Errorf("mode=2&limit=1 gave %v", body)
	}

	for _, bad := range []string{"/v1/lines?mode=train", "/v1/lines?mode=-1", "/v1/lines?limit=0", "/v1/lines?limit=1001"} {
		rec, b := h.get(t, bad)
		assertEnvelope(t, rec, b, 400, "invalid_parameter")
	}
}

func TestLineNow_WithTheTimetable_FillsTheScheduledFields(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{
		matched("R1", "trip-a", "s1", 1, ptr(int32(90)), 0),
		matched("R1", "trip-a", "s2", 2, ptr(int32(120)), 0),
	})

	_, body := h.get(t, "/v1/lines/R1/now")

	if body["short_name"] != "T1" {
		t.Errorf("short_name = %v", body["short_name"])
	}
	trip := body["trips"].([]any)[0].(map[string]any)
	next := trip["next_stop"].(map[string]any)
	if trip["headsign"] != "Emu Plains" || next["name"] != "Stop s1" || next["stop_sequence"] != 1.0 {
		t.Errorf("trip = %v", trip)
	}
	if next["scheduled"] != "2026-09-23T11:10:00+10:00" || next["predicted"] != "2026-09-23T11:11:30+10:00" {
		t.Errorf("scheduled %v predicted %v; want 11:10 and 90 s later", next["scheduled"], next["predicted"])
	}
}

func TestLineNow_KnownRouteWithNothingRunning_IsEmptyNotMissing(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{matched("R1", "trip-a", "s1", 1, ptr(int32(0)), 0)})
	rec, body := h.get(t, "/v1/lines/R2/now")
	if rec.Code != 200 || body["count"] != 0.0 || len(body["trips"].([]any)) != 0 {
		t.Errorf("got %d %v; want 200 with no trips (§7.1)", rec.Code, body)
	}
	if body["feed_age_s"] != 9.0 {
		t.Errorf("feed_age_s = %v, want the newest feed's age", body["feed_age_s"])
	}
}

func TestLineNow_CancelledTrip_HasStatusCancelled(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{matched("R1", "trip-a", "s1", 1, nil, tripRelCanceled)})
	_, body := h.get(t, "/v1/lines/R1/now")
	next := body["trips"].([]any)[0].(map[string]any)["next_stop"].(map[string]any)
	if next["status"] != "cancelled" || next["predicted"] != nil {
		t.Errorf("next_stop = %v; want cancelled with no prediction", next)
	}
}

func TestStopNow_ListsUpcomingDeparturesInPredictedOrder(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{
		matched("R1", "trip-a", "s1", 1, ptr(int32(0)), 0),   // 11:10
		matched("R2", "trip-b", "s1", 4, ptr(int32(600)), 0), // 11:05 + 10 min = 11:15
		matched("R1", "trip-c", "s1", 1, ptr(int32(0)), 0),   // 13:00: outside 60 min
		obs("R1", "trip-x", "s1", ptr(int32(0)), nil, 0),     // unmatched: no time to place
		matched("R1", "trip-a", "s2", 2, ptr(int32(0)), 0),   // another stop
	})

	_, body := h.get(t, "/v1/stops/s1/now")

	if body["name"] != "Stop s1" || body["as_of"] != "2026-09-23T11:04:03+10:00" {
		t.Errorf("header = %v", body)
	}
	deps := body["departures"].([]any)
	if body["count"] != 2.0 || len(deps) != 2 {
		t.Fatalf("got %d departures, want trip-a then trip-b: %v", len(deps), deps)
	}
	a, b := deps[0].(map[string]any), deps[1].(map[string]any)
	if a["trip_id"] != "trip-a" || b["trip_id"] != "trip-b" {
		t.Errorf("order = %v, %v; want by predicted time, so the late trip-b second", a["trip_id"], b["trip_id"])
	}
	if b["scheduled"] != "2026-09-23T11:05:00+10:00" || b["predicted"] != "2026-09-23T11:15:00+10:00" || b["status"] != "late" || b["short_name"] != "T2" || b["headsign"] != "Liverpool" {
		t.Errorf("trip-b = %v", b)
	}

	_, body = h.get(t, "/v1/stops/s1/now?window_min=180&limit=5")
	if body["count"] != 3.0 {
		t.Errorf("a 3-hour window: %v departures, want 3", body["count"])
	}
}

func TestStopNow_BadInput(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct{ path, code string }{
		{"/v1/stops/nowhere/now", "stop_not_found"},
		{"/v1/stops/s1/now?window_min=0", "invalid_parameter"},
		{"/v1/stops/s1/now?window_min=181", "invalid_parameter"},
		{"/v1/stops/s1/now?limit=101", "invalid_parameter"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec, body := h.get(t, c.path)
			status := 400
			if c.code == "stop_not_found" {
				status = 404
			}
			assertEnvelope(t, rec, body, status, c.code)
		})
	}
}

func TestAdminStats_ReportsEverySource(t *testing.T) {
	c := cache.New(time.Hour, func() time.Time { return now })
	c.Update("trains", now.Add(-9*time.Second), nil)
	oldest := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	h := harness{h: NewHandler(Options{
		Cache:         c,
		Schedules:     func() []*match.Schedule { return []*match.Schedule{fixtureSchedule()} },
		PipelineStats: func() ingest.Stats { return ingest.Stats{QueueLen: 3, QueueCap: 8192, Written: 100} },
		MatchCounts: func() map[string]match.Counts {
			return map[string]match.Counts{"trains": {FullMatch: 99, UnknownTrip: 1}}
		},
		RequestsToday: func() int { return 42 },
		Maintenance: func(context.Context) (store.Maintenance, error) {
			return store.Maintenance{OldestPartition: &oldest, Partitions: 10, Watermark: now.Truncate(time.Hour)}, nil
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.DiscardHandler),
	})}

	rec, body := h.get(t, "/v1/admin/stats")

	if rec.Code != 200 || body["requests_today"] != 42.0 {
		t.Fatalf("got %d %v", rec.Code, body)
	}
	if s := body["active_schedule_versions"].([]any)[0].(map[string]any); s["version_id"] != 41.0 || s["trip_count"] != 3.0 {
		t.Errorf("schedule = %v", s)
	}
	if f := body["feeds"].([]any)[0].(map[string]any); f["feed_id"] != "trains" || f["feed_age_s"] != 9.0 {
		t.Errorf("feed = %v", f)
	}
	if m := body["match_last_poll"].(map[string]any)["trains"].(map[string]any); m["match_rate"] != 0.99 {
		t.Errorf("match = %v", m)
	}
	if i := body["ingest"].(map[string]any); i["queue_len"] != 3.0 || i["written"] != 100.0 {
		t.Errorf("ingest = %v", i)
	}
	if mt := body["maintenance"].(map[string]any); mt["oldest_partition"] != "2026-09-16" || mt["partitions"] != 10.0 {
		t.Errorf("maintenance = %v", mt)
	}
}
