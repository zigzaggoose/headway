package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zigzaggoose/transitlateagain/internal/cache"
	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/ingest"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/metrics"
)

// fixtureSchedule is feed "trains": routes R1 (T1) and R2 (T2), stops s1 to
// s9, and trip-a on R1 calling at s1 at 11:10 and s2 at 11:20 on its service
// date. Observations only get a scheduled time when they carry a sequence.
func fixtureSchedule() *match.Schedule {
	s := match.NewSchedule(41, "trains")
	s.AddRoute("R1", match.Route{ShortName: "T1", LongName: "North Shore", Type: 2})
	s.AddRoute("R2", match.Route{ShortName: "T2", LongName: "Inner West", Type: 2})
	s.AddRoute("F1", match.Route{ShortName: "F1", LongName: "Manly", Type: 4})
	s.AddRoute("RTTA_DEF", match.Route{Type: 2})
	for _, id := range []string{"s1", "s2", "s3", "s5", "s7", "s9"} {
		s.AddStop(id, "Stop "+id)
	}
	s.AddTrip("trip-a", match.Trip{RouteID: "R1", ServiceID: "X", Headsign: "Emu Plains"})
	s.AddStopTime("trip-a", match.StopTime{Seq: 1, StopID: "s1", ArrS: 11*3600 + 600, DepS: 11*3600 + 600})
	s.AddStopTime("trip-a", match.StopTime{Seq: 2, StopID: "s2", ArrS: 11*3600 + 1200, DepS: match.NoTime})
	s.AddTrip("trip-b", match.Trip{RouteID: "R2", ServiceID: "X", Headsign: "Liverpool"})
	s.AddStopTime("trip-b", match.StopTime{Seq: 4, StopID: "s1", ArrS: 11*3600 + 300, DepS: 11*3600 + 300})
	s.AddTrip("trip-c", match.Trip{RouteID: "R1", ServiceID: "X", Headsign: "Hornsby"})
	s.AddStopTime("trip-c", match.StopTime{Seq: 1, StopID: "s1", ArrS: 13 * 3600, DepS: 13 * 3600})
	return s
}

// 11:04:12 AEST. Feed timestamps below are a few seconds before it.
var now = time.Date(2026, 9, 23, 1, 4, 12, 0, time.UTC)

var thresholds = config.Thresholds{EarlyS: -60, LateS: 300, VeryLateS: 900}

func ptr[T any](v T) *T { return &v }

func obs(routeID, tripID, stopID string, delay *int32, dir *int16, rel int32) ingest.Observation {
	return ingest.Observation{
		ServiceDate:    time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		FeedID:         "trains",
		TripID:         tripID,
		StopID:         stopID,
		RouteID:        routeID,
		FeedTS:         now.Add(-9 * time.Second),
		ObservedDelayS: delay,
		DirectionID:    dir,
		TripRel:        rel,
	}
}

type harness struct {
	h   http.Handler
	log *bytes.Buffer
}

func newHarness(t *testing.T, ping error, feeds ...[]ingest.Observation) harness {
	t.Helper()
	c := cache.New(45*time.Minute, func() time.Time { return now })
	for _, f := range feeds {
		c.Update("trains", now.Add(-9*time.Second), f)
	}
	var buf bytes.Buffer
	return harness{
		h: NewHandler(Options{
			Cache:           c,
			Ping:            func(context.Context) error { return ping },
			ScheduleLoaded:  func() bool { return true },
			Schedules:       func() []*match.Schedule { return []*match.Schedule{fixtureSchedule()} },
			OnTime:          thresholds,
			ReadyMaxFeedAge: 120 * time.Second,
			Now:             func() time.Time { return now },
			Log:             slog.New(slog.NewJSONHandler(&buf, nil)),
		}),
		log: &buf,
	}
}

func (h harness) get(t *testing.T, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("%s: content type = %q", path, ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body is not JSON: %v\n%s", path, err, rec.Body)
	}
	return rec, body
}

// assertEnvelope checks the §7.2 shape, and that the request id in the body
// is the one in the header, so a client report leads to the log line.
func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, body map[string]any, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %v", body)
	}
	if e["code"] != code {
		t.Errorf("code = %v, want %s", e["code"], code)
	}
	if id := rec.Header().Get("X-Request-Id"); id == "" || e["request_id"] != id {
		t.Errorf("request_id = %v, header = %q; want equal and non-empty", e["request_id"], id)
	}
}

func TestHealthz_AlwaysOK_EvenWithTheDatabaseDown(t *testing.T) {
	rec, body := newHarness(t, errors.New("connection refused")).get(t, "/healthz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("got %d %v, want 200 ok", rec.Code, body)
	}
}

