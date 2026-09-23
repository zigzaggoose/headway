package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/store"
)

var aest = time.FixedZone("AEST", 10*3600)

func hour(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, aest) }

func hr(start time.Time, early, on, late, very, cancelled int, p50, p90 int32, mean float32) store.HourRow {
	return store.HourRow{
		BucketStart: start, NObs: early + on + late + very + cancelled,
		NEarly: early, NOnTime: on, NLate: late, NVeryLate: very, NCancelled: cancelled,
		P50: &p50, P90: &p90, Mean: &mean,
	}
}

// historyHarness serves h from a fake store and records the query it got.
func historyHarness(h store.History, err error) (harness, *store.HistoryQuery) {
	var got store.HistoryQuery
	return harness{h: NewHandler(Options{
		Cache:          cache.New(time.Hour, func() time.Time { return now }),
		ScheduleLoaded: func() bool { return true },
		History: func(_ context.Context, q store.HistoryQuery) (store.History, error) {
			got = q
			return h, err
		},
		HistoryMaxDays: 90,
		OnTime:         thresholds,
		Now:            func() time.Time { return now },
		Log:            slog.New(slog.DiscardHandler),
	})}, &got
}

var known = store.History{ScheduleLoaded: true, Known: true, Name: "Strathfield Station, Platform 4"}

func TestHistory_BadInput_ReturnsTheEnvelope(t *testing.T) {
	h, _ := historyHarness(known, nil)
	cases := []struct{ name, path, code string }{
		{"an unreadable from", "/v1/stops/S/history?from=yesterday", "invalid_parameter"},
		{"an unreadable to", "/v1/stops/S/history?to=2026-13-01", "invalid_parameter"},
		{"from not before to", "/v1/stops/S/history?from=2026-09-21&to=2026-09-21", "invalid_parameter"},
		{"a range past HISTORY_MAX_DAYS", "/v1/stops/S/history?from=2026-01-01&to=2026-09-21", "range_too_large"},
		{"a direction other than 0 or 1", "/v1/lines/R/history?direction=2", "invalid_parameter"},
		{"a bucket other than hour or day", "/v1/lines/R/history?bucket=week", "invalid_parameter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := h.get(t, c.path)
			assertEnvelope(t, rec, body, 400, c.code)
		})
	}
}

func TestHistory_StoreAnswers_MapToStatuses(t *testing.T) {
	cases := []struct {
		name   string
		h      store.History
		err    error
		path   string
		status int
		code   string
	}{
		{"no schedule loaded is not ready (§7.2)", store.History{}, nil, "/v1/stops/S/history", 503, "not_ready"},
		{"an unknown stop", store.History{ScheduleLoaded: true}, nil, "/v1/stops/S/history", 404, "stop_not_found"},
		{"an unknown route", store.History{ScheduleLoaded: true}, nil, "/v1/lines/R/history", 404, "line_not_found"},
		{"a database failure is internal, with no detail", store.History{}, errors.New("conn refused: password=x"), "/v1/stops/S/history", 500, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _ := historyHarness(c.h, c.err)
			rec, body := h.get(t, c.path)
			assertEnvelope(t, rec, body, c.status, c.code)
			if c.status == 500 && body["error"].(map[string]any)["message"] != "internal error" {
				t.Errorf("a 500 leaked detail: %v", body)
			}
		})
	}
}

func TestHistory_Query_IsWhatTheParametersSay(t *testing.T) {
	h, got := historyHarness(known, nil)
	h.get(t, "/v1/stops/2000341/history?from=2026-09-20&to=2026-09-21&route_id=APS_1a&direction=1")

	if got.StopID != "2000341" || got.RouteID != "APS_1a" || got.Direction == nil || *got.Direction != 1 {
		t.Errorf("query = %+v", *got)
	}
	if !got.From.Equal(hour(20, 0)) || !got.To.Equal(hour(21, 0)) {
		t.Errorf("range = %s to %s; a bare date means Sydney midnight", got.From, got.To)
	}

	h.get(t, "/v1/lines/APS_1a/history")
	if got.StopID != "" || got.RouteID != "APS_1a" || !got.To.Equal(now) || !got.From.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("default query = %+v; want the route, and the seven days to now", *got)
	}
}

