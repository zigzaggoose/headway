package gtfsrt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

var fetchedAt = time.Date(2026, 9, 21, 2, 38, 26, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// The recorded response is the contract: if a future change to this package
// stops decoding it, the feed has moved or the decoder has broken.
func TestDecode_RecordedResponse(t *testing.T) {
	got, err := Decode("sydneytrains", fixture(t, "sydneytrains_tripupdate_0001.pb"), fetchedAt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Entities == 0 || len(got.Updates) == 0 {
		t.Fatalf("decoded %d entities into %d updates", got.Entities, len(got.Updates))
	}
	if got.HeaderTSMissing {
		t.Error("the recorded response has a header timestamp; it was reported missing")
	}
	if got.HeaderTS.IsZero() || got.HeaderTS.Year() != 2026 {
		t.Errorf("header timestamp = %s", got.HeaderTS)
	}
	if got.FeedID != "sydneytrains" {
		t.Errorf("feed id = %q", got.FeedID)
	}

	// Every update must carry the two things that make it storable at all.
	for i, u := range got.Updates {
		if u.TripID == "" {
			t.Fatalf("update %d has no trip id but was not dropped", i)
		}
		if !u.TripLevel && u.StopID == "" && u.StopSequence == nil {
			t.Fatalf("update %d has no stop anchor but was not dropped", i)
		}
	}
}

// Measured against the live feed on 2026-09-21 and recorded in §9.1. These
// assertions are about the shape of the producer's data, not our code: if one
// fails, the feed has changed and the matcher's assumptions need revisiting.
func TestDecode_RecordedResponse_MatchesTheDocumentedFeedShape(t *testing.T) {
	got, err := Decode("sydneytrains", fixture(t, "sydneytrains_tripupdate_0001.pb"), fetchedAt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var withSeq, withVehicle, withDirection, replacement, tripLevel int
	for _, u := range got.Updates {
		if u.StopSequence != nil {
			withSeq++
		}
		if u.VehicleID != "" {
			withVehicle++
		}
		if u.DirectionID != nil {
			withDirection++
		}
		if u.TripRel == TripReplacement {
			replacement++
		}
		if u.TripLevel {
			tripLevel++
		}
	}

	// The feed never sends stop_sequence, which makes trip_id + stop_id the
	// primary match path rather than the fallback (§9.1).
	if withSeq != 0 {
		t.Errorf("%d updates carry stop_sequence; the feed was documented as never sending it", withSeq)
	}
	// The VehicleDescriptor is present but empty, so vehicle_id is never known.
	if withVehicle != 0 {
		t.Errorf("%d updates carry a vehicle id; the feed was documented as never sending one", withVehicle)
	}
	if withDirection != 0 {
		t.Errorf("%d updates carry a direction id; the feed was documented as never sending one", withDirection)
	}
	// REPLACEMENT is routine here, not exotic: roughly a fifth of updates.
	if replacement == 0 {
		t.Error("no REPLACEMENT relationships; §9.1 records them as a large minority of this feed")
	}
	if tripLevel == 0 {
		t.Error("no trip-level updates; the feed was documented as sending trips with no stop-time updates")
	}
}

// Two consecutive captures are the change filter's justification: nearly every
// update repeats unchanged fifteen seconds later.
func TestDecode_ConsecutiveCaptures_AreMostlyIdentical(t *testing.T) {
	first, err := Decode("sydneytrains", fixture(t, "sydneytrains_tripupdate_0001.pb"), fetchedAt)
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	second, err := Decode("sydneytrains", fixture(t, "sydneytrains_tripupdate_0002.pb"), fetchedAt.Add(15*time.Second))
	if err != nil {
		t.Fatalf("decode second: %v", err)
	}

	if !second.HeaderTS.After(first.HeaderTS) {
		t.Errorf("the second capture's header (%s) does not advance past the first (%s)",
			second.HeaderTS, first.HeaderTS)
	}

	type key struct{ trip, stop string }
	type value struct {
		arrival, departure int32
		hasArr, hasDep     bool
		stopRel, tripRel   int32
	}
	valueOf := func(u RawUpdate) value {
		v := value{stopRel: u.StopTimeRel, tripRel: u.TripRel}
		if u.ArrivalDelay != nil {
			v.arrival, v.hasArr = *u.ArrivalDelay, true
		}
		if u.DepartureDelay != nil {
			v.departure, v.hasDep = *u.DepartureDelay, true
		}
		return v
	}

	prev := make(map[key]value, len(first.Updates))
	for _, u := range first.Updates {
		prev[key{u.TripID, u.StopID}] = valueOf(u)
	}
	var same, total int
	for _, u := range second.Updates {
		old, ok := prev[key{u.TripID, u.StopID}]
		if !ok {
			continue
		}
		total++
		if old == valueOf(u) {
			same++
		}
	}
	if total == 0 {
		t.Fatal("no keys in common between two captures fifteen seconds apart")
	}
	// Printed rather than only asserted: this ratio is the change filter's
	// entire justification, and §13 wants a measured number rather than an
	// estimate. Run with -v to read it.
	t.Logf("%d of %d updates (%.1f%%) repeated unchanged across one 15s poll",
		same, total, 100*float64(same)/float64(total))

	if ratio := float64(same) / float64(total); ratio < 0.5 {
		t.Errorf("only %.1f%% of updates repeated unchanged (%d of %d); the change filter's premise needs re-measuring",
			100*ratio, same, total)
	}
}

func TestDecode_DerivedFixtures(t *testing.T) {
	t.Run("a cancelled trip with no stop-time updates becomes a trip-level update", func(t *testing.T) {
		got, err := Decode("sydneytrains", fixture(t, "sydneytrains_cancelled.pb"), fetchedAt)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var tripLevel, cancelled int
		for _, u := range got.Updates {
			if u.TripLevel {
				tripLevel++
			}
			if u.TripRel == TripCanceled {
				cancelled++
			}
		}
		if tripLevel == 0 {
			t.Error("a cancellation with no stop-time updates decoded to nothing; it would be invisible in the rollup")
		}
		if cancelled == 0 {
			t.Error("no update carries trip_rel = 3")
		}
	})

	t.Run("an added trip decodes and keeps its relationship", func(t *testing.T) {
		got, err := Decode("sydneytrains", fixture(t, "sydneytrains_added.pb"), fetchedAt)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var added bool
		for _, u := range got.Updates {
			if u.TripRel == TripAdded {
				added = true
			}
		}
		if !added {
			t.Error("no update carries trip_rel = 1")
		}
	})

	t.Run("a start time past 24:00:00 survives as text", func(t *testing.T) {
		got, err := Decode("sydneytrains", fixture(t, "sydneytrains_pastmidnight.pb"), fetchedAt)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var found bool
		for _, u := range got.Updates {
			if u.StartTime == "25:10:00" {
				found = true
			}
		}
		if !found {
			// Parsing it is servicetime's job; mangling it here would hide the
			// case before that package ever sees it.
			t.Error(`no update carries StartTime "25:10:00"`)
		}
	})
}

// §11.2: a truncated payload must return an error and must not panic.
func TestDecode_TruncatedPayload_ErrorsWithoutPanicking(t *testing.T) {
	_, err := Decode("sydneytrains", fixture(t, "malformed_truncated.pb"), fetchedAt)
	if err == nil {
		t.Fatal("a truncated payload decoded successfully")
	}
	if !strings.Contains(err.Error(), "sydneytrains") || !strings.Contains(err.Error(), "200 bytes") {
		t.Errorf("error should name the feed and the size: %v", err)
	}
	// §9.5 case 5: the prefix is logged as hex, never as a string.
	if !strings.Contains(err.Error(), "prefix 0a") {
		t.Errorf("error should carry a hex prefix: %v", err)
	}
}

func TestDecode_Garbage(t *testing.T) {
	cases := map[string][]byte{
		"an html error page served with a 200": []byte("<html><body>502 Bad Gateway</body></html>"),
		"random bytes":                         {0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode("sydneytrains", body, fetchedAt); err == nil {
				t.Error("decoded successfully")
			}
		})
	}
}

// An empty body is a valid protobuf message with no fields, so it decodes to
// an empty feed rather than an error. The poller has already rejected it
// before this point (§9.5 case 4); this asserts the decoder does not panic on
// the shape.
func TestDecode_EmptyBody_IsAnEmptyFeedNotAPanic(t *testing.T) {
	got, err := Decode("sydneytrains", nil, fetchedAt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Updates) != 0 || got.Entities != 0 {
		t.Errorf("decoded %d updates from nothing", len(got.Updates))
	}
	if !got.HeaderTSMissing || !got.HeaderTS.Equal(fetchedAt) {
		t.Errorf("a missing header timestamp must fall back to our clock, got %s missing=%v", got.HeaderTS, got.HeaderTSMissing)
	}
}

// Everything below is built by hand: these shapes are legal, rare, and exactly
// what a nil dereference would be hiding in.
func build(t *testing.T, entities ...*gtfs.FeedEntity) []byte {
	t.Helper()
	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{
			GtfsRealtimeVersion: proto.String("2.0"),
			Timestamp:           proto.Uint64(uint64(fetchedAt.Unix())),
		},
		Entity: entities,
	}
	// AllowPartial so the test can build the malformed shapes a producer might
	// send: GTFS-realtime is proto2 and marks some of these fields required.
	b, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestDecode_SparseMessages_DoNotPanic(t *testing.T) {
	cases := []struct {
		name         string
		entity       *gtfs.FeedEntity
		wantUpdates  int
		wantDropped  DropReason
		wantDropping bool
	}{
		{
			name:         "an entity with no trip update at all",
			entity:       &gtfs.FeedEntity{Id: proto.String("e1")},
			wantDropped:  DropNotTripUpdate,
			wantDropping: true,
		},
		{
			// GTFS-realtime marks TripUpdate.trip required, so a well-behaved
			// producer cannot send this. The decoder handles it anyway: with
			// AllowPartial a malformed entity now reaches us instead of
			// destroying the whole response.
			name:         "a trip update with no trip descriptor",
			entity:       &gtfs.FeedEntity{Id: proto.String("e2"), TripUpdate: &gtfs.TripUpdate{}},
			wantDropped:  DropNoTripID,
			wantDropping: true,
		},
		{
			name: "a trip descriptor with no trip id",
			entity: &gtfs.FeedEntity{Id: proto.String("e3"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{RouteId: proto.String("T1")},
			}},
			wantDropped:  DropNoTripID,
			wantDropping: true,
		},
		{
			name: "a stop-time update with no stop anchor",
			entity: &gtfs.FeedEntity{Id: proto.String("e4"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{Arrival: &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(60)}},
				},
			}},
			wantDropped:  DropNoStopAnchor,
			wantDropping: true,
		},
		{
			name: "a scheduled stop with neither arrival nor departure says nothing (§9.1 case 6)",
			entity: &gtfs.FeedEntity{Id: proto.String("e5"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{StopId: proto.String("2000341")},
				},
			}},
			wantDropped:  DropNoTime,
			wantDropping: true,
		},
		{
			name: "a skipped stop with no times is kept: the relationship is the information",
			entity: &gtfs.FeedEntity{Id: proto.String("e6"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{
						StopId:               proto.String("2000341"),
						ScheduleRelationship: gtfs.TripUpdate_StopTimeUpdate_SKIPPED.Enum(),
					},
				},
			}},
			wantUpdates: 1,
		},
		{
			name: "a stop on a cancelled trip with no times is kept",
			entity: &gtfs.FeedEntity{Id: proto.String("e7"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{
					TripId:               proto.String("trip-1"),
					ScheduleRelationship: gtfs.TripDescriptor_CANCELED.Enum(),
				},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{{StopId: proto.String("2000341")}},
			}},
			wantUpdates: 1,
		},
		{
			name: "a trip with no stop-time updates becomes one trip-level update",
			entity: &gtfs.FeedEntity{Id: proto.String("e8"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{
					TripId:               proto.String("trip-1"),
					ScheduleRelationship: gtfs.TripDescriptor_CANCELED.Enum(),
				},
			}},
			wantUpdates: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode("sydneytrains", build(t, tc.entity), fetchedAt)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Updates) != tc.wantUpdates {
				t.Errorf("%d updates, want %d", len(got.Updates), tc.wantUpdates)
			}
			if tc.wantDropping && got.Dropped[tc.wantDropped] != 1 {
				t.Errorf("dropped = %v, want one %s", got.Dropped, tc.wantDropped)
			}
		})
	}
}

