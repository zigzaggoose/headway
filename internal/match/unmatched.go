// Package match turns decoded realtime updates into observations. Until the
// schedule loader exists (Stage 2) it has only the unmatched path of §9.1
// order 4: the delay is whatever the producer sent, and nothing is looked up.
package match

import (
	"time"

	"github.com/zigzaggoose/headway/internal/gtfsrt"
	"github.com/zigzaggoose/headway/internal/ingest"
	"github.com/zigzaggoose/headway/internal/servicetime"
)

// Skipped counts updates Unmatched could not turn into an observation.
type Skipped struct {
	// TripLevel updates are expanded into one observation per scheduled stop,
	// which needs the timetable. Until then there is no stop to key them on.
	TripLevel int
	// NoStopID is anchored by stop_sequence alone. stop_id is part of the
	// key, and only the timetable can supply it from the sequence.
	NoStopID int
	// BadStartDate is a start_date that is present but not YYYYMMDD. It is
	// not replaced with a guess: §9.2 case 7 trusts start_date, so a date
	// that cannot be read is a producer fault to count, not to paper over.
	BadStartDate int
}

// Unmatched converts updates without a schedule.
//
// The service date is the producer's start_date when it sends one. Otherwise
// it is the newest candidate for the feed timestamp; near midnight that is
// wrong for a trip still running from the previous evening, and only the
// schedule can tell the two apart (§9.2 case 6). Accepted until Stage 2.
func Unmatched(updates []gtfsrt.RawUpdate, overlap time.Duration) ([]ingest.Observation, Skipped) {
	var skipped Skipped
	out := make([]ingest.Observation, 0, len(updates))
	for _, u := range updates {
		if u.TripLevel {
			skipped.TripLevel++
			continue
		}
		if u.StopID == "" {
			skipped.NoStopID++
			continue
		}
		serviceDate := servicetime.CandidateServiceDates(u.HeaderTS, overlap)[0]
		if u.StartDate != "" {
			d, err := time.Parse("20060102", u.StartDate)
			if err != nil {
				skipped.BadStartDate++
				continue
			}
			serviceDate = d
		}

		o := ingest.Observation{
			ServiceDate:     serviceDate,
			FeedID:          u.FeedID,
			TripID:          u.TripID,
			StopID:          u.StopID,
			FeedTS:          u.HeaderTS,
			RouteID:         u.RouteID,
			ArrivalDelayS:   u.ArrivalDelay,
			DepartureDelayS: u.DepartureDelay,
			TripRel:         u.TripRel,
			StopTimeRel:     u.StopTimeRel,
			VehicleID:       u.VehicleID,
		}
		// GTFS defines only 0 and 1; anything else would be a lie in the
		// int16 column, so it is stored as unknown.
		if u.DirectionID != nil && *u.DirectionID <= 1 {
			d := int16(*u.DirectionID)
			o.DirectionID = &d
		}
		// §16 q3: departure is what a boarding passenger sees.
		if u.DepartureDelay != nil {
			o.ObservedDelayS = u.DepartureDelay
		} else {
			o.ObservedDelayS = u.ArrivalDelay
		}
		out = append(out, o)
	}
	return out, skipped
}
