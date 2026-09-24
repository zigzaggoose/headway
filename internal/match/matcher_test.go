package match

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/zigzaggoose/transitlateagain/internal/gtfsrt"
	"github.com/zigzaggoose/transitlateagain/internal/ingest"
	"github.com/zigzaggoose/transitlateagain/internal/servicetime"
)

const feedID = "sydneytrains"

var opts = Options{DayOverlap: 6 * time.Hour, DateTolerance: 6 * time.Hour, ReconcileS: 60}

var (
	aest = time.FixedZone("AEST", 10*3600)
	// 2026-09-23 is a Wednesday.
	wed = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	tue = time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
)

func ptr[T any](v T) *T { return &v }

// at is a Sydney wall-clock instant on 2026-09-23 (or the 24th past 24:00).
func at(h, m int) time.Time { return time.Date(2026, 9, 23, h, m, 0, 0, aest) }

// schedule has one route and these trips, all on service SVC, which runs
// every day from 2026-09-21 to 2026-09-30:
//
//	T-1  08:00 stop A (seq 1), 08:10 stop B (seq 2), 08:20 stop C (seq 3)
//	T-N  23:50 stop A, 25:10 stop B          (runs past midnight)
//	LOOP 09:00 stop A, 09:10 stop B, 09:20 stop A   (visits A twice)
//	WKD  12:00 stop A, on service WEEKDAYS (Mon–Fri only)
func schedule() *Schedule {
	s := NewSchedule(41, feedID)
	s.AddCalendar("SVC", [7]bool{true, true, true, true, true, true, true}, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	s.AddCalendar("WEEKDAYS", [7]bool{true, true, true, true, true, false, false}, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	add := func(trip, svc string, stops ...StopTime) {
		s.AddTrip(trip, Trip{RouteID: "APS_1a", ServiceID: svc, DirectionID: ptr(int16(1)), Headsign: "Macarthur"})
		for _, st := range stops {
			s.AddStopTime(trip, st)
		}
	}
	hm := func(h, m int) int32 { return int32(h*3600 + m*60) }
	add("T-1", "SVC", StopTime{1, "A", hm(8, 0), hm(8, 0)}, StopTime{2, "B", hm(8, 10), hm(8, 10)}, StopTime{3, "C", hm(8, 20), NoTime})
	add("T-N", "SVC", StopTime{1, "A", hm(23, 50), hm(23, 50)}, StopTime{2, "B", hm(25, 10), hm(25, 10)})
	add("LOOP", "SVC", StopTime{1, "A", hm(9, 0), hm(9, 0)}, StopTime{2, "B", hm(9, 10), hm(9, 10)}, StopTime{3, "A", hm(9, 20), hm(9, 20)})
	add("WKD", "WEEKDAYS", StopTime{1, "A", hm(12, 0), hm(12, 0)})
	return s
}

func matcher(withSchedule bool) *Matcher {
	m := NewMatcher([]string{feedID}, opts)
	if withSchedule {
		m.Swap(schedule())
	}
	return m
}

func update(trip, stop string, header time.Time) gtfsrt.RawUpdate {
	return gtfsrt.RawUpdate{FeedID: feedID, HeaderTS: header.UTC(), TripID: trip, RouteID: "FEED_ROUTE", StopID: stop}
}

func one(t *testing.T, m *Matcher, u gtfsrt.RawUpdate) (ingest.Observation, Counts) {
	t.Helper()
	obs, c := m.Match(feedID, []gtfsrt.RawUpdate{u})
	if len(obs) != 1 {
		t.Fatalf("got %d observations (counts %+v), want 1", len(obs), c)
	}
	return obs[0], c
}

func day(o ingest.Observation) string { return o.ServiceDate.Format(time.DateOnly) }

// --- No schedule loaded: §9.1 order 4 for everything ---

func TestMatch_NoSchedule_StoresWhatTheFeedSays(t *testing.T) {
	m := matcher(false)
	u := update("T-1", "B", at(8, 5))
	u.DepartureDelay = ptr(int32(90))
	u.DirectionID = ptr(uint32(0))

	o, c := one(t, m, u)

	if o.Matched || o.RouteID != "FEED_ROUTE" || o.StopSequence != nil {
		t.Errorf("observation = %+v, want the feed's own values, unmatched", o)
	}
	if *o.ObservedDelayS != 90 || *o.DirectionID != 0 || day(o) != "2026-09-23" {
		t.Errorf("delay %d, direction %d, date %s", *o.ObservedDelayS, *o.DirectionID, day(o))
	}
	if c.NoSchedule != 1 || c.MatchRate() != 0 {
		t.Errorf("counts = %+v", c)
	}
}

func TestMatch_UnkeyableUpdates_AreSkippedAndCounted(t *testing.T) {
	for _, withSchedule := range []bool{false, true} {
		m := matcher(withSchedule)
		tripLevel := update("NOPE", "", at(8, 0))
		tripLevel.TripLevel = true
		noStop := update("NOPE", "", at(8, 0))
		noStop.StopSequence = ptr(uint32(4))
		badDate := update("T-1", "A", at(8, 0))
		badDate.StartDate = "2026-09-23"

		obs, c := m.Match(feedID, []gtfsrt.RawUpdate{tripLevel, noStop, badDate})

		if len(obs) != 0 || c.TripLevelSkipped != 1 || c.NoStopID != 1 || c.BadStartDate != 1 {
			t.Errorf("schedule=%v: %d observations, counts %+v", withSchedule, len(obs), c)
		}
	}
}

func TestMatch_RawFields_PassThroughUnchanged(t *testing.T) {
	m := matcher(true)
	t.Run("a direction GTFS does not define is stored as unknown", func(t *testing.T) {
		u := update("UNKNOWN", "A", at(8, 0))
		u.DirectionID = ptr(uint32(7))
		if o, _ := one(t, m, u); o.DirectionID != nil {
			t.Errorf("direction = %d, want nil", *o.DirectionID)
		}
	})
	t.Run("an unknown schedule relationship is stored, not rejected (§9.1 case 5)", func(t *testing.T) {
		u := update("T-1", "A", at(8, 0))
		u.TripRel = 5
		if o, _ := one(t, m, u); o.TripRel != 5 || !o.Matched {
			t.Errorf("trip rel = %d matched %v, want 5, true", o.TripRel, o.Matched)
		}
	})
	t.Run("feed_ts is the header timestamp, never the fetch time", func(t *testing.T) {
		u := update("T-1", "A", at(8, 0))
		u.FetchedAt = u.HeaderTS.Add(3 * time.Second)
		if o, _ := one(t, m, u); !o.FeedTS.Equal(u.HeaderTS) {
			t.Errorf("feed ts = %s, want %s", o.FeedTS, u.HeaderTS)
		}
	})
}

// --- Resolution orders, §9.1 ---

func TestMatch_ResolutionOrder_FirstHitWins(t *testing.T) {
	m := matcher(true)
	// The order label of transitlateagain_matched_total, which must name
	// exactly one order per matched update.
	order := func(n string) func(Counts) int {
		return func(c Counts) int {
			matched, _, _ := c.Labels()
			if matched["1"]+matched["2"]+matched["3"] != 1 {
				return -1
			}
			return matched[n]
		}
	}
	cases := []struct {
		name      string
		u         func() gtfsrt.RawUpdate
		wantSeq   *int32
		wantStop  string
		wantMatch bool
		count     func(Counts) int
	}{
		{"order 1: trip and stop_sequence", func() gtfsrt.RawUpdate {
			u := update("T-1", "", at(8, 5))
			u.StopSequence = ptr(uint32(2))
			return u
		}, ptr(int32(2)), "B", true, order("1")},
		{"order 2: trip and a stop it visits once", func() gtfsrt.RawUpdate {
			return update("T-1", "C", at(8, 15))
		}, ptr(int32(3)), "C", true, order("2")},
		{"order 2 after order 1 misses: the feed is ahead of the bundle (§9.1 case 11)", func() gtfsrt.RawUpdate {
			u := update("T-1", "B", at(8, 5))
			u.StopSequence = ptr(uint32(99))
			return u
		}, ptr(int32(2)), "B", true, order("2")},
		{"order 3: trip known, stop not on it", func() gtfsrt.RawUpdate {
			return update("T-1", "Z", at(8, 5))
		}, nil, "Z", true, order("3")},
		{"order 3: a loop visiting the stop twice is ambiguous (§9.1 case 9)", func() gtfsrt.RawUpdate {
			return update("LOOP", "A", at(9, 5))
		}, nil, "A", true, func(c Counts) int { return c.AmbiguousStop }},
		{"order 4: trip not in the schedule (§9.1 case 1)", func() gtfsrt.RawUpdate {
			return update("NOPE", "A", at(8, 0))
		}, nil, "A", false, func(c Counts) int { return c.UnknownTrip }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, counts := one(t, m, c.u())
			if o.Matched != c.wantMatch || o.StopID != c.wantStop {
				t.Errorf("matched %v stop %q, want %v %q", o.Matched, o.StopID, c.wantMatch, c.wantStop)
			}
			if (o.StopSequence == nil) != (c.wantSeq == nil) || (o.StopSequence != nil && *o.StopSequence != *c.wantSeq) {
				t.Errorf("stop_sequence = %v, want %v", o.StopSequence, c.wantSeq)
			}
			if c.count(counts) != 1 {
				t.Errorf("counts = %+v", counts)
			}
			if c.wantMatch && (o.RouteID != "APS_1a" || o.DirectionID == nil || *o.DirectionID != 1) {
				t.Errorf("route %q direction %v, want the timetable's APS_1a and 1", o.RouteID, o.DirectionID)
			}
		})
	}
}

func TestMatch_Added_IsUnmatchedAndOutsideTheMatchRate(t *testing.T) {
	m := matcher(true)
	added := update("EXTRA", "A", at(8, 0))
	added.TripRel = gtfsrt.TripAdded
	obs, c := m.Match(feedID, []gtfsrt.RawUpdate{added, update("T-1", "A", at(8, 0))})

	if obs[0].Matched || c.Added != 1 {
		t.Errorf("added trip matched %v, counts %+v", obs[0].Matched, c)
	}
	if c.MatchRate() != 1 {
		t.Errorf("match rate = %v, want 1: an ADDED trip must not count against it (§9.1 case 2)", c.MatchRate())
	}
}

// --- Cancellations, §9.1 cases 3 and 4 ---

func TestMatch_CancelledTrip_WithNoStops_SynthesisesOnePerScheduledStop(t *testing.T) {
	m := matcher(true)
	u := update("T-1", "", at(7, 30))
	u.TripLevel, u.TripRel = true, gtfsrt.TripCanceled

	obs, c := m.Match(feedID, []gtfsrt.RawUpdate{u})

	if len(obs) != 3 || c.CancelledStops != 3 {
		t.Fatalf("got %d observations, counts %+v; want one per scheduled stop", len(obs), c)
	}
	for i, o := range obs {
		if o.TripRel != gtfsrt.TripCanceled || o.ObservedDelayS != nil || !o.Matched || *o.StopSequence != int32(i+1) {
			t.Errorf("stop %d = %+v, want trip_rel 3, no delay, matched, seq %d", i, o, i+1)
		}
	}
	if obs[0].StopID != "A" || obs[2].StopID != "C" || day(obs[0]) != "2026-09-23" {
		t.Errorf("stops %s..%s on %s", obs[0].StopID, obs[2].StopID, day(obs[0]))
	}
}

func TestMatch_CancelledTrip_WithStops_UsesThemAndKeepsTheRelationship(t *testing.T) {
	m := matcher(true)
	u := update("T-1", "B", at(7, 30))
	u.TripRel = gtfsrt.TripCanceled
	if o, c := one(t, m, u); o.TripRel != gtfsrt.TripCanceled || c.FullMatch != 1 {
		t.Errorf("trip rel %d, counts %+v", o.TripRel, c)
	}
}

// --- Delay derivation, §9.1 cases 7 and 8, §9.5 ---

func TestMatch_Delay_DerivedFromTimeOnlyWhenTheProducerGaveNone(t *testing.T) {
	m := matcher(true)
	scheduledB := at(8, 10).Unix()
	cases := []struct {
		name         string
		delay        *int32
		time         *int64
		trip, stop   string
		want         *int32
		disagreement int
	}{
		{"time only: delay is time minus scheduled (case 7)", nil, ptr(scheduledB + 150), "T-1", "B", ptr(int32(150)), 0},
		{"delay only: used directly (case 8)", ptr(int32(40)), nil, "T-1", "B", ptr(int32(40)), 0},
		{"both, agreeing: delay", ptr(int32(40)), ptr(scheduledB + 70), "T-1", "B", ptr(int32(40)), 0},
		{"both, disagreeing past the tolerance: delay wins, counted", ptr(int32(40)), ptr(scheduledB + 400), "T-1", "B", ptr(int32(40)), 1},
		{"time only, but the stop is unresolved: unknown", nil, ptr(scheduledB + 150), "T-1", "Z", nil, 0},
		{"time only, but the trip is unknown: unknown", nil, ptr(scheduledB + 150), "NOPE", "B", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := update(c.trip, c.stop, at(8, 5))
			u.DepartureDelay, u.DepartureTime = c.delay, c.time
			o, counts := one(t, m, u)
			if (o.ObservedDelayS == nil) != (c.want == nil) || (c.want != nil && *o.ObservedDelayS != *c.want) {
				t.Errorf("observed delay = %v, want %v", deref(o.ObservedDelayS), deref(c.want))
			}
			if counts.DelayDisagreements != c.disagreement {
				t.Errorf("disagreements = %d, want %d", counts.DelayDisagreements, c.disagreement)
			}
		})
	}
}