func TestReadyz_EachDependency_DecidesReadiness(t *testing.T) {
	fresh := []ingest.Observation{obs("R1", "trip-a", "s1", ptr(int32(0)), nil, 0)}
	cases := []struct {
		name        string
		ping        error
		feeds       [][]ingest.Observation
		wantStatus  int
		wantReasons []string
	}{
		{"a live database and a fresh feed is ready", nil, [][]ingest.Observation{fresh}, 200, nil},
		{"a database that does not answer is not ready", errors.New("timeout"), [][]ingest.Observation{fresh}, 503, []string{"database did not answer within 2s"}},
		{"no feed yet is not ready", nil, nil, 503, []string{"no feed has succeeded yet"}},
		{"both failures are both reported", errors.New("timeout"), nil, 503, []string{"database did not answer within 2s", "no feed has succeeded yet"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := newHarness(t, c.ping, c.feeds...).get(t, "/readyz")
			if c.wantStatus == 200 {
				if rec.Code != 200 || body["status"] != "ready" {
					t.Errorf("got %d %v, want 200 ready", rec.Code, body)
				}
				return
			}
			assertEnvelope(t, rec, body, c.wantStatus, "not_ready")
			got, _ := json.Marshal(body["reasons"]) // re-encoding a decoded value cannot fail
			want, _ := json.Marshal(c.wantReasons)  // nor can a []string
			if string(got) != string(want) {
				t.Errorf("reasons = %s, want %s", got, want)
			}
		})
	}
}

func TestReadyz_NoSchedule_IsNotReady(t *testing.T) {
	c := cache.New(45*time.Minute, func() time.Time { return now })
	c.Update("trains", now, nil)
	h := harness{h: NewHandler(Options{
		Cache: c, Ping: func(context.Context) error { return nil }, ScheduleLoaded: func() bool { return false },
		ReadyMaxFeedAge: 120 * time.Second, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
	})}
	rec, body := h.get(t, "/readyz")
	assertEnvelope(t, rec, body, 503, "not_ready")
	if r := body["reasons"].([]any); len(r) != 1 || r[0] != "no schedule version is loaded" {
		t.Errorf("reasons = %v", r)
	}
}

func TestReadyz_StaleFeed_IsNotReady(t *testing.T) {
	c := cache.New(45*time.Minute, func() time.Time { return now })
	c.Update("trains", now.Add(-121*time.Second), nil)
	h := harness{h: NewHandler(Options{
		Cache: c, Ping: func(context.Context) error { return nil }, ScheduleLoaded: func() bool { return true }, OnTime: thresholds,
		ReadyMaxFeedAge: 120 * time.Second, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
	})}
	rec, body := h.get(t, "/readyz")
	assertEnvelope(t, rec, body, 503, "not_ready")
	if r := body["reasons"].([]any); len(r) != 1 || !strings.Contains(r[0].(string), "within 2m0s") {
		t.Errorf("reasons = %v", r)
	}
}

func TestLineNow_ActiveRoute_ReturnsTripsAndSummary(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{
		obs("R1", "trip-c", "s9", ptr(int32(1000)), nil, 0),
		obs("R1", "trip-a", "s1", ptr(int32(30)), nil, 0),
		obs("R1", "trip-a", "s2", ptr(int32(45)), nil, 0),
		obs("R1", "trip-b", "s5", ptr(int32(400)), nil, 0),
		obs("R1", "trip-x", "s7", ptr(int32(0)), nil, tripRelCanceled),
		obs("R2", "trip-z", "s3", ptr(int32(0)), nil, 0),
	})
	rec, body := h.get(t, "/v1/lines/R1/now")

	if rec.Code != 200 {
		t.Fatalf("status = %d: %v", rec.Code, body)
	}
	if body["as_of"] != "2026-09-23T11:04:03+10:00" {
		t.Errorf("as_of = %v, want Sydney time with its offset", body["as_of"])
	}
	if body["feed_age_s"] != 9.0 {
		t.Errorf("feed_age_s = %v, want 9", body["feed_age_s"])
	}
	if body["direction_id"] != nil {
		t.Errorf("direction_id = %v, want null when not filtered", body["direction_id"])
	}

	sum := body["summary"].(map[string]any)
	want := map[string]float64{"active_trips": 3, "early": 0, "on_time": 1, "late": 1, "very_late": 1, "cancelled": 1, "median_delay_s": 400}
	for k, v := range want {
		if sum[k] != v {
			t.Errorf("summary.%s = %v, want %v", k, sum[k], v)
		}
	}

	trips := body["trips"].([]any)
	if body["count"] != 4.0 || len(trips) != 4 {
		t.Fatalf("count = %v with %d trips, want 4", body["count"], len(trips))
	}
	first := trips[0].(map[string]any)
	next := first["next_stop"].(map[string]any)
	if first["trip_id"] != "trip-a" || next["stop_id"] != "s1" || next["delay_s"] != 30.0 || next["status"] != "on_time" {
		t.Errorf("first trip = %v, want trip-a at its first reported stop s1, 30 s, on_time", first)
	}
	if first["service_date"] != "2026-09-23" || first["matched"] != false {
		t.Errorf("first trip = %v", first)
	}
}

