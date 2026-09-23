package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/match"
	"github.com/zigzaggoose/headway/internal/servicetime"
)

// tripRelCanceled is TripDescriptor.CANCELED. Kept as a raw int32 like every
// schedule relationship (§9.1): 5 (REPLACEMENT) is common and not in the spec.
const tripRelCanceled = 3

type nowResponse struct {
	RouteID     string     `json:"route_id"`
	ShortName   string     `json:"short_name"`
	DirectionID *int       `json:"direction_id"`
	AsOf        *string    `json:"as_of"`
	FeedAgeS    *int64     `json:"feed_age_s"`
	Summary     nowSummary `json:"summary"`
	Trips       []nowTrip  `json:"trips"`
	Count       int        `json:"count"`
}

type nowSummary struct {
	ActiveTrips  int    `json:"active_trips"`
	Early        int    `json:"early"`
	OnTime       int    `json:"on_time"`
	Late         int    `json:"late"`
	VeryLate     int    `json:"very_late"`
	Cancelled    int    `json:"cancelled"`
	MedianDelayS *int32 `json:"median_delay_s"`
}

type nowTrip struct {
	TripID      string  `json:"trip_id"`
	ServiceDate string  `json:"service_date"`
	VehicleID   string  `json:"vehicle_id,omitempty"`
	Headsign    string  `json:"headsign,omitempty"`
	TripRel     int32   `json:"trip_rel"`
	Matched     bool    `json:"matched"`
	NextStop    nowStop `json:"next_stop"`
	LastUpdate  string  `json:"last_update"`
}

type nowStop struct {
	StopID       string  `json:"stop_id"`
	Name         string  `json:"name,omitempty"`
	StopSequence *int32  `json:"stop_sequence"`
	Scheduled    *string `json:"scheduled"`
	Predicted    *string `json:"predicted"`
	DelayS       *int32  `json:"delay_s"`
	Status       string  `json:"status"`
}

type stopNowResponse struct {
	StopID     string         `json:"stop_id"`
	Name       string         `json:"name"`
	AsOf       *string        `json:"as_of"`
	Departures []departureRow `json:"departures"`
	Count      int            `json:"count"`
}

type departureRow struct {
	RouteID     string  `json:"route_id"`
	ShortName   string  `json:"short_name"`
	TripID      string  `json:"trip_id"`
	Headsign    string  `json:"headsign,omitempty"`
	Scheduled   string  `json:"scheduled"`
	Predicted   *string `json:"predicted"`
	DelayS      *int32  `json:"delay_s"`
	Status      string  `json:"status"`
	StopTimeRel int32   `json:"stop_time_rel"`
}

type linesResponse struct {
	Lines             []lineRow `json:"lines"`
	ScheduleVersionID int64     `json:"schedule_version_id"`
	Count             int       `json:"count"`
}

type lineRow struct {
	RouteID   string `json:"route_id"`
	ShortName string `json:"short_name"`
	LongName  string `json:"long_name"`
	RouteType int16  `json:"route_type"`
	FeedID    string `json:"feed_id"`
}

// timetable is the loaded schedules for one request. They are immutable, so
// reading them needs no lock.
type timetable map[string]*match.Schedule

func (s *server) timetable() timetable {
	t := timetable{}
	for _, sch := range s.Schedules() {
		t[sch.FeedID] = sch
	}
	return t
}

func (t timetable) route(id string) (match.Route, bool) {
	for _, s := range t {
		if r, ok := s.Route(id); ok {
			return r, true
		}
	}
	return match.Route{}, false
}

func (t timetable) stopName(id string) (string, bool) {
	for _, s := range t {
		if n, ok := s.Stop(id); ok {
			return n, true
		}
	}
	return "", false
}

// call is what the timetable says about one trip's call: its headsign, and
// the scheduled departure (or arrival) on service date d. scheduled is nil
// when the trip or stop is unknown or the timetable leaves the time blank.
func (t timetable) call(feedID, tripID string, seq *int32, d time.Time) (headsign string, scheduled *time.Time) {
	sch, ok := t[feedID]
	if !ok {
		return "", nil
	}
	trip, ok := sch.Trip(tripID)
	if !ok {
		return "", nil
	}
	if seq == nil {
		return trip.Headsign, nil
	}
	for _, st := range trip.Stops {
		if st.Seq != *seq {
			continue
		}
		secs := st.DepS
		if secs == match.NoTime {
			secs = st.ArrS
		}
		if secs == match.NoTime {
			break
		}
		at := servicetime.AtServiceOffset(d, int(secs))
		return trip.Headsign, &at
	}
	return trip.Headsign, nil
}