// The time-derived delay has to use the service day's real start, so a trip
// past midnight is measured from yesterday's start, not today's.
func TestMatch_Delay_PastMidnightIsMeasuredFromTheRightServiceDay(t *testing.T) {
	m := matcher(true)
	sched := servicetime.AtServiceOffset(tue, 25*3600+10*60) // 01:10 on the 23rd
	u := update("T-N", "B", sched.Add(-5*time.Minute))
	u.ArrivalTime = ptr(sched.Unix() + 120)

	o, _ := one(t, m, u)

	if day(o) != "2026-09-22" || o.ObservedDelayS == nil || *o.ObservedDelayS != 120 {
		t.Errorf("date %s delay %v, want 2026-09-22 and 120", day(o), deref(o.ObservedDelayS))
	}
}

// --- Service date selection, §9.2 ---

func TestMatch_ServiceDate_ChosenByTheTimetable(t *testing.T) {
	m := matcher(true)
	cases := []struct {
		name string
		u    gtfsrt.RawUpdate
		want string
	}{
		{"00:30 update for a trip that began 23:50 last night is yesterday's (§9.2 case 6)", update("T-N", "B", at(0, 30)), "2026-09-22"},
		{"00:30 update for a trip that starts at 08:00 is today's", update("T-1", "A", at(0, 30)), "2026-09-23"},
		{"23:45 update for tonight's 23:50 departure is today's", update("T-N", "A", at(23, 45)), "2026-09-23"},
		{"start_date, when sent, is trusted (§9.2 case 7)", func() gtfsrt.RawUpdate {
			u := update("T-1", "A", at(8, 0))
			u.StartDate = "20260921"
			return u
		}(), "2026-09-21"},
		{"a trip-only match uses the trip's span", update("T-N", "Z", at(0, 30)), "2026-09-22"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if o, _ := one(t, m, c.u); day(o) != c.want {
				t.Errorf("service date = %s, want %s", day(o), c.want)
			}
		})
	}
}