func TestLineNow_Limit_TruncatesTripsButNotTheSummary(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{
		obs("R1", "trip-a", "s1", ptr(int32(0)), nil, 0),
		obs("R1", "trip-b", "s1", ptr(int32(0)), nil, 0),
		obs("R1", "trip-c", "s1", ptr(int32(0)), nil, 0),
	})
	_, body := h.get(t, "/v1/lines/R1/now?limit=2")
	if body["count"] != 2.0 || body["summary"].(map[string]any)["active_trips"] != 3.0 {
		t.Errorf("count = %v, active_trips = %v; want 2 and 3", body["count"], body["summary"].(map[string]any)["active_trips"])
	}
}

func TestLineNow_Direction_FiltersAndEchoes(t *testing.T) {
	h := newHarness(t, nil, []ingest.Observation{
		obs("R1", "trip-a", "s1", ptr(int32(0)), ptr(int16(0)), 0),
		obs("R1", "trip-b", "s1", ptr(int32(0)), ptr(int16(1)), 0),
		obs("R1", "trip-c", "s1", ptr(int32(0)), nil, 0),
	})
	_, body := h.get(t, "/v1/lines/R1/now?direction=0")
	trips := body["trips"].([]any)
	if body["direction_id"] != 0.0 || len(trips) != 1 || trips[0].(map[string]any)["trip_id"] != "trip-a" {
		t.Errorf("direction_id = %v, trips = %v; want only trip-a", body["direction_id"], trips)
	}
}

func TestLineNow_BadInput_ReturnsTheEnvelope(t *testing.T) {
	cases := []struct {
		name, path string
		status     int
		code       string
	}{
		{"a direction other than 0 or 1 is rejected", "/v1/lines/R1/now?direction=2", 400, "invalid_parameter"},
		{"a non-numeric direction is rejected", "/v1/lines/R1/now?direction=up", 400, "invalid_parameter"},
		{"a zero limit is rejected", "/v1/lines/R1/now?limit=0", 400, "invalid_parameter"},
		{"a limit above 1000 is rejected", "/v1/lines/R1/now?limit=1001", 400, "invalid_parameter"},
		{"a route not in the schedule is not found", "/v1/lines/R9/now", 404, "line_not_found"},
		{"an unknown path is not found", "/v1/nope", 404, "not_found"},
	}
	h := newHarness(t, nil, []ingest.Observation{obs("R1", "trip-a", "s1", ptr(int32(0)), nil, 0)})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := h.get(t, c.path)
			assertEnvelope(t, rec, body, c.status, c.code)
		})
	}
}

func TestStatus_Boundaries_MatchTheRollupSQL(t *testing.T) {
	s := &server{Options{OnTime: thresholds}}
	cases := []struct {
		delay *int32
		want  string
	}{
		{nil, "unknown"},
		{ptr(int32(0)), "on_time"},
		{ptr(int32(-61)), "early"},
		{ptr(int32(-60)), "on_time"},
		{ptr(int32(300)), "on_time"},
		{ptr(int32(301)), "late"},
		{ptr(int32(900)), "late"},
		{ptr(int32(901)), "very_late"},
	}
	for _, c := range cases {
		if got := s.status(c.delay, 0); got != c.want {
			t.Errorf("status(%v) = %s, want %s", c.delay, got, c.want)
		}
	}
	if got := s.status(ptr(int32(0)), tripRelCanceled); got != "cancelled" {
		t.Errorf("a cancelled trip's status = %s, want cancelled whatever its delay", got)
	}
}

func TestSummarise_EvenCount_MedianIsTheLowerMiddle(t *testing.T) {
	s := &server{Options{OnTime: thresholds}}
	var trips []cache.Trip
	for _, d := range []int32{40, 10, 30, 20} {
		trips = append(trips, cache.Trip{DelayS: ptr(d)})
	}
	if m := s.summarise(trips).MedianDelayS; m == nil || *m != 20 {
		t.Errorf("median = %v, want 20", m)
	}
}

