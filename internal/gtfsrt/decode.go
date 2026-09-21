package gtfsrt

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// Decode turns one feed body into flattened updates.
//
// It is deliberately permissive: an update is dropped only when it could never
// become an observation, never because it looks unusual. Deciding that an
// update is uninteresting is the matcher's job, and an unmatched observation is
// the signal that tells us the matcher is drifting (§9.1).
func Decode(feedID string, body []byte, fetchedAt time.Time) (DecodedFeed, error) {
	var msg gtfs.FeedMessage
	// AllowPartial tolerates a missing proto2 required field. GTFS-realtime
	// marks FeedMessage.header and TripUpdate.trip required, so without this
	// one malformed entity makes the whole response undecodable and costs
	// every other update in it — thousands of them. Absence is handled field by
	// field below; a wire-format error still fails, as it must.
	if err := (proto.UnmarshalOptions{AllowPartial: true}).Unmarshal(body, &msg); err != nil {
		// A gateway error page served with a 200 lands here. Log the prefix as
		// hex, never as a string: it is arbitrary bytes, and a terminal will
		// happily interpret some of them (§9.5 case 5).
		return DecodedFeed{}, fmt.Errorf("decode feed %s (%d bytes, prefix %s): %w",
			feedID, len(body), hexPrefix(body, 64), err)
	}

	out := DecodedFeed{
		FeedID:    feedID,
		FetchedAt: fetchedAt,
		Entities:  len(msg.GetEntity()),
		Dropped:   make(map[DropReason]int),
	}

	// A zero timestamp is absent, not the epoch. Falling back to our clock
	// keeps the feed usable; the flag is what tells the metric that this
	// response's idempotency key is weaker than the others (§9.5 case 1).
	if ts := msg.GetHeader().GetTimestamp(); ts == 0 {
		out.HeaderTS = fetchedAt
		out.HeaderTSMissing = true
	} else {
		out.HeaderTS = time.Unix(int64(ts), 0).UTC()
	}

	for _, entity := range msg.GetEntity() {
		tu := entity.GetTripUpdate()
		if tu == nil {
			// Vehicle positions and alerts. Expected: Headway ingests trip
			// updates only, and the feed may carry others.
			out.Dropped[DropNotTripUpdate]++
			continue
		}

		base := RawUpdate{
			FeedID:      feedID,
			HeaderTS:    out.HeaderTS,
			FetchedAt:   fetchedAt,
			EntityID:    entity.GetId(),
			TripID:      tu.GetTrip().GetTripId(),
			RouteID:     tu.GetTrip().GetRouteId(),
			StartDate:   tu.GetTrip().GetStartDate(),
			StartTime:   tu.GetTrip().GetStartTime(),
			DirectionID: optUint32(tu.GetTrip(), func(t *gtfs.TripDescriptor) *uint32 { return t.DirectionId }),
			VehicleID:   tu.GetVehicle().GetId(),
			TripRel:     int32(tu.GetTrip().GetScheduleRelationship()),
		}

		if base.TripID == "" {
			out.Dropped[DropNoTripID]++
			continue
		}

		stus := tu.GetStopTimeUpdate()
		if len(stus) == 0 {
			// A cancellation usually arrives exactly like this. Emitting a
			// trip-level update is what makes it visible to the rollup; the
			// matcher expands it against the schedule.
			u := base
			u.TripLevel = true
			out.Updates = append(out.Updates, u)
			continue
		}

		for _, stu := range stus {
			u := base
			u.StopSequence = stu.StopSequence
			u.StopID = stu.GetStopId()
			u.StopTimeRel = int32(stu.GetScheduleRelationship())

			arrival, departure := stu.GetArrival(), stu.GetDeparture()
			if arrival != nil {
				u.ArrivalDelay = arrival.Delay
				u.ArrivalTime = arrival.Time
				u.Uncertainty = arrival.Uncertainty
			}
			if departure != nil {
				u.DepartureDelay = departure.Delay
				u.DepartureTime = departure.Time
				if u.Uncertainty == nil {
					u.Uncertainty = departure.Uncertainty
				}
			}

			if u.StopSequence == nil && u.StopID == "" {
				out.Dropped[DropNoStopAnchor]++
				continue
			}
			if arrival == nil && departure == nil && !carriesMeaningWithoutATime(u) {
				out.Dropped[DropNoTime]++
				continue
			}
			out.Updates = append(out.Updates, u)
		}
	}

	return out, nil
}

// carriesMeaningWithoutATime reports whether an update with no arrival and no
// departure is still worth storing.
//
// A plain SCHEDULED stop on a running trip is not: it says only that a stop
// exists, which the timetable already said. A SKIPPED stop is, and so is any
// stop on a cancelled or deleted trip — there the relationship is the whole
// point, and dropping it would make n_skipped and n_cancelled permanently zero
// in the rollup.
func carriesMeaningWithoutATime(u RawUpdate) bool {
	switch u.StopTimeRel {
	case StopSkipped, StopNoData, StopUnscheduled:
		return true
	}
	switch u.TripRel {
	case TripCanceled, TripDeleted, TripAdded, TripReplacement, TripDuplicated:
		return true
	}
	return false
}

// optUint32 reaches an optional scalar without dereferencing a nil parent. The
// generated getters flatten absence to zero, and zero is a legal direction id.
func optUint32(t *gtfs.TripDescriptor, get func(*gtfs.TripDescriptor) *uint32) *uint32 {
	if t == nil {
		return nil
	}
	return get(t)
}

func hexPrefix(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return hex.EncodeToString(b)
}