func TestMatch_ServiceDate_SkipsDaysTheServiceDoesNotRun(t *testing.T) {
	m := matcher(true)
	// 00:30 on Saturday the 26th: candidates are Sat and Fri. WKD runs Fri
	// only, so Friday wins even though nothing about the time decides it.
	sat := time.Date(2026, 9, 26, 0, 30, 0, 0, aest)
	if o, _ := one(t, m, update("WKD", "A", sat)); day(o) != "2026-09-25" {
		t.Errorf("service date = %s, want Friday 2026-09-25", day(o))
	}
}

// §9.1 case 17: a calendar that no longer covers today must not stop a match
// by trip_id.
func TestMatch_CalendarOutOfDate_StillMatchesByTrip(t *testing.T) {
	m := matcher(true)
	later := time.Date(2026, 11, 2, 8, 0, 0, 0, aest)
	o, c := one(t, m, update("T-1", "A", later))
	if !o.Matched || c.FullMatch != 1 || day(o) != "2026-11-02" {
		t.Errorf("matched %v on %s, counts %+v", o.Matched, day(o), c)
	}
}

func TestSchedule_RunsOn_ExceptionsOverrideTheWeek(t *testing.T) {
	s := schedule()
	s.AddCalendarDate("WEEKDAYS", wed, 2)
	s.AddCalendarDate("WEEKDAYS", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), 1)
	s.AddCalendarDate("ONLY_DATES", tue, 1)
	cases := []struct {
		name, svc string
		d         time.Time
		want      bool
	}{
		{"a weekday in range runs", "WEEKDAYS", tue, true},
		{"a removed date does not", "WEEKDAYS", wed, false},
		{"an added Sunday does", "WEEKDAYS", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), true},
		{"a Saturday does not", "WEEKDAYS", time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), false},
		{"after end_date does not", "SVC", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false},
		{"a service only in calendar_dates runs on its date", "ONLY_DATES", tue, true},
		{"and not on others", "ONLY_DATES", wed, false},
		{"an unknown service does not", "NOPE", wed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.RunsOn(c.svc, c.d); got != c.want {
				t.Errorf("RunsOn(%s, %s) = %v", c.svc, c.d.Format(time.DateOnly), got)
			}
		})
	}
	if share := s.ActiveShare(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); share != 0 {
		t.Errorf("active share past the calendar = %v, want 0", share)
	}
}