func TestMiddleware_HandlerPanic_Returns500AndLogsTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	h := harness{h: NewHandler(Options{
		Cache: nil, // any read of it panics
		Now:   func() time.Time { return now },
		Log:   slog.New(slog.NewJSONHandler(&buf, nil)),
	})}

	rec, body := h.get(t, "/v1/lines/R1/now")

	assertEnvelope(t, rec, body, 500, "internal")
	if msg := body["error"].(map[string]any)["message"]; msg != "internal error" {
		t.Errorf("message = %v, want the literal \"internal error\"", msg)
	}
	id := rec.Header().Get("X-Request-Id")
	for _, want := range []string{`"msg":"handler panicked"`, `"msg":"request failed"`, `"msg":"http request"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %s", want)
		}
	}
	if strings.Count(buf.String(), id) < 3 {
		t.Errorf("request id %s is not on every log line:\n%s", id, buf.String())
	}
}

func TestMiddleware_EveryRequest_WritesOneAccessLogLine(t *testing.T) {
	h := newHarness(t, nil)
	h.get(t, "/healthz")
	if n := strings.Count(h.log.String(), `"msg":"http request"`); n != 1 {
		t.Errorf("%d access log lines, want 1:\n%s", n, h.log)
	}
	if !strings.Contains(h.log.String(), `"status":200`) {
		t.Errorf("access log lacks the status:\n%s", h.log)
	}
}

// A 503 is "not ready yet", which every probe during startup gets. It is
// logged as a request, not as an error.
func TestMiddleware_NotReady_IsNotAnErrorLine(t *testing.T) {
	var buf bytes.Buffer
	h := harness{h: NewHandler(Options{
		Cache: cache.New(time.Hour, func() time.Time { return now }), Ping: func(context.Context) error { return nil },
		ScheduleLoaded: func() bool { return false }, ReadyMaxFeedAge: time.Minute,
		Now: func() time.Time { return now }, Log: slog.New(slog.NewJSONHandler(&buf, nil)),
	})}
	rec, _ := h.get(t, "/readyz")
	if rec.Code != 503 || strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("status %d, log:\n%s", rec.Code, buf.String())
	}
}

func TestMiddleware_Metrics_LabelTheRouteByPatternNeverByPath(t *testing.T) {
	counter := func(route, status string) float64 {
		return testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(route, status))
	}

	t.Run("a path with an id is counted under its pattern", func(t *testing.T) {
		before := counter("GET /v1/lines/{route_id}/now", "404")
		newHarness(t, nil).get(t, "/v1/lines/NO-SUCH-ROUTE/now")
		if got := counter("GET /v1/lines/{route_id}/now", "404") - before; got != 1 {
			t.Errorf("pattern counter grew by %v, want 1", got)
		}
		if got := counter("/v1/lines/NO-SUCH-ROUTE/now", "404"); got != 0 {
			t.Error("the raw path became a label value")
		}
	})
	t.Run("a request the limiter refuses never reaches the mux", func(t *testing.T) {
		h := NewHandler(Options{
			Cache: cache.New(time.Hour, func() time.Time { return now }), RateLimit: 1,
			Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
		})
		before := counter("unrouted", "429")
		for range 2 {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/lines", nil))
		}
		if got := counter("unrouted", "429") - before; got != 1 {
			t.Errorf("unrouted 429s grew by %v, want 1", got)
		}
	})
}

func TestMetricsEndpoint_OnlyOnTheAdminHandler(t *testing.T) {
	o := Options{Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler), Metrics: metrics.Handler()}
	get := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec
	}

	if rec := get(NewAdminHandler(o)); rec.Code != 200 || !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Errorf("admin /metrics = %d", rec.Code)
	}
	if rec := get(NewHandler(o)); rec.Code != 404 {
		t.Errorf("public /metrics = %d, want 404", rec.Code)
	}
}

func TestCORS_OneConfiguredOrigin_OnEveryPublicResponse(t *testing.T) {
	const site = "https://transitlateagain.dev"
	opts := func(origin string) Options {
		return Options{
			Cache: cache.New(time.Hour, func() time.Time { return now }), RateLimit: 1, CORSOrigin: origin,
			Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
		}
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	t.Run("an error and a 429 carry it too, so the site can read them", func(t *testing.T) {
		h := NewHandler(opts(site))
		for _, want := range []int{404, 429} {
			rec := get(h, "/no-such-path")
			if rec.Code != want || rec.Header().Get("Access-Control-Allow-Origin") != site {
				t.Errorf("%d: Access-Control-Allow-Origin = %q, want %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"), site)
			}
		}
	})
	t.Run("unconfigured, no browser on another site may read the API", func(t *testing.T) {
		if v := get(NewHandler(opts("")), "/healthz").Header().Get("Access-Control-Allow-Origin"); v != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want none", v)
		}
	})
	t.Run("never on the admin port", func(t *testing.T) {
		if v := get(NewAdminHandler(opts(site)), "/v1/admin/stats").Header().Get("Access-Control-Allow-Origin"); v != "" {
			t.Errorf("admin Access-Control-Allow-Origin = %q, want none", v)
		}
	})
}
