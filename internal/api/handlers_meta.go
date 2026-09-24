package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/match"
)

// healthz is liveness only. It checks nothing, because a Postgres outage
// that restarted the process would turn into a restart loop (§7.1).
func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz checks the three things §7.1 names: a timetable in the matcher, the
// database, and feed freshness.
func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	var reasons []string

	if !s.ScheduleLoaded() {
		reasons = append(reasons, "no schedule version is loaded")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		// The reason is for an operator; the error itself goes to the log,
		// because pgx errors can carry connection details.
		s.Log.Warn("readiness: database ping failed", "component", "api", "request_id", requestID(r.Context()), "err", err.Error())
		reasons = append(reasons, "database did not answer within 2s")
	}

	// A snapshot lands in the cache only after a fetch and a decode have both
	// succeeded, so its age is how long ago a feed last succeeded.
	if newest := s.Cache.Newest(); newest.IsZero() {
		reasons = append(reasons, "no feed has succeeded yet")
	} else if age := s.Now().Sub(newest); age > s.ReadyMaxFeedAge {
		reasons = append(reasons, fmt.Sprintf("no feed has succeeded within %s (newest is %s old)", s.ReadyMaxFeedAge, age.Round(time.Second)))
	}

	if len(reasons) > 0 {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "not ready", reasons...)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type adminStats struct {
	Schedules     []adminSchedule       `json:"active_schedule_versions"`
	Feeds         []adminFeed           `json:"feeds"`
	RequestsToday int                   `json:"requests_today"`
	Ingest        adminIngest           `json:"ingest"`
	Match         map[string]adminMatch `json:"match_last_poll"`
	Maintenance   adminMaintenance      `json:"maintenance"`
}

type adminSchedule struct {
	FeedID    string `json:"feed_id"`
	VersionID int64  `json:"version_id"`
	TripCount int    `json:"trip_count"`
}

type adminFeed struct {
	FeedID   string `json:"feed_id"`
	FeedTS   string `json:"feed_ts"`
	FeedAgeS int64  `json:"feed_age_s"`
}

type adminIngest struct {
	QueueLen   int    `json:"queue_len"`
	QueueCap   int    `json:"queue_cap"`
	Admitted   uint64 `json:"admitted"`
	Suppressed uint64 `json:"suppressed"`
	Dropped    uint64 `json:"dropped"`
	Written    uint64 `json:"written"`
	Failed     uint64 `json:"failed"`
	Conflicts  uint64 `json:"conflicts"`
	FilterSize int    `json:"filter_size"`
}

type adminMatch struct {
	MatchRate float64      `json:"match_rate"`
	Counts    match.Counts `json:"counts"`
}

type adminMaintenance struct {
	OldestPartition *string `json:"oldest_partition"`
	Partitions      int     `json:"partitions"`
	DefaultRows     int64   `json:"default_partition_rows"`
	RollupWatermark string  `json:"rollup_watermark"`
	DatabaseBytes   int64   `json:"database_bytes"`
}

// adminStats is the operator's view (§7.1). It is not a public contract: it
// reports what exists, and changes freely as the service does.
func (s *server) adminStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()
	m, err := s.Maintenance(ctx)
	if err != nil {
		s.Log.Error("admin stats query failed", "component", "api", "request_id", requestID(r.Context()), "err", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	out := adminStats{
		Schedules:     []adminSchedule{},
		Feeds:         []adminFeed{},
		RequestsToday: s.RequestsToday(),
		Match:         map[string]adminMatch{},
		Maintenance: adminMaintenance{
			Partitions:      m.Partitions,
			DefaultRows:     m.DefaultRows,
			RollupWatermark: m.Watermark.UTC().Format(time.RFC3339),
			DatabaseBytes:   m.DatabaseBytes,
		},
	}
	if m.OldestPartition != nil {
		d := m.OldestPartition.Format(time.DateOnly)
		out.Maintenance.OldestPartition = &d
	}
	for _, sch := range s.Schedules() {
		out.Schedules = append(out.Schedules, adminSchedule{FeedID: sch.FeedID, VersionID: sch.VersionID, TripCount: sch.Trips()})
	}
	for id, ts := range s.Cache.Feeds() {
		out.Feeds = append(out.Feeds, adminFeed{FeedID: id, FeedTS: rfc3339(ts), FeedAgeS: sinceSeconds(s.Now(), ts)})
	}
	slices.SortFunc(out.Schedules, func(a, b adminSchedule) int { return cmp.Compare(a.FeedID, b.FeedID) })
	slices.SortFunc(out.Feeds, func(a, b adminFeed) int { return cmp.Compare(a.FeedID, b.FeedID) })
	for id, c := range s.MatchCounts() {
		out.Match[id] = adminMatch{MatchRate: c.MatchRate(), Counts: c}
	}
	p := s.PipelineStats()
	out.Ingest = adminIngest{
		QueueLen: p.QueueLen, QueueCap: p.QueueCap, Admitted: p.Admitted, Suppressed: p.Suppressed,
		Dropped: p.Dropped, Written: p.Written, Failed: p.Failed, Conflicts: p.Conflicts, FilterSize: p.FilterSize,
	}
	writeJSON(w, http.StatusOK, out)
}