func TestSchedule_StopIDs_AreInterned(t *testing.T) {
	s := NewSchedule(1, feedID)
	s.AddTrip("a", Trip{})
	s.AddTrip("b", Trip{})
	s.AddStopTime("a", StopTime{Seq: 1, StopID: string([]byte("2000341"))})
	s.AddStopTime("b", StopTime{Seq: 1, StopID: string([]byte("2000341"))})
	a, _ := s.Trip("a")
	b, _ := s.Trip("b")
	if unsafeData(a.Stops[0].StopID) != unsafeData(b.Stops[0].StopID) {
		t.Error("two trips calling at the same stop hold separate copies of its id")
	}
	if s.AddStopTime("missing", StopTime{}) {
		t.Error("a stop time for an unknown trip was accepted")
	}
}

// --- Concurrency and the real feed ---

// Pollers match while the schedule loader swaps. Under -race this fails if
// the swap is not atomic.
func TestMatcher_SwapDuringMatch_IsRaceFree(t *testing.T) {
	m := matcher(true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			m.Swap(schedule())
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if _, c := m.Match(feedID, []gtfsrt.RawUpdate{update("T-1", "A", at(8, 0))}); c.FullMatch != 1 {
				t.Errorf("counts %+v", c)
				return
			}
		}
	}()
	wg.Wait()
	if m.Loaded(feedID) != 41 || m.Loaded("other") != 0 {
		t.Errorf("loaded = %d / %d", m.Loaded(feedID), m.Loaded("other"))
	}
}

