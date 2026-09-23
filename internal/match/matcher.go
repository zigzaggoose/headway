// Package match turns decoded realtime updates into observations by
// resolving each against the feed's timetable (§9.1). It knows nothing about
// SQL: the store builds the Schedule, and main publishes it here.
package match

import (
	"sync/atomic"
	"time"

	"github.com/zigzaggoose/headway/internal/gtfsrt"
	"github.com/zigzaggoose/headway/internal/ingest"
	"github.com/zigzaggoose/headway/internal/servicetime"
)

// Counts says what happened to one poll's updates. The unmatched reasons are
// the labels of headway_unmatched_total (§9.1).
type Counts struct {
	Updates      int // stop-level and trip-level updates offered
	Observations int // observations returned, including synthesised ones

	FullMatch     int // orders 1 and 2: trip and stop found, scheduled time known
	TripOnly      int // order 3: trip found, stop not resolved
	UnknownTrip   int // order 4: trip not in the schedule
	Added         int // order 4 by definition; excluded from the match rate (§9.1 case 2)
	NoSchedule    int // order 4 because no schedule is loaded for the feed
	AmbiguousStop int // part of TripOnly: the trip calls at the stop more than once (§9.1 case 9)

	CancelledStops     int // observations synthesised for cancelled trips (§9.1 case 3)
	TripLevelSkipped   int // trip-level updates that are not a known cancelled trip
	NoStopID           int // unkeyable: no stop_id and no resolvable stop_sequence
	BadStartDate       int // unreadable start_date; skipped rather than guessed (§9.2 case 7)
	DelayDisagreements int // delay and time disagreed beyond tolerance; delay won (§9.5)
	UnknownStop        int // stop_id not in the timetable's stops; still stored (§9.1 case 12)
	Duplicates         int // a later update for the same key in one message replaced an earlier one (§9.1 case 14)
}

// MatchRate is the share of trip-matched updates among those that could
// have matched: ADDED trips have no timetable by definition.
func (c Counts) MatchRate() float64 {
	denom := c.FullMatch + c.TripOnly + c.UnknownTrip + c.NoSchedule
	if denom == 0 {
		return 0
	}
	return float64(c.FullMatch+c.TripOnly) / float64(denom)
}

// Options are the §8 values the matcher uses.
type Options struct {
	DayOverlap time.Duration // SERVICE_DAY_OVERLAP_H
	// DateTolerance is how far a scheduled time may be from the update for a
	// candidate service date to be accepted (SERVICE_DATE_TOLERANCE).
	DateTolerance time.Duration
	ReconcileS    int32 // DELAY_RECONCILE_TOLERANCE_S
}

// Matcher holds one schedule per feed and swaps them atomically: a match in
// flight finishes against the schedule it started with (§9.1 step 4).
type Matcher struct {
	opts      Options
	schedules map[string]*atomic.Pointer[Schedule]
	// last is each feed's most recent Counts, for /v1/admin/stats. Pollers
	// write it and the API reads it, so it is atomic like the schedules.
	last map[string]*atomic.Pointer[Counts]
}

// NewMatcher returns a matcher for a fixed set of feeds. The map is never
// written after this, so only the pointers need to be atomic.
func NewMatcher(feedIDs []string, o Options) *Matcher {
	m := &Matcher{
		opts:      o,
		schedules: make(map[string]*atomic.Pointer[Schedule], len(feedIDs)),
		last:      make(map[string]*atomic.Pointer[Counts], len(feedIDs)),
	}
	for _, id := range feedIDs {
		m.schedules[id] = new(atomic.Pointer[Schedule])
		m.last[id] = new(atomic.Pointer[Counts])
	}
	return m
}

// Swap publishes a schedule for its feed. A feed the matcher was not built
// for is ignored.
func (m *Matcher) Swap(s *Schedule) {
	if p, ok := m.schedules[s.FeedID]; ok {
		p.Store(s)
	}
}

