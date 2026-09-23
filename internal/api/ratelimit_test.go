package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/match"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestIPLimiter_BurstsToTheRateThenRefills(t *testing.T) {
	c := &clock{t: now}
	l := newIPLimiter(5, c.now)

	for i := range 5 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d of the first second was refused", i+1)
		}
	}
	ok, wait := l.allow("a")
	if ok || wait <= 0 || wait > 200*time.Millisecond {
		t.Errorf("sixth request: allowed %v, wait %s; want refused with at most 200 ms to wait", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Error("a second client was refused because of the first")
	}

	c.t = c.t.Add(200 * time.Millisecond)
	if ok, _ := l.allow("a"); !ok {
		t.Error("one token did not refill in a fifth of a second")
	}
	c.t = c.t.Add(time.Hour)
	for i := range 5 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("after an idle hour, request %d was refused: the bucket overfilled or never refilled", i+1)
		}
	}
	if ok, _ := l.allow("a"); ok {
		t.Error("an idle hour banked more than one second of requests")
	}
}

func TestIPLimiter_ForgetsIdleClients(t *testing.T) {
	c := &clock{t: now}
	l := newIPLimiter(5, c.now)
	l.allow("a")
	l.allow("b")
	c.t = c.t.Add(2 * time.Minute)
	l.allow("c")
	if n := l.size(); n != 1 {
		t.Errorf("tracking %d clients after the others went idle, want 1", n)
	}
}

func TestLimit_OverTheRate_Is429WithRetryAfter(t *testing.T) {
	c := &clock{t: now}
	h := NewHandler(Options{
		Cache:          cache.New(time.Hour, c.now),
		Schedules:      func() []*match.Schedule { return []*match.Schedule{fixtureSchedule()} },
		ScheduleLoaded: func() bool { return true },
		Ping:           nil,
		RateLimit:      2,
		OnTime:         thresholds,
		Now:            c.now,
		Log:            slog.New(slog.DiscardHandler),
	})
	get := func(path, ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = ip + ":51000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	for range 2 {
		if rec := get("/v1/lines", "203.0.113.9"); rec.Code != 200 {
			t.Fatalf("within the limit: %d", rec.Code)
		}
	}
	rec := get("/v1/lines", "203.0.113.9")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"too_many_requests"`) {
		t.Errorf("over the limit: %d, Retry-After %q, body %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("a 429 has no request id; the limit must run inside the middleware")
	}
	if rec := get("/v1/lines", "198.51.100.7"); rec.Code != 200 {
		t.Errorf("another client was limited: %d", rec.Code)
	}
	if rec := get("/healthz", "203.0.113.9"); rec.Code != 200 {
		t.Errorf("a health probe was limited: %d", rec.Code)
	}
}