// A direction id of 0 is a real direction, not an absent field. Flattening the
// pointer would silently label every southbound trip northbound.
func TestDecode_DirectionZero_IsNotMistakenForAbsent(t *testing.T) {
	withZero := &gtfs.FeedEntity{Id: proto.String("e1"), TripUpdate: &gtfs.TripUpdate{
		Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1"), DirectionId: proto.Uint32(0)},
		StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
			{StopId: proto.String("s1"), Departure: &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(30)}},
		},
	}}
	absent := &gtfs.FeedEntity{Id: proto.String("e2"), TripUpdate: &gtfs.TripUpdate{
		Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-2")},
		StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
			{StopId: proto.String("s1"), Departure: &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(30)}},
		},
	}}

	got, err := Decode("sydneytrains", build(t, withZero, absent), fetchedAt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Updates) != 2 {
		t.Fatalf("%d updates, want 2", len(got.Updates))
	}
	if got.Updates[0].DirectionID == nil || *got.Updates[0].DirectionID != 0 {
		t.Errorf("direction 0 came through as %v", got.Updates[0].DirectionID)
	}
	if got.Updates[1].DirectionID != nil {
		t.Errorf("absent direction came through as %v", *got.Updates[1].DirectionID)
	}
}

// A delay of 0 means on time. Losing the difference between that and "no delay
// reported" would turn every silent stop into a punctual one.
func TestDecode_ZeroDelay_IsNotMistakenForAbsent(t *testing.T) {
	e := &gtfs.FeedEntity{Id: proto.String("e1"), TripUpdate: &gtfs.TripUpdate{
		Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1")},
		StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{{
			StopId:    proto.String("s1"),
			Arrival:   &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(0)},
			Departure: &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(1789955444)},
		}},
	}}
	got, err := Decode("sydneytrains", build(t, e), fetchedAt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	u := got.Updates[0]
	if u.ArrivalDelay == nil || *u.ArrivalDelay != 0 {
		t.Errorf("arrival delay = %v, want a pointer to 0", u.ArrivalDelay)
	}
	if u.DepartureDelay != nil {
		t.Errorf("departure delay = %v, want nil: the producer sent a time, not a delay", *u.DepartureDelay)
	}
	if u.DepartureTime == nil || *u.DepartureTime != 1789955444 {
		t.Errorf("departure time = %v", u.DepartureTime)
	}
}
func TestDecode_OneMalformedEntity_DoesNotCostTheRest(t *testing.T) {
	good := &gtfs.FeedEntity{Id: proto.String("good"), TripUpdate: &gtfs.TripUpdate{
		Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-1")},
		StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
			{StopId: proto.String("s1"), Departure: &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(60)}},
		},
	}}
	bad := &gtfs.FeedEntity{Id: proto.String("bad"), TripUpdate: &gtfs.TripUpdate{}}

	got, err := Decode("sydneytrains", build(t, bad, good, good), fetchedAt)
	if err != nil {
		t.Fatalf("one malformed entity made the whole response undecodable: %v", err)
	}
	if len(got.Updates) != 2 {
		t.Errorf("%d updates survived, want 2", len(got.Updates))
	}
	if got.Dropped[DropNoTripID] != 1 {
		t.Errorf("dropped = %v", got.Dropped)
	}
}