// Schedules returns every schedule in use, for the API to read. Schedules are
// immutable, so the caller may keep and read them without a lock.
func (m *Matcher) Schedules() []*Schedule {
	var out []*Schedule
	for _, p := range m.schedules {
		if s := p.Load(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Loaded returns the version in use for a feed, or 0 when none is.
func (m *Matcher) Loaded(feedID string) int64 {
	if p, ok := m.schedules[feedID]; ok {
		if s := p.Load(); s != nil {
			return s.VersionID
		}
	}
	return 0
}

// Match resolves one poll's updates. With no schedule loaded every update
// takes the order-4 path, which is exactly what was stored before the
// matcher existed.
func (m *Matcher) Match(feedID string, updates []gtfsrt.RawUpdate) ([]ingest.Observation, Counts) {
	var sched *Schedule
	if p, ok := m.schedules[feedID]; ok {
		sched = p.Load()
	}
	c := Counts{Updates: len(updates)}
	out := make([]ingest.Observation, 0, len(updates))
	// §9.1 case 14: the same trip twice in one message means the last word
	// wins. Left to the database, ON CONFLICT DO NOTHING would keep the
	// first instead, so duplicates are resolved here, in place.
	at := make(map[ingest.Key]int, len(updates))
	add := func(o ingest.Observation) {
		if i, ok := at[o.Key()]; ok {
			out[i] = o
			c.Duplicates++
			return
		}
		at[o.Key()] = len(out)
		out = append(out, o)
	}

	for _, u := range updates {
		if sched != nil && u.StopID != "" && sched.hasStops() {
			if _, ok := sched.stops[u.StopID]; !ok {
				c.UnknownStop++
			}
		}
		cands, ok := m.candidates(u)
		if !ok {
			c.BadStartDate++
			continue
		}

		var trip *Trip
		if sched != nil {
			trip = sched.trips[u.TripID]
		}

		if trip == nil {
			if u.TripLevel {
				c.TripLevelSkipped++
				continue
			}
			if u.StopID == "" {
				c.NoStopID++
				continue
			}
			switch {
			case sched == nil:
				c.NoSchedule++
			case u.TripRel == gtfsrt.TripAdded:
				c.Added++
			default:
				c.UnknownTrip++
			}
			add(base(u, cands[0]))
			continue
		}

		if u.TripLevel {
			if u.TripRel != gtfsrt.TripCanceled {
				c.TripLevelSkipped++
				continue
			}
			// §9.1 case 3: a cancellation arrives with no stops at all, and
			// without one row per scheduled stop it never reaches a rollup.
			d := m.pickDate(sched, trip, cands, u.HeaderTS, NoTime)
			for i := range trip.Stops {
				st := &trip.Stops[i]
				o := base(u, d)
				o.StopID = st.StopID
				o.StopSequence = seqOf(st)
				o.ScheduledAt = scheduledAt(d, st)
				o.ArrivalDelayS, o.DepartureDelayS, o.ObservedDelayS = nil, nil, nil
				matched(&o, trip)
				add(o)
				c.CancelledStops++
			}
			continue
		}

		st, ambiguous := resolveStop(trip, u)
		if st == nil {
			if u.StopID == "" {
				c.NoStopID++
				continue
			}
			c.TripOnly++
			if ambiguous {
				c.AmbiguousStop++
			}
			o := base(u, m.pickDate(sched, trip, cands, reference(u), NoTime))
			matched(&o, trip)
			add(o)
			continue
		}

		c.FullMatch++
		scheduled := st.DepS
		if scheduled == NoTime {
			scheduled = st.ArrS
		}
		d := m.pickDate(sched, trip, cands, reference(u), scheduled)
		o := base(u, d)
		o.StopID = st.StopID
		o.StopSequence = seqOf(st)
		o.ScheduledAt = scheduledAt(d, st)
		matched(&o, trip)
		var dis1, dis2 bool
		o.ArrivalDelayS, dis1 = m.derive(u.ArrivalDelay, u.ArrivalTime, d, st.ArrS)
		o.DepartureDelayS, dis2 = m.derive(u.DepartureDelay, u.DepartureTime, d, st.DepS)
		if dis1 || dis2 {
			c.DelayDisagreements++
		}
		o.ObservedDelayS = observed(o.ArrivalDelayS, o.DepartureDelayS)
		add(o)
	}
	c.Observations = len(out)
	if p, ok := m.last[feedID]; ok {
		snapshot := c
		p.Store(&snapshot)
	}
	return out, c
}

// LastCounts returns each feed's counts from its most recent poll.
func (m *Matcher) LastCounts() map[string]Counts {
	out := make(map[string]Counts, len(m.last))
	for id, p := range m.last {
		if c := p.Load(); c != nil {
			out[id] = *c
		}
	}
	return out
}

// candidates is the producer's start_date when present, which §9.2 case 7
// says to trust, or else the service dates the header timestamp could be in.
func (m *Matcher) candidates(u gtfsrt.RawUpdate) ([]time.Time, bool) {
	if u.StartDate == "" {
		return servicetime.CandidateServiceDates(u.HeaderTS, m.opts.DayOverlap), true
	}
	d, err := time.Parse("20060102", u.StartDate)
	if err != nil {
		return nil, false
	}
	return []time.Time{d}, true
}

// pickDate chooses the service date for a matched trip: the first candidate,
// newest first, on which the trip's service runs and, when a scheduled time
// is known, puts that time within DateTolerance of the update. This is what
// tells a trip still running from last night from today's run of the same
// trip_id (§9.2 case 6). With no candidate passing, the first one on which
// the service runs, else the newest: a calendar that has fallen out of date
// must not stop a match by trip_id (§9.1 case 17).
func (m *Matcher) pickDate(s *Schedule, t *Trip, cands []time.Time, ref time.Time, scheduledS int32) time.Time {
	var running []time.Time
	for _, d := range cands {
		if s.RunsOn(t.ServiceID, d) {
			running = append(running, d)
		}
	}
	for _, d := range running {
		if m.near(d, t, ref, scheduledS) {
			return d
		}
	}
	if len(running) > 0 {
		return running[0]
	}
	return cands[0]
}

// near reports whether ref is within tolerance of the scheduled time on d,
// or, with no stop time to go on, of the trip's span on d.
func (m *Matcher) near(d time.Time, t *Trip, ref time.Time, scheduledS int32) bool {
	tol := m.opts.DateTolerance
	if scheduledS != NoTime {
		diff := ref.Sub(servicetime.AtServiceOffset(d, int(scheduledS)))
		return diff >= -tol && diff <= tol
	}
	first, last := span(t)
	if first == NoTime {
		return true
	}
	return !ref.Before(servicetime.AtServiceOffset(d, int(first)).Add(-tol)) &&
		!ref.After(servicetime.AtServiceOffset(d, int(last)).Add(tol))
}

// span is the trip's first and last known time, or NoTime.
func span(t *Trip) (first, last int32) {
	first, last = NoTime, NoTime
	for _, st := range t.Stops {
		for _, v := range [2]int32{st.ArrS, st.DepS} {
			if v == NoTime {
				continue
			}
			if first == NoTime || v < first {
				first = v
			}
			if v > last {
				last = v
			}
		}
	}
	return first, last
}

// resolveStop is match orders 1 and 2. ambiguous is true when order 2 fails
// only because the trip calls at the stop more than once (§9.1 case 9).
func resolveStop(t *Trip, u gtfsrt.RawUpdate) (st *StopTime, ambiguous bool) {
	if u.StopSequence != nil {
		for i := range t.Stops {
			if uint32(t.Stops[i].Seq) == *u.StopSequence {
				// A stop_sequence that names a different stop than the
				// producer's stop_id is not a match on either.
				if u.StopID == "" || u.StopID == t.Stops[i].StopID {
					return &t.Stops[i], false
				}
				break
			}
		}
	}
	if u.StopID == "" {
		return nil, false
	}
	var found *StopTime
	for i := range t.Stops {
		if t.Stops[i].StopID == u.StopID {
			if found != nil {
				return nil, true
			}
			found = &t.Stops[i]
		}
	}
	return found, false
}

// derive is one StopTimeEvent's delay (§9.1 cases 7 and 8, §9.5). The
// producer's delay wins whenever present, because it was computed against the
// producer's own timetable; the absolute time is only used to fill a missing
// delay, and only when the scheduled time is known.
func (m *Matcher) derive(delay *int32, eventTime *int64, d time.Time, scheduledS int32) (*int32, bool) {
	if eventTime == nil || scheduledS == NoTime {
		return delay, false
	}
	computed := int32(*eventTime - servicetime.AtServiceOffset(d, int(scheduledS)).Unix())
	if delay == nil {
		return &computed, false
	}
	diff := computed - *delay
	return delay, diff > m.opts.ReconcileS || diff < -m.opts.ReconcileS
}

// reference is the instant an update is about: its predicted time when the
// producer gave one, else the moment the feed was generated.
func reference(u gtfsrt.RawUpdate) time.Time {
	if u.DepartureTime != nil {
		return time.Unix(*u.DepartureTime, 0)
	}
	if u.ArrivalTime != nil {
		return time.Unix(*u.ArrivalTime, 0)
	}
	return u.HeaderTS
}

// base is the observation as the feed alone describes it: §9.1 order 4.
func base(u gtfsrt.RawUpdate, serviceDate time.Time) ingest.Observation {
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
	o.ObservedDelayS = observed(o.ArrivalDelayS, o.DepartureDelayS)
	return o
}

// matched fills what the timetable knows about the trip. The schedule's
// route and direction replace the feed's: the feed never sends a direction.
func matched(o *ingest.Observation, t *Trip) {
	o.Matched = true
	o.RouteID = t.RouteID
	if t.DirectionID != nil {
		o.DirectionID = t.DirectionID
	}
}

// scheduledAt is when the timetable has the visit on service date d: the
// departure, else the arrival, else nil for a stop the timetable leaves blank.
func scheduledAt(d time.Time, st *StopTime) *time.Time {
	s := st.DepS
	if s == NoTime {
		s = st.ArrS
	}
	if s == NoTime {
		return nil
	}
	t := servicetime.AtServiceOffset(d, int(s))
	return &t
}

// seqOf copies the sequence out, so no observation points into a schedule
// that other goroutines are reading.
func seqOf(st *StopTime) *int32 {
	seq := st.Seq
	return &seq
}

// observed is §16 q3: departure is what a boarding passenger sees.
func observed(arr, dep *int32) *int32 {
	if dep != nil {
		return dep
	}
	return arr
}