// With no schedule, a real response must give every keyable update a
// distinct key; a collision is a row silently lost to ON CONFLICT.
func TestMatch_RecordedResponse_EveryKeyableUpdateHasADistinctKey(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sydneytrains_tripupdate_0001.pb"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoded, err := gtfsrt.Decode(feedID, body, time.Date(2026, 9, 21, 2, 38, 26, 0, time.UTC))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	obs, c := matcher(false).Match(feedID, decoded.Updates)

	if got := c.Observations + c.TripLevelSkipped + c.NoStopID + c.BadStartDate; got != len(decoded.Updates) {
		t.Fatalf("%d observations + skipped (%+v) != %d updates", c.Observations, c, len(decoded.Updates))
	}
	seen := make(map[ingest.Key]bool, len(obs))
	for _, o := range obs {
		if seen[o.Key()] {
			t.Fatalf("duplicate key %+v", o.Key())
		}
		seen[o.Key()] = true
	}
}

func deref(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

func unsafeData(s string) *byte { return unsafe.StringData(s) }

func TestMatch_RemainingEdgeCases(t *testing.T) {
	t.Run("gaps in the stops sent are not interpolated (§9.1 case 10)", func(t *testing.T) {
		m := matcher(true)
		obs, c := m.Match(feedID, []gtfsrt.RawUpdate{update("T-1", "A", at(7, 50)), update("T-1", "C", at(7, 50))})
		if len(obs) != 2 || c.FullMatch != 2 {
			t.Errorf("got %d observations, counts %+v; want exactly the two stops sent, nothing for B", len(obs), c)
		}
	})
	t.Run("a stop_id missing from stops.txt is still stored, and counted (§9.1 case 12)", func(t *testing.T) {
		s := schedule()
		s.AddStop("A", "Alpha")
		m := NewMatcher([]string{feedID}, opts)
		m.Swap(s)
		o, c := one(t, m, update("T-1", "Z", at(8, 5)))
		if o.StopID != "Z" || c.UnknownStop != 1 {
			t.Errorf("stop %q, counts %+v", o.StopID, c)
		}
		if _, c := one(t, m, update("T-1", "A", at(8, 0))); c.UnknownStop != 0 {
			t.Errorf("a known stop was counted unknown: %+v", c)
		}
	})
	t.Run("no direction anywhere is stored as unknown (§9.1 case 13)", func(t *testing.T) {
		s := NewSchedule(1, feedID)
		s.AddTrip("T", Trip{RouteID: "R", ServiceID: "X"})
		s.AddStopTime("T", StopTime{1, "A", 3600, 3600})
		m := NewMatcher([]string{feedID}, opts)
		m.Swap(s)
		if o, _ := one(t, m, update("T", "A", at(1, 0))); o.DirectionID != nil {
			t.Errorf("direction = %d, want nil (the rollup turns it into -1)", *o.DirectionID)
		}
	})
	t.Run("the same stop twice in one message: the last one wins (§9.1 case 14)", func(t *testing.T) {
		m := matcher(true)
		first, last := update("T-1", "B", at(8, 5)), update("T-1", "B", at(8, 5))
		first.DepartureDelay, last.DepartureDelay = ptr(int32(30)), ptr(int32(240))
		obs, c := m.Match(feedID, []gtfsrt.RawUpdate{first, update("T-1", "A", at(8, 5)), last})
		if len(obs) != 2 || c.Duplicates != 1 {
			t.Fatalf("got %d observations, counts %+v", len(obs), c)
		}
		if obs[0].StopID != "B" || *obs[0].ObservedDelayS != 240 {
			t.Errorf("kept %s at %d, want B at the later 240", obs[0].StopID, *obs[0].ObservedDelayS)
		}
	})
	t.Run("one poll is matched against one schedule, even across a swap (§9.1 case 15)", func(t *testing.T) {
		m := matcher(true)
		ups := make([]gtfsrt.RawUpdate, 500)
		for i := range ups {
			ups[i] = update("T-1", "A", at(8, 0))
			ups[i].TripID = "T-1"
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Swap(NewSchedule(99, feedID)) // an empty schedule: nothing would match
		}()
		_, c := m.Match(feedID, ups)
		<-done
		if c.FullMatch != 0 && c.UnknownTrip != 0 {
			t.Errorf("one poll saw both schedules: %+v", c)
		}
	})
}

// /v1/admin/stats reads the last counts while pollers write them.
func TestMatcher_LastCounts_ReadWhileMatchingIsRaceFree(t *testing.T) {
	m := matcher(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			m.Match(feedID, []gtfsrt.RawUpdate{update("T-1", "A", at(8, 0))})
		}
	}()
	for range 200 {
		_ = m.LastCounts()
	}
	<-done
	if c := m.LastCounts()[feedID]; c.FullMatch != 1 || c.Updates != 1 {
		t.Errorf("last counts = %+v, want the final poll's", c)
	}
}

