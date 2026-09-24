// Package gtfsrt turns a feed body into flattened updates. Every
// field-presence check in Transit Late Again lives here: the generated protobuf structs
// are full of pointers, and no other package is allowed to call GetX() on one.
// It knows nothing about the timetable, the database, or what "late" means.
// PROJECT.md §4.2 (3).
package gtfsrt

import "time"

// Schedule-relationship values are stored and compared as raw integers, never
// as the generated enum type. TfNSW has historically emitted REPLACEMENT,
// which the current specification removed, and a producer may emit a value
// nobody has seen yet; an exhaustive switch on a typed enum would either
// refuse to compile against it or quietly bucket it as something else. §15.
//
// These constants are names for the integers, not a closed set: code reads
// them, and nothing rejects a value that is missing from the list.
const (
	TripScheduled   int32 = 0
	TripAdded       int32 = 1
	TripUnscheduled int32 = 2
	TripCanceled    int32 = 3 // one L, as the specification spells it
	TripReplacement int32 = 5 // removed from the specification; still emitted in the wild
	TripDuplicated  int32 = 6
	TripDeleted     int32 = 7
)

const (
	StopScheduled   int32 = 0
	StopSkipped     int32 = 1
	StopNoData      int32 = 2
	StopUnscheduled int32 = 3
)

// RawUpdate is one StopTimeUpdate flattened with its parent TripDescriptor.
// Every pointer from the protobuf has already been dereferenced or defaulted;
// a nil pointer here means "the producer did not send this field", which is
// information the matcher needs and must not be flattened away into a zero.
type RawUpdate struct {
	FeedID    string
	HeaderTS  time.Time // FeedHeader.timestamp, feed-supplied
	FetchedAt time.Time // our clock, when the body arrived
	EntityID  string

	TripID      string // "" when the producer omitted it
	RouteID     string // "" when the producer omitted it
	StartDate   string // "YYYYMMDD" from TripDescriptor, "" when absent
	StartTime   string // "HH:MM:SS" from TripDescriptor, "" when absent
	DirectionID *uint32
	VehicleID   string
	TripRel     int32 // raw TripDescriptor.schedule_relationship

	// TripLevel marks an update that describes the whole trip rather than one
	// of its stops: a cancellation that arrives with no stop-time updates at
	// all. Without it a cancelled trip would decode to nothing and be invisible
	// in the rollup (§9.1 case 3). The matcher expands one of these into an
	// observation per scheduled stop.
	TripLevel bool

	StopSequence   *uint32
	StopID         string
	StopTimeRel    int32 // raw StopTimeUpdate.schedule_relationship
	ArrivalDelay   *int32
	ArrivalTime    *int64 // Unix seconds
	DepartureDelay *int32
	DepartureTime  *int64
	Uncertainty    *int32
}

// DropReason labels an update the decoder refused. The reasons are metric
// labels (§10.3 transitlateagain_updates_dropped_total{reason}), so they are a small
// closed set of lowercase identifiers.
type DropReason string

const (
	// DropNotTripUpdate is an entity carrying a vehicle position or an alert.
	// Transit Late Again ingests trip updates only (§2), so these are expected, not bad.
	DropNotTripUpdate DropReason = "not_trip_update"

	// DropNoTripID is a trip update with no trip_id. The observation key is
	// (service_date, feed_id, trip_id, stop_sequence, feed_ts); with no trip
	// id, unrelated trips would collide on that key.
	DropNoTripID DropReason = "no_trip_id"

	// DropNoStopAnchor is a stop-time update naming neither a stop_sequence
	// nor a stop_id. Nothing can ever resolve it to a stop.
	DropNoStopAnchor DropReason = "no_stop_anchor"

	// DropNoTime is a stop-time update with no arrival, no departure, and
	// nothing else to say: a plain SCHEDULED stop on a running trip carries no
	// information without a time. A SKIPPED or NO_DATA update, or any update
	// on a cancelled trip, is kept — the relationship is the information.
	DropNoTime DropReason = "no_time"
)

// DecodedFeed is one decoded body.
type DecodedFeed struct {
	FeedID    string
	HeaderTS  time.Time
	FetchedAt time.Time

	// HeaderTSMissing records that the producer sent no header timestamp and
	// HeaderTS is our fetch time instead. Idempotency degrades to per-fetch for
	// that response, and §9.5 case 1 wants it counted.
	HeaderTSMissing bool

	Updates  []RawUpdate
	Entities int

	// Dropped is keyed by reason because the metric is labelled by reason. A
	// single count would have to be re-derived to be useful.
	Dropped map[DropReason]int
}

// TotalDropped is every refused update, whatever the reason.
func (d DecodedFeed) TotalDropped() int {
	var n int
	for _, c := range d.Dropped {
		n += c
	}
	return n
}

// Age is how stale the data was when it arrived: the first half of the
// freshness lag, before anything of ours queues it.
func (d DecodedFeed) Age() time.Duration { return d.FetchedAt.Sub(d.HeaderTS) }
