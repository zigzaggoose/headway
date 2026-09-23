package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zigzaggoose/headway/internal/cache"
	"github.com/zigzaggoose/headway/internal/servicetime"
)

// tripRelCanceled is TripDescriptor.CANCELED. Kept as a raw int32 like every
// schedule relationship (§9.1): 5 (REPLACEMENT) is common and not in the spec.
const tripRelCanceled = 3

type nowResponse struct {
	RouteID     string     `json:"route_id"`
	DirectionID *int       `json:"direction_id"`
	AsOf        string     `json:"as_of"`
	FeedAgeS    int64      `json:"feed_age_s"`
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

// nowTrip carries only what the feed itself supplies. Headsign, stop names,
// stop_sequence and scheduled/predicted times come from the timetable and
// join the response with the matcher in Stage 2.
type nowTrip struct {
	TripID      string  `json:"trip_id"`
	ServiceDate string  `json:"service_date"`
	VehicleID   string  `json:"vehicle_id,omitempty"`
	TripRel     int32   `json:"trip_rel"`
	Matched     bool    `json:"matched"`
	NextStop    nowStop `json:"next_stop"`
	LastUpdate  string  `json:"last_update"`
}

type nowStop struct {
	StopID string `json:"stop_id"`
	DelayS *int32 `json:"delay_s"`
	Status string `json:"status"`
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
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("limit must be 1 to 1000, got %q", v))
			return
		}
		limit = n
	}

	trips, asOf, found := s.Cache.Route(routeID)
	if !found {
		// Until a schedule is loaded, "known route" can only mean "in the live
		// feed now", so a real line with nothing running is also a 404 here.
		writeError(w, r, http.StatusNotFound, "line_not_found", fmt.Sprintf("no route with id %q in the live feed", routeID))
		return
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
		DirectionID: direction,
		AsOf:        asOf.In(servicetime.Loc).Format(time.RFC3339),
		FeedAgeS:    sinceSeconds(s.Now(), asOf),
		Summary:     s.summarise(trips),
		Trips:       make([]nowTrip, 0, min(limit, len(trips))),
	}
	for _, t := range trips[:min(limit, len(trips))] {
		resp.Trips = append(resp.Trips, nowTrip{
			TripID:      t.TripID,
			ServiceDate: t.ServiceDate.Format(time.DateOnly),
			VehicleID:   t.VehicleID,
			TripRel:     t.TripRel,
			Matched:     t.Matched,
			NextStop:    nowStop{StopID: t.NextStopID, DelayS: t.DelayS, Status: s.status(t.DelayS)},
			LastUpdate:  t.LastUpdate.In(servicetime.Loc).Format(time.RFC3339),
		})
	}
	resp.Count = len(resp.Trips)
	writeJSON(w, http.StatusOK, resp)
}

// status classifies a delay with the same boundaries the rollups use (§8),
// so /now and /history never disagree about what "late" means.
func (s *server) status(delay *int32) string {
	switch {
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
		if t.TripRel == tripRelCanceled {
			sum.Cancelled++
			continue
		}
		sum.ActiveTrips++
		switch s.status(t.DelayS) {
		case "early":
			sum.Early++
		case "on_time":
			sum.OnTime++
		case "late":
			sum.Late++
		case "very_late":
			sum.VeryLate++
		}
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
