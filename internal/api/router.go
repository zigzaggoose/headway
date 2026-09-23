// Package api maps HTTP requests onto the latest-state cache and, from Stage
// 2, the rollup tables. It reads; it never writes, and it never waits on the
// ingest path (§4.2 component 10).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/ingest"
	"github.com/zigzaggoose/headway/internal/match"
	"github.com/zigzaggoose/headway/internal/store"
)

// Options is everything the handlers read. Ping is a function rather than
// the store so tests can make the database fail without one.
type Options struct {
	Cache          *cache.Cache
	Ping           func(context.Context) error
	ScheduleLoaded func() bool // true once any feed's timetable is in the matcher
	Schedules      func() []*match.Schedule
	History        func(context.Context, store.HistoryQuery) (store.History, error)
	HistoryMaxDays int

	// Admin stats sources. Each is a snapshot safe to take from any goroutine.
	PipelineStats   func() ingest.Stats
	MatchCounts     func() map[string]match.Counts
	RequestsToday   func() int
	Maintenance     func(context.Context) (store.Maintenance, error)
	OnTime          config.Thresholds
	ReadyMaxFeedAge time.Duration
	Now             func() time.Time
	Log             *slog.Logger
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
	mux.HandleFunc("GET /v1/admin/stats", s.adminStats)
	// Without this the mux answers unknown paths in plain text, and §7.2 says
	// every non-2xx response carries the envelope.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no endpoint at "+r.URL.Path)
	})
	return s.middleware(mux)
}
