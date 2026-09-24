// Package api maps HTTP requests onto the latest-state cache and, from Stage
// 2, the rollup tables. It reads; it never writes, and it never waits on the
// ingest path (§4.2 component 10).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/cache"
	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/ingest"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/store"
)

// Options is everything the handlers read. Ping is a function rather than
// the store so tests can make the database fail without one.
type Options struct {
	Cache           *cache.Cache
	Ping            func(context.Context) error
	ScheduleLoaded  func() bool // true once any feed's timetable is in the matcher
	Schedules       func() []*match.Schedule
	History         func(context.Context, store.HistoryQuery) (store.History, error)
	TripStops       func(ctx context.Context, serviceDate time.Time, feedID, tripID string) (map[string]store.StopObservation, error)
	HistoryMaxDays  int
	RateLimit       float64 // HTTP_RATE_LIMIT_RPS per client IP; 0 disables it
	ClientIPHeader  string  // HTTP_CLIENT_IP_HEADER; empty means the connection's address
	CORSOrigin      string  // HTTP_CORS_ORIGIN; the one site allowed to read the API from a browser
	OnTime          config.Thresholds
	ReadyMaxFeedAge time.Duration
	Now             func() time.Time
	Log             *slog.Logger

	// Admin stats sources. Each is a snapshot safe to take from any goroutine.
	PipelineStats func() ingest.Stats
	MatchCounts   func() map[string]match.Counts
	RequestsToday func() int
	Maintenance   func(context.Context) (store.Maintenance, error)

	// Metrics is the /metrics handler, served on the admin address only.
	// Nil when METRICS_ENABLED is false.
	Metrics http.Handler
}

type server struct{ Options }

// NewHandler returns the API with its middleware applied. This is the one
// place that knows the paths.
func NewHandler(o Options) http.Handler {
	s := &server{o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/lines", s.lines)
	mux.HandleFunc("GET /v1/lines/{route_id}/now", s.lineNow)
	mux.HandleFunc("GET /v1/stops/{stop_id}/now", s.stopNow)
	mux.HandleFunc("GET /v1/lines/{route_id}/history", s.lineHistory)
	mux.HandleFunc("GET /v1/stops/{stop_id}/history", s.stopHistory)
	mux.HandleFunc("GET /v1/trips/{trip_id}", s.trip)
	notFound(mux)
	// The limit runs inside the middleware, so a 429 still gets a request id
	// and an access log line.
	h := s.middleware(s.limit(mux))
	if o.CORSOrigin != "" {
		h = cors(o.CORSOrigin, h)
	}
	return h
}

// cors lets the one site that renders this API read its responses from a
// browser (§7). Every endpoint is a GET with no custom request headers, so
// browsers never preflight and there is no OPTIONS handling. It is set before
// anything else writes, so errors and 429s are readable too.
func cors(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "Retry-After, X-Request-Id")
		next.ServeHTTP(w, r)
	})
}

// NewAdminHandler serves the operator's routes. It is a separate handler for
// a separate listener, so no path on the public address can reach it (§7).
func NewAdminHandler(o Options) http.Handler {
	s := &server{o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/stats", s.adminStats)
	if o.Metrics != nil {
		mux.Handle("GET /metrics", o.Metrics)
	}
	notFound(mux)
	return s.middleware(mux)
}

// Without this the mux answers unknown paths in plain text, and §7.2 says
// every non-2xx response carries the envelope.
func notFound(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no endpoint at "+r.URL.Path)
	})
}
