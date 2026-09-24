package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/servicetime"
	"github.com/zigzaggoose/transitlateagain/internal/store"
)

// handlerTimeout is §10.1's budget for an API handler that reads Postgres.
const handlerTimeout = 15 * time.Second

type historyResponse struct {
	StopID    string          `json:"stop_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	RouteID   *string         `json:"route_id"`
	ShortName string          `json:"short_name,omitempty"`
	Bucket    string          `json:"bucket"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Totals    historyTotals   `json:"totals"`
	Buckets   []historyBucket `json:"buckets"`
	Count     int             `json:"count"`
}

type historyTotals struct {
	NObs      int      `json:"n_obs"`
	OnTimePct *float64 `json:"on_time_pct"`
	P50       *int32   `json:"delay_p50_s"`
	P90       *int32   `json:"delay_p90_s"`
}

type historyBucket struct {
	BucketStart string   `json:"bucket_start"`
	NObs        int      `json:"n_obs"`
	NEarly      int      `json:"n_early"`
	NOnTime     int      `json:"n_on_time"`
	NLate       int      `json:"n_late"`
	NVeryLate   int      `json:"n_very_late"`
	NSkipped    int      `json:"n_skipped"`
	NCancelled  int      `json:"n_cancelled"`
	OnTimePct   *float64 `json:"on_time_pct"`
	P50         *int32   `json:"delay_p50_s"`
	P90         *int32   `json:"delay_p90_s"`
	Mean        *float64 `json:"delay_mean_s"`
}

func (s *server) stopHistory(w http.ResponseWriter, r *http.Request) {
	s.history(w, r, true)
}

func (s *server) lineHistory(w http.ResponseWriter, r *http.Request) {
	s.history(w, r, false)
}

func (s *server) history(w http.ResponseWriter, r *http.Request, isStop bool) {
	params := r.URL.Query()
	to := s.Now()
	if v := params.Get("to"); v != "" {
		t, err := parseInstant(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("to must be RFC 3339 or YYYY-MM-DD, got %q", v))
			return
		}
		to = t
	}
	from := to.Add(-7 * 24 * time.Hour)
	if v := params.Get("from"); v != "" {
		t, err := parseInstant(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("from must be RFC 3339 or YYYY-MM-DD, got %q", v))
			return
		}
		from = t
	}
	if !from.Before(to) {
		writeError(w, r, http.StatusBadRequest, "invalid_parameter", "from must be before to")
		return
	}
	if to.Sub(from) > time.Duration(s.HistoryMaxDays)*24*time.Hour {
		writeError(w, r, http.StatusBadRequest, "range_too_large", fmt.Sprintf("from to to spans more than %d days", s.HistoryMaxDays))
		return
	}
	var direction *int16
	if v := params.Get("direction"); v != "" {
		if v != "0" && v != "1" {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("direction must be 0 or 1, got %q", v))
			return
		}
		d := int16(v[0] - '0')
		direction = &d
	}
	bucket := params.Get("bucket")
	switch bucket {
	case "":
		bucket = "hour"
	case "hour", "day":
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("bucket must be hour or day, got %q", bucket))
		return
	}

	q := store.HistoryQuery{Direction: direction, From: from, To: to}
	if isStop {
		q.StopID = r.PathValue("stop_id")
		q.RouteID = params.Get("route_id")
	} else {
		q.RouteID = r.PathValue("route_id")
	}

	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()
	h, err := s.History(ctx, q)
	if err != nil {
		s.Log.Error("history query failed", "component", "api", "request_id", requestID(r.Context()), "err", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	switch {
	case !h.ScheduleLoaded:
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "no schedule version is loaded")
		return
	case !h.Known && isStop:
		writeError(w, r, http.StatusNotFound, "stop_not_found", fmt.Sprintf("no stop with id %q in the active schedule", q.StopID))
		return
	case !h.Known:
		writeError(w, r, http.StatusNotFound, "line_not_found", fmt.Sprintf("no route with id %q in the active schedule", q.RouteID))
		return
	}

	resp := historyResponse{
		Bucket:  bucket,
		From:    from.In(servicetime.Loc).Format(time.RFC3339),
		To:      to.In(servicetime.Loc).Format(time.RFC3339),
		Buckets: []historyBucket{},
	}
	if isStop {
		resp.StopID, resp.Name = q.StopID, h.Name
		if q.RouteID != "" {
			resp.RouteID = &q.RouteID
		}
	} else {
		resp.RouteID, resp.ShortName = &q.RouteID, h.Name
	}

	for _, group := range groupRows(h.Rows, bucket == "day") {
		c := combine(group)
		start := group[0].BucketStart
		if bucket == "day" {
			start = sydneyMidnight(start)
		}
		resp.Buckets = append(resp.Buckets, historyBucket{
			BucketStart: start.In(servicetime.Loc).Format(time.RFC3339),
			NObs:        c.nObs, NEarly: c.nEarly, NOnTime: c.nOnTime, NLate: c.nLate,
			NVeryLate: c.nVeryLate, NSkipped: c.nSkipped, NCancelled: c.nCancelled,
			OnTimePct: c.onTimePct(), P50: c.p50, P90: c.p90, Mean: c.mean,
		})
	}
	all := combine(h.Rows)
	resp.Totals = historyTotals{NObs: all.nObs, OnTimePct: all.onTimePct(), P50: all.p50, P90: all.p90}
	resp.Count = len(resp.Buckets)
	writeJSON(w, http.StatusOK, resp)
}

