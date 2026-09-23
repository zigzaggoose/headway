// Package ingest turns observations into rows: it suppresses the ones that say
// nothing new, buffers the rest, and writes them in batches. It is the only
// backpressure point in the design, and it drops rather than blocks (§9.4).
package ingest

import "time"

// Observation is Headway's unit of stored data: one delay measurement for one
// stop of one trip at one feed timestamp.
//
// A nil pointer means the producer did not supply the value, which is not the
// same as zero: a delay of 0 is on time, and direction 0 is a real direction.
type Observation struct {
	ServiceDate time.Time // a date in the Australia/Sydney service-day sense (§9.2)
	FeedID      string
	TripID      string
	StopID      string
	FeedTS      time.Time // the feed's own header timestamp, never our clock

	// StopSequence can only come from the timetable: the TfNSW feeds do not
	// send it (§9.1). Nil on an unmatched observation, which is why it is not
	// part of the key.
	StopSequence *int32

	RouteID     string // "" when unmatched
	DirectionID *int16

	ArrivalDelayS   *int32
	DepartureDelayS *int32
	// ObservedDelayS is the one number every query uses (§9.4, §16 q3).
	ObservedDelayS *int32

	TripRel     int32
	StopTimeRel int32
	Matched     bool
	VehicleID   string

	// ScheduledAt is when the timetable has this visit, set when the matcher
	// resolves the stop. The rollup buckets by it; nil falls back to FeedTS.
	ScheduledAt *time.Time
}

// Key identifies the thing being observed, independent of when it was
// observed. It is the change filter's key and, with feed_ts, the row's
// identity in the database.
//
// ServiceDate is held as a formatted date rather than a time.Time because a
// time.Time carries a location pointer and a monotonic reading: two values
// naming the same day can compare unequal, which in a map key is a bug that
// only shows up as a filter that stops suppressing.
type Key struct {
	ServiceDate string
	FeedID      string
	TripID      string
	StopID      string
}

// Key returns the observation's identity.
func (o Observation) Key() Key {
	return Key{
		ServiceDate: o.ServiceDate.Format(time.DateOnly),
		FeedID:      o.FeedID,
		TripID:      o.TripID,
		StopID:      o.StopID,
	}
}