func rfc3339(t time.Time) string { return t.In(servicetime.Loc).Format(time.RFC3339) }

func optTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := rfc3339(*t)
	return &s
}

// predicted is scheduled plus delay, when both are known.
func predicted(scheduled *time.Time, delay *int32) *time.Time {
	if scheduled == nil || delay == nil {
		return nil
	}
	p := scheduled.Add(time.Duration(*delay) * time.Second)
	return &p
}

// intParam reads an optional integer query parameter within [lo, hi].
func intParam(w http.ResponseWriter, r *http.Request, name string, def, lo, hi int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("%s must be %d to %d, got %q", name, lo, hi, v))
		return 0, false
	}
	return n, true
}

// requireSchedule answers 503 not_ready when no timetable is loaded, which
// §7.2 requires of every data endpoint.
func (s *server) requireSchedule(w http.ResponseWriter, r *http.Request) (timetable, bool) {
	t := s.timetable()
	if len(t) == 0 {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "no schedule version is loaded")
		return nil, false
	}
	return t, true
}

func (s *server) lines(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParam(w, r, "limit", 200, 1, 1000)
	if !ok {
		return
	}
	var mode *int16
	if v := r.URL.Query().Get("mode"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1702 {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("mode must be a GTFS route_type, got %q", v))
			return
		}
		m := int16(n)
		mode = &m
	}
	t, ok := s.requireSchedule(w, r)
	if !ok {
		return
	}

	resp := linesResponse{Lines: []lineRow{}}
	for feedID, sch := range t {
		// One feed today; with several, the newest version is the one worth
		// reporting, and each line carries its feed.
		resp.ScheduleVersionID = max(resp.ScheduleVersionID, sch.VersionID)
		sch.Routes(func(id string, rt match.Route) {
			if mode == nil || rt.Type == *mode {
				resp.Lines = append(resp.Lines, lineRow{RouteID: id, ShortName: rt.ShortName, LongName: rt.LongName, RouteType: rt.Type, FeedID: feedID})
			}
		})
	}
	slices.SortFunc(resp.Lines, func(a, b lineRow) int {
		return cmp.Or(cmp.Compare(a.ShortName, b.ShortName), cmp.Compare(a.RouteID, b.RouteID))
	})
	resp.Lines = resp.Lines[:min(limit, len(resp.Lines))]
	resp.Count = len(resp.Lines)
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) lineNow(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("route_id")

	var direction *int
	if v := r.URL.Query().Get("direction"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || (d != 0 && d != 1) {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("direction must be 0 or 1, got %q", v))
			return
		}
		direction = &d
	}
	limit, ok := intParam(w, r, "limit", 100, 1, 1000)
	if !ok {
		return
	}
	t, ok := s.requireSchedule(w, r)
	if !ok {
		return
	}
	route, ok := t.route(routeID)
	if !ok {
		writeError(w, r, http.StatusNotFound, "line_not_found", fmt.Sprintf("no route with id %q in the active schedule", routeID))
		return
	}

	// A known route with nothing running is 200 with no trips (§7.1).
	trips, asOf, found := s.Cache.Route(routeID)
	if !found {
		asOf = s.Cache.Newest()
	}
	if direction != nil {
		trips = slices.DeleteFunc(trips, func(t cache.Trip) bool {
			return t.DirectionID == nil || int(*t.DirectionID) != *direction
		})
	}
	slices.SortFunc(trips, func(a, b cache.Trip) int {
		return cmp.Or(a.ServiceDate.Compare(b.ServiceDate), cmp.Compare(a.TripID, b.TripID))
	})

	resp := nowResponse{
		RouteID:     routeID,
		ShortName:   route.ShortName,
		DirectionID: direction,
		Summary:     s.summarise(trips),
		Trips:       make([]nowTrip, 0, min(limit, len(trips))),
	}
	if !asOf.IsZero() {
		resp.AsOf = optTime(&asOf)
		age := sinceSeconds(s.Now(), asOf)
		resp.FeedAgeS = &age
	}
	for _, tr := range trips[:min(limit, len(trips))] {
		headsign, scheduled := t.call(tr.FeedID, tr.TripID, tr.NextStopSeq, tr.ServiceDate)
		name, _ := t.stopName(tr.NextStopID) // "" for a stop the timetable lacks, which omits the field
		resp.Trips = append(resp.Trips, nowTrip{
			TripID:      tr.TripID,
			ServiceDate: tr.ServiceDate.Format(time.DateOnly),
			VehicleID:   tr.VehicleID,
			Headsign:    headsign,
			TripRel:     tr.TripRel,
			Matched:     tr.Matched,
			NextStop: nowStop{
				StopID:       tr.NextStopID,
				Name:         name,
				StopSequence: tr.NextStopSeq,
				Scheduled:    optTime(scheduled),
				Predicted:    optTime(predicted(scheduled, tr.DelayS)),
				DelayS:       tr.DelayS,
				Status:       s.status(tr.DelayS, tr.TripRel),
			},
			LastUpdate: rfc3339(tr.LastUpdate),
		})
	}
	resp.Count = len(resp.Trips)
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) stopNow(w http.ResponseWriter, r *http.Request) {
	stopID := r.PathValue("stop_id")
	window, ok := intParam(w, r, "window_min", 60, 1, 180)
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit", 20, 1, 100)
	if !ok {
		return
	}
	t, ok := s.requireSchedule(w, r)
	if !ok {
		return
	}
	name, ok := t.stopName(stopID)
	if !ok {
		writeError(w, r, http.StatusNotFound, "stop_not_found", fmt.Sprintf("no stop with id %q in the active schedule", stopID))
		return
	}

	calls, asOf, found := s.Cache.Stop(stopID)
	if !found {
		asOf = s.Cache.Newest()
	}
	horizon := s.Now().Add(time.Duration(window) * time.Minute)

	type dep struct {
		row  departureRow
		when time.Time
	}
	var deps []dep
	for _, c := range calls {
		headsign, scheduled := t.call(c.FeedID, c.TripID, c.StopSeq, c.ServiceDate)
		// An unmatched call has no scheduled time, so it cannot be placed in
		// the window. Every call the feed still reports is one not yet
		// served, so there is no lower bound: a late train stays listed.
		if scheduled == nil {
			continue
		}
		pred := predicted(scheduled, c.DelayS)
		when := *scheduled
		if pred != nil {
			when = *pred
		}
		if when.After(horizon) {
			continue
		}
		rt, _ := t.route(c.RouteID) // an unknown route leaves short_name empty
		deps = append(deps, dep{when: when, row: departureRow{
			RouteID:     c.RouteID,
			ShortName:   rt.ShortName,
			TripID:      c.TripID,
			Headsign:    headsign,
			Scheduled:   rfc3339(*scheduled),
			Predicted:   optTime(pred),
			DelayS:      c.DelayS,
			Status:      s.status(c.DelayS, c.TripRel),
			StopTimeRel: c.StopTimeRel,
		}})
	}
	slices.SortFunc(deps, func(a, b dep) int {
		return cmp.Or(a.when.Compare(b.when), cmp.Compare(a.row.TripID, b.row.TripID))
	})

	resp := stopNowResponse{StopID: stopID, Name: name, Departures: make([]departureRow, 0, min(limit, len(deps)))}
	if !asOf.IsZero() {
		resp.AsOf = optTime(&asOf)
	}
	for _, d := range deps[:min(limit, len(deps))] {
		resp.Departures = append(resp.Departures, d.row)
	}
	resp.Count = len(resp.Departures)
	writeJSON(w, http.StatusOK, resp)
}