// parseInstant accepts RFC 3339, or a date meaning midnight in Sydney: the
// §7.1 example's from=2026-09-20 renders as 2026-09-20T00:00:00+10:00.
func parseInstant(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, servicetime.Loc), nil
}

// sydneyMidnight is the start of t's calendar day in Sydney. Day buckets are
// calendar days, not service days: a reader asking for "Tuesday" means the
// clock on the wall.
func sydneyMidnight(t time.Time) time.Time {
	l := t.In(servicetime.Loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, servicetime.Loc)
}

// groupRows splits rows, already in bucket_start order, into one group per
// hour or per Sydney day. Several rows share an hour when a stop is served
// by several routes or directions.
func groupRows(rows []store.HourRow, byDay bool) [][]store.HourRow {
	var out [][]store.HourRow
	var last time.Time
	for _, r := range rows {
		key := r.BucketStart
		if byDay {
			key = sydneyMidnight(key)
		}
		if len(out) == 0 || !key.Equal(last) {
			out = append(out, nil)
			last = key
		}
		out[len(out)-1] = append(out[len(out)-1], r)
	}
	return out
}

type combined struct {
	nObs, nEarly, nOnTime, nLate, nVeryLate, nSkipped, nCancelled int
	p50, p90                                                      *int32
	mean                                                          *float64
}

// known is how many visits have a delay at all; cancelled calls do not.
func (c combined) known() int { return c.nEarly + c.nOnTime + c.nLate + c.nVeryLate }

// onTimePct excludes cancellations, which are reported separately (§16 q4).
func (c combined) onTimePct() *float64 {
	if c.known() == 0 {
		return nil
	}
	return round1(100 * float64(c.nOnTime) / float64(c.known()))
}

// combine merges rollup rows. Counts sum and the mean is weighted by the
// visits that have a delay, so both are exact. Percentiles do not combine:
// from one row they are exact, and across rows they are the median of the
// rows' own percentiles, weighted the same way. That approximation was
// chosen over nulls or storing histograms (§15, 2026-09-23) and §7.1 says so.
func combine(rows []store.HourRow) combined {
	var c combined
	var sum float64
	for _, r := range rows {
		c.nObs += r.NObs
		c.nEarly += r.NEarly
		c.nOnTime += r.NOnTime
		c.nLate += r.NLate
		c.nVeryLate += r.NVeryLate
		c.nSkipped += r.NSkipped
		c.nCancelled += r.NCancelled
		if r.Mean != nil {
			sum += float64(*r.Mean) * float64(r.NEarly+r.NOnTime+r.NLate+r.NVeryLate)
		}
	}
	if c.known() > 0 {
		c.mean = round1(sum / float64(c.known()))
	}
	if len(rows) == 1 {
		c.p50, c.p90 = rows[0].P50, rows[0].P90
		return c
	}
	c.p50 = weightedMedian(rows, func(r store.HourRow) *int32 { return r.P50 })
	c.p90 = weightedMedian(rows, func(r store.HourRow) *int32 { return r.P90 })
	return c
}

func weightedMedian(rows []store.HourRow, pick func(store.HourRow) *int32) *int32 {
	type vw struct {
		v int32
		w int
	}
	var vals []vw
	total := 0
	for _, r := range rows {
		w := r.NEarly + r.NOnTime + r.NLate + r.NVeryLate
		if v := pick(r); v != nil && w > 0 {
			vals = append(vals, vw{*v, w})
			total += w
		}
	}
	if len(vals) == 0 {
		return nil
	}
	slices.SortFunc(vals, func(a, b vw) int { return int(a.v) - int(b.v) })
	acc := 0
	for _, x := range vals {
		acc += x.w
		if 2*acc >= total {
			v := x.v
			return &v
		}
	}
	return nil // unreachable: acc reaches total
}

func round1(f float64) *float64 {
	r := math.Round(f*10) / 10
	return &r
}
