package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/cache"
	"github.com/zigzaggoose/transitlateagain/internal/ingest"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/store"
)

// tripHarness serves the fixture timetable with TripStops answering seen, or
// failing with err.
func tripHarness(seen map[string]store.StopObservation, err error, feeds ...[]ingest.Observation) harness {
	c := cache.New(45*time.Minute, func() time.Time { return now })
	for _, f := range feeds {
		c.Update("trains", now.Add(-9*time.Second), f)
	}
	return harness{h: NewHandler(Options{
		Cache:     c,
		Schedules: func() []*match.Schedule { return []*match.Schedule{fixtureSchedule()} },
		OnTime:    thresholds,
		Now:       func() time.Time { return now },
		Log:       slog.New(slog.DiscardHandler),
		TripStops: func(_ context.Context, _ time.Time, feedID, tripID string) (map[string]store.StopObservation, error) {
			if feedID != "trains" || tripID != "trip-a" {
				return nil, errors.New("wrong trip asked for")
			}
			return seen, err
		},
	})}
}

func TestTrip_EveryTimetabledStop_WithItsLastObservation(t *testing.T) {
	// trip-a: s1 at 11:10, s2 at 11:20; now is 11:04:12.
	h := tripHarness(map[string]store.StopObservation{
		"s1": {DelayS: ptr(int32(-420))}, // left at 11:03, early
		"s2": {StopTimeRel: 1},           // skipped, no delay
	}, nil)

	rec, body := h.get(t, "/v1/trips/trip-a?service_date=2026-09-23")

	if rec.Code != 200 || body["route_id"] != "R1" || body["short_name"] != "T1" || body["headsign"] != "Emu Plains" || body["count"] != 2.0 {
		t.Fatalf("got %d %v", rec.Code, body)
	}
	if s, e := body["start"].(map[string]any), body["end"].(map[string]any); s["name"] != "Stop s1" || s["scheduled"] != "2026-09-23T11:10:00+10:00" ||
		e["name"] != "Stop s2" || e["scheduled"] != "2026-09-23T11:20:00+10:00" {
		t.Errorf("start %v end %v; the end has no departure, so its arrival", s, e)
	}
	stops := body["stops"].([]any)
	first, second := stops[0].(map[string]any), stops[1].(map[string]any)
	t.Run("a stop the train has left is passed, at its observed time", func(t *testing.T) {
		if first["status"] != "early" || first["predicted"] != "2026-09-23T11:03:00+10:00" || first["passed"] != true {
			t.Errorf("s1 = %v", first)
		}
	})
	t.Run("a skipped stop says so and is not passed before its time", func(t *testing.T) {
		if second["status"] != "skipped" || second["delay_s"] != nil || second["predicted"] != nil || second["passed"] != false {
			t.Errorf("s2 = %v", second)
		}
	})
}

func TestTrip_StopTheFeedNeverMentioned_IsUnknownAndPassesOnSchedule(t *testing.T) {
	h := tripHarness(map[string]store.StopObservation{}, nil)
	_, body := h.get(t, "/v1/trips/trip-a?service_date=2026-09-22")
	stops := body["stops"].([]any)
	if s := stops[0].(map[string]any); s["status"] != "unknown" || s["passed"] != true {
		t.Errorf("yesterday's s1 = %v, want unknown and passed by its scheduled time", s)
	}
}

func TestTrip_BadRequests(t *testing.T) {
	h := tripHarness(nil, errors.New("connection reset"))
	for _, c := range []struct{ path, code string }{
		{"/v1/trips/trip-a", "invalid_parameter"},
		{"/v1/trips/trip-a?service_date=23-09-2026", "invalid_parameter"},
		{"/v1/trips/nope?service_date=2026-09-23", "trip_not_found"},
		{"/v1/trips/trip-a?service_date=2026-09-23", "internal"},
	} {
		t.Run(c.path, func(t *testing.T) {
			rec, body := h.get(t, c.path)
			assertEnvelope(t, rec, body, map[string]int{"invalid_parameter": 400, "trip_not_found": 404, "internal": 500}[c.code], c.code)
		})
	}
}

func TestLineNow_EachTrip_CarriesItsFirstAndLastStop(t *testing.T) {
	h := tripHarness(nil, nil, []ingest.Observation{matched("R1", "trip-a", "s1", 1, ptr(int32(0)), 0)})
	_, body := h.get(t, "/v1/lines/R1/now")
	tr := body["trips"].([]any)[0].(map[string]any)
	if s, e := tr["start"].(map[string]any), tr["end"].(map[string]any); s["stop_id"] != "s1" || s["scheduled"] != "2026-09-23T11:10:00+10:00" || e["stop_id"] != "s2" {
		t.Errorf("start %v end %v", s, e)
	}
}
