package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/servicetime"
)

// stopTimeRelSkipped is StopTimeUpdate.SKIPPED, raw like every relationship (§9.1).
const stopTimeRelSkipped = 1

type tripResponse struct {
	TripID      string     `json:"trip_id"`
	ServiceDate string     `json:"service_date"`
	RouteID     string     `json:"route_id"`
	ShortName   string     `json:"short_name"`
	Headsign    string     `json:"headsign,omitempty"`
	Start       *tripEnd   `json:"start"`
	End         *tripEnd   `json:"end"`
	Stops       []tripStop `json:"stops"`
	Count       int        `json:"count"`
}

// tripEnd is a trip's first or last stop and when the timetable has it there.
type tripEnd struct {
	StopID    string  `json:"stop_id"`
	Name      string  `json:"name,omitempty"`
	Scheduled *string `json:"scheduled"`
}

type tripStop struct {
	StopID       string  `json:"stop_id"`
	Name         string  `json:"name,omitempty"`
	StopSequence int32   `json:"stop_sequence"`
	Scheduled    *string `json:"scheduled"`
	Predicted    *string `json:"predicted"`
	DelayS       *int32  `json:"delay_s"`
	Status       string  `json:"status"`
	Passed       bool    `json:"passed"`
}

// at is when the timetable has trip at st on service date d: departure, or
// arrival when departure is blank. nil when both are blank.
func at(st match.StopTime, d time.Time) *time.Time {
	secs := st.DepS
	if secs == match.NoTime {
		secs = st.ArrS
	}
	if secs == match.NoTime {
		return nil
	}
	t := servicetime.AtServiceOffset(d, int(secs))
	return &t
}

// ends is a trip's first and last stop, nil for a trip the timetable lacks.
func (t timetable) ends(feedID, tripID string, d time.Time) (start, end *tripEnd) {
	sch, ok := t[feedID]
	if !ok {
		return nil, nil
	}
	trip, ok := sch.Trip(tripID)
	if !ok || len(trip.Stops) == 0 {
		return nil, nil
	}
	end1 := func(st match.StopTime) *tripEnd {
		name, _ := t.stopName(st.StopID) // "" omits the field
		return &tripEnd{StopID: st.StopID, Name: name, Scheduled: optTime(at(st, d))}
	}
	return end1(trip.Stops[0]), end1(trip.Stops[len(trip.Stops)-1])
}

// trip is every stop of one trip in timetable order, each with the last
// delay the feed gave for it: final at a stop the train has left, predicted
// at one it has not reached. It reads observations, so unlike /now it sees
// the stops a trip has already served.
func (s *server) trip(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("trip_id")
	v := r.URL.Query().Get("service_date")
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_parameter", fmt.Sprintf("service_date must be YYYY-MM-DD, got %q", v))
		return
	}
	t, ok := s.requireSchedule(w, r)
	if !ok {
		return
	}
	var feedID string
	var trip *match.Trip
	for id, sch := range t {
		if tr, ok := sch.Trip(tripID); ok {
			feedID, trip = id, tr
			break
		}
	}
	if trip == nil {
		writeError(w, r, http.StatusNotFound, "trip_not_found", fmt.Sprintf("no trip with id %q in the active schedule", tripID))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()
	seen, err := s.TripStops(ctx, d, feedID, tripID)
	if err != nil {
		s.Log.Error("trip query failed", "component", "api", "request_id", requestID(r.Context()), "err", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	rt, _ := t.route(trip.RouteID) // a trip's route is in the same bundle
	resp := tripResponse{
		TripID:      tripID,
		ServiceDate: d.Format(time.DateOnly),
		RouteID:     trip.RouteID,
		ShortName:   rt.ShortName,
		Headsign:    trip.Headsign,
		Stops:       make([]tripStop, 0, len(trip.Stops)),
	}
	resp.Start, resp.End = t.ends(feedID, tripID, d)
	now := s.Now()
	for _, st := range trip.Stops {
		name, _ := t.stopName(st.StopID)
		scheduled := at(st, d)
		row := tripStop{StopID: st.StopID, Name: name, StopSequence: st.Seq, Scheduled: optTime(scheduled), Status: "unknown"}
		if o, ok := seen[st.StopID]; ok {
			row.DelayS = o.DelayS
			row.Status = s.status(o.DelayS, o.TripRel)
			if o.StopTimeRel == stopTimeRelSkipped && row.Status != "cancelled" {
				row.Status = "skipped"
			}
		}
		p := predicted(scheduled, row.DelayS)
		row.Predicted = optTime(p)
		if p == nil {
			p = scheduled
		}
		row.Passed = p != nil && p.Before(now)
		resp.Stops = append(resp.Stops, row)
	}
	resp.Count = len(resp.Stops)
	writeJSON(w, http.StatusOK, resp)
}