// status classifies a call with the same delay boundaries the rollups use
// (§8), so /now and /history never disagree about what "late" means. A
// cancelled trip is "cancelled" whatever its delay says.
func (s *server) status(delay *int32, tripRel int32) string {
	switch {
	case tripRel == tripRelCanceled:
		return "cancelled"
	case delay == nil:
		return "unknown"
	case int(*delay) < s.OnTime.EarlyS:
		return "early"
	case int(*delay) <= s.OnTime.LateS:
		return "on_time"
	case int(*delay) <= s.OnTime.VeryLateS:
		return "late"
	default:
		return "very_late"
	}
}

// summarise counts every trip on the route, before limit is applied. As in
// the §7.1 example, a cancelled trip is counted as cancelled, not as active.
func (s *server) summarise(trips []cache.Trip) nowSummary {
	var sum nowSummary
	var delays []int32
	for _, t := range trips {
		switch s.status(t.DelayS, t.TripRel) {
		case "cancelled":
			sum.Cancelled++
			continue
		case "early":
			sum.Early++
		case "on_time":
			sum.OnTime++
		case "late":
			sum.Late++
		case "very_late":
			sum.VeryLate++
		}
		sum.ActiveTrips++
		if t.DelayS != nil {
			delays = append(delays, *t.DelayS)
		}
	}
	if len(delays) > 0 {
		// The lower middle on an even count: an observed delay, not an
		// average of two that no train actually had.
		slices.Sort(delays)
		sum.MedianDelayS = &delays[(len(delays)-1)/2]
	}
	return sum
}