// The rollup buckets by ScheduledAt (§16 q12), so it must be the visit's
// timetable time on the chosen service date, and only when the stop resolved.
func TestMatch_ScheduledAt_IsTheTimetableTimeOnTheServiceDate(t *testing.T) {
	m := matcher(true)
	t.Run("a full match carries the departure time", func(t *testing.T) {
		o, _ := one(t, m, update("T-1", "B", at(8, 5)))
		if o.ScheduledAt == nil || !o.ScheduledAt.Equal(at(8, 10)) {
			t.Errorf("scheduled_at = %v, want 08:10", o.ScheduledAt)
		}
	})
	t.Run("a stop with no departure falls back to its arrival", func(t *testing.T) {
		o, _ := one(t, m, update("T-1", "C", at(8, 15)))
		if o.ScheduledAt == nil || !o.ScheduledAt.Equal(at(8, 20)) {
			t.Errorf("scheduled_at = %v, want 08:20", o.ScheduledAt)
		}
	})
	t.Run("past midnight it is on the next calendar day", func(t *testing.T) {
		o, _ := one(t, m, update("T-N", "B", at(0, 30)))
		if want := at(1, 10); o.ScheduledAt == nil || !o.ScheduledAt.Equal(want) {
			t.Errorf("scheduled_at = %v, want %s (25:10 on the 22nd)", o.ScheduledAt, want)
		}
	})
	t.Run("trip-only and unmatched have none", func(t *testing.T) {
		for _, u := range []gtfsrt.RawUpdate{update("T-1", "Z", at(8, 5)), update("NOPE", "A", at(8, 5))} {
			if o, _ := one(t, m, u); o.ScheduledAt != nil {
				t.Errorf("%s at %s: scheduled_at = %v, want nil", u.TripID, u.StopID, o.ScheduledAt)
			}
		}
	})
	t.Run("each synthesised cancellation carries its own stop's time", func(t *testing.T) {
		u := update("T-1", "", at(7, 30))
		u.TripLevel, u.TripRel = true, gtfsrt.TripCanceled
		obs, _ := m.Match(feedID, []gtfsrt.RawUpdate{u})
		if len(obs) != 3 || !obs[0].ScheduledAt.Equal(at(8, 0)) || !obs[2].ScheduledAt.Equal(at(8, 20)) {
			t.Errorf("cancelled stops scheduled at %v .. %v", obs[0].ScheduledAt, obs[len(obs)-1].ScheduledAt)
		}
	})
}

// §12 Stage 3: a worker pool between decode and match is built only if the
// poller goroutine is the bottleneck. This is the whole of a poll's work on
// the goroutine for the largest enabled feed, against its 15 s budget.
func BenchmarkPoll_DecodeAndMatch_RecordedTrains(b *testing.B) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sydneytrains_tripupdate_0001.pb"))
	if err != nil {
		b.Fatalf("read fixture: %v", err)
	}
	m := matcher(true)
	b.ReportAllocs()
	for b.Loop() {
		d, err := gtfsrt.Decode(feedID, body, time.Date(2026, 9, 21, 2, 38, 26, 0, time.UTC))
		if err != nil {
			b.Fatal(err)
		}
		m.Match(feedID, d.Updates)
	}
}