func TestHistory_Hourly_CombinesRowsThatShareAnHour(t *testing.T) {
	h, _ := historyHarness(store.History{ScheduleLoaded: true, Known: true, Name: "Central", Rows: []store.HourRow{
		// Two routes at 08:00: 10 visits at p50 30, and 30 visits at p50 90.
		hr(hour(20, 8), 0, 8, 2, 0, 1, 30, 200, 40),
		hr(hour(20, 8), 1, 27, 2, 0, 0, 90, 300, 100),
		hr(hour(20, 9), 0, 5, 0, 0, 0, 10, 20, 12),
	}}, nil)

	_, body := h.get(t, "/v1/stops/200060/history?from=2026-09-20&to=2026-09-21")

	if body["stop_id"] != "200060" || body["name"] != "Central" || body["route_id"] != nil || body["bucket"] != "hour" {
		t.Errorf("header = %v", body)
	}
	if body["from"] != "2026-09-20T00:00:00+10:00" || body["count"] != 2.0 {
		t.Errorf("from %v count %v", body["from"], body["count"])
	}
	b := body["buckets"].([]any)[0].(map[string]any)
	want := map[string]float64{
		"n_obs": 41, "n_early": 1, "n_on_time": 35, "n_late": 4, "n_cancelled": 1,
		// 35 on time of 40 with a delay; the cancelled call is not in it.
		"on_time_pct": 87.5,
		// The 30-visit row carries more weight than the 10-visit one.
		"delay_p50_s": 90, "delay_p90_s": 300,
		// (40*10 + 100*30) / 40, exactly.
		"delay_mean_s": 85,
	}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("08:00 %s = %v, want %v", k, b[k], v)
		}
	}
	if b["bucket_start"] != "2026-09-20T08:00:00+10:00" {
		t.Errorf("bucket_start = %v", b["bucket_start"])
	}
	single := body["buckets"].([]any)[1].(map[string]any)
	if single["delay_p50_s"] != 10.0 || single["delay_p90_s"] != 20.0 {
		t.Errorf("a single row's percentiles changed: %v", single)
	}
	tot := body["totals"].(map[string]any)
	if tot["n_obs"] != 46.0 || tot["on_time_pct"] != 88.9 {
		t.Errorf("totals = %v; want 46 visits, 40 of 45 on time", tot)
	}
}

// Day buckets are Sydney calendar days, so 23:00 and 00:00 local are two
// days even though they are one UTC day apart by an hour.
func TestHistory_Daily_GroupsBySydneyCalendarDay(t *testing.T) {
	h, _ := historyHarness(store.History{ScheduleLoaded: true, Known: true, Name: "T8", Rows: []store.HourRow{
		hr(hour(20, 9), 0, 5, 0, 0, 0, 10, 20, 12),
		hr(hour(20, 23), 0, 5, 0, 0, 0, 10, 20, 12),
		hr(hour(21, 0), 0, 3, 1, 0, 0, 60, 400, 90),
	}}, nil)

	_, body := h.get(t, "/v1/lines/APS_1a/history?bucket=day&from=2026-09-20&to=2026-09-22")

	b := body["buckets"].([]any)
	if len(b) != 2 {
		t.Fatalf("got %d day buckets, want 2", len(b))
	}
	if d0 := b[0].(map[string]any); d0["bucket_start"] != "2026-09-20T00:00:00+10:00" || d0["n_obs"] != 10.0 {
		t.Errorf("first day = %v", d0)
	}
	if body["route_id"] != "APS_1a" || body["short_name"] != "T8" {
		t.Errorf("line header = %v", body)
	}
}

func TestHistory_NoRows_IsAnEmptyListNotNull(t *testing.T) {
	h, _ := historyHarness(known, nil)
	rec, body := h.get(t, "/v1/stops/S/history")
	if rec.Code != http.StatusOK || body["buckets"] == nil || body["count"] != 0.0 {
		t.Errorf("got %d %v", rec.Code, body)
	}
	if tot := body["totals"].(map[string]any); tot["on_time_pct"] != nil || tot["delay_p50_s"] != nil {
		t.Errorf("totals with no data = %v, want nulls", tot)
	}
}
