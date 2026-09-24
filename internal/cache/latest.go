// Package cache holds the latest state of every active trip, so "now" queries
// never touch Postgres (§4.2 component 8).
package cache

import (
	"sync"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/ingest"
)

// Trip is one active trip as of its feed's latest poll. NextStopID and DelayS
// describe the first stop the producer reports for it that has not been
// served (see Update).
type Trip struct {
	FeedID      string
	TripID      string
	RouteID     string
	ServiceDate time.Time
	DirectionID *int16
	VehicleID   string
	TripRel     int32
	Matched     bool
	NextStopID  string
	NextStopSeq *int32 // from the timetable; nil when unmatched
	DelayS      *int32
	LastUpdate  time.Time
}

// Call is one trip's reported call at one stop that has not been served.
type Call struct {
	FeedID      string
	TripID      string
	RouteID     string
	ServiceDate time.Time
	StopSeq     *int32 // from the timetable; nil when unmatched
	DelayS      *int32
	StopTimeRel int32
	TripRel     int32
	Matched     bool
}

// snapshot is one feed's trips at one feed timestamp. It is never modified
// after Update publishes it, so readers use it without holding the lock.
type snapshot struct {
	feedTS  time.Time
	byRoute map[string][]Trip
	byStop  map[string][]Call
	// unchanged counts the polls in a row, before this one, whose header
	// timestamp this one did not advance past (§9.5 staleness).
	unchanged int
}

// Cache is safe for concurrent use: pollers update it, HTTP handlers read it.
//
// Each poll replaces its feed's snapshot wholesale rather than updating trips
// one by one. A trip missing from the latest poll has finished or been
// withdrawn, and per-entry expiry would keep showing it for CACHE_TTL; it
// would also need a sweep goroutine. The TTL applies to whole feeds instead,
// so a feed that stops updating disappears rather than being served stale.
type Cache struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.RWMutex
	feeds map[string]*snapshot
}

// New returns an empty cache.
func New(ttl time.Duration, now func() time.Time) *Cache {
	return &Cache{ttl: ttl, now: now, feeds: make(map[string]*snapshot)}
}

// servedAfter is how far behind the feed's own timestamp a stop's predicted
// time must fall before the stop counts as served. The TfNSW producers keep
// finished trips, and some keep a trip's served stops ahead of the next one
// (§9.1 case 18). Five minutes because that is where the live data splits:
// trips whose first listed stop really was next sat 0–5 minutes past it (27
// of them), with only 5 in the next five minutes; ten let a trip show the
// stop it had just left (58 of 643 did).
const servedAfter = 5 * time.Minute

// Update replaces feedID's trips with those in obs, one poll's worth. The
// snapshot is built before the lock is taken, so readers wait only for a
// map assignment.
//
// Served stops are dropped, so a trip's next stop is its first unserved one
// and a trip with none left is not active. The reference is feedTS, never
// our clock, so a replay builds the same snapshot. An unmatched observation
// has no scheduled time to judge by and is kept.
func (c *Cache) Update(feedID string, feedTS time.Time, obs []ingest.Observation) {
	s := &snapshot{feedTS: feedTS, byRoute: make(map[string][]Trip), byStop: make(map[string][]Call)}
	type tripKey struct{ serviceDate, tripID string }
	seen := make(map[tripKey]bool)
	for _, o := range obs {
		if served(o, feedTS) {
			continue
		}
		s.byStop[o.StopID] = append(s.byStop[o.StopID], Call{
			FeedID:      o.FeedID,
			TripID:      o.TripID,
			RouteID:     o.RouteID,
			ServiceDate: o.ServiceDate,
			StopSeq:     o.StopSequence,
			DelayS:      o.ObservedDelayS,
			StopTimeRel: o.StopTimeRel,
			TripRel:     o.TripRel,
			Matched:     o.Matched,
		})
		// The first unserved observation of a trip is its next stop; the
		// rest are further down the line.
		k := tripKey{o.ServiceDate.Format(time.DateOnly), o.TripID}
		if seen[k] {
			continue
		}
		seen[k] = true
		s.byRoute[o.RouteID] = append(s.byRoute[o.RouteID], Trip{
			FeedID:      o.FeedID,
			TripID:      o.TripID,
			RouteID:     o.RouteID,
			ServiceDate: o.ServiceDate,
			DirectionID: o.DirectionID,
			VehicleID:   o.VehicleID,
			TripRel:     o.TripRel,
			Matched:     o.Matched,
			NextStopID:  o.StopID,
			NextStopSeq: o.StopSequence,
			DelayS:      o.ObservedDelayS,
			LastUpdate:  o.FeedTS,
		})
	}

	c.mu.Lock()
	if prev := c.feeds[feedID]; prev != nil && !feedTS.After(prev.feedTS) {
		s.unchanged = prev.unchanged + 1
	}
	c.feeds[feedID] = s
	c.mu.Unlock()
}

// Stale reports whether feedID's header timestamp has failed to advance for
// polls successful polls in a row (FEED_STALE_POLLS). A producer that keeps
// answering 200 with the same message is up, and serving nothing new.
func (c *Cache) Stale(feedID string, polls int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.feeds[feedID]
	return s != nil && s.unchanged >= polls
}

func served(o ingest.Observation, feedTS time.Time) bool {
	if o.ScheduledAt == nil {
		return false
	}
	at := *o.ScheduledAt
	if o.ObservedDelayS != nil {
		at = at.Add(time.Duration(*o.ObservedDelayS) * time.Second)
	}
	return feedTS.Sub(at) > servedAfter
}

// Route returns the active trips on routeID across every feed that is still
// fresh, and the oldest feed timestamp among the feeds that have the route,
// which is what makes the answer as stale as it is. found is false when no
// fresh feed reports the route at all.
//
// The returned slice is the caller's to keep.
func (c *Cache) Route(routeID string) (trips []Trip, asOf time.Time, found bool) {
	c.mu.RLock()
	snaps := make([]*snapshot, 0, len(c.feeds))
	for _, s := range c.feeds {
		snaps = append(snaps, s)
	}
	c.mu.RUnlock()

	now := c.now()
	for _, s := range snaps {
		if now.Sub(s.feedTS) > c.ttl {
			continue
		}
		ts, ok := s.byRoute[routeID]
		if !ok {
			continue
		}
		trips = append(trips, ts...)
		if !found || s.feedTS.Before(asOf) {
			asOf = s.feedTS
		}
		found = true
	}
	return trips, asOf, found
}

// Stop returns the calls at stopID across every fresh feed, and the oldest
// feed timestamp among the feeds that report the stop. found is false when no
// fresh feed does. The returned slice is the caller's to keep.
func (c *Cache) Stop(stopID string) (calls []Call, asOf time.Time, found bool) {
	c.mu.RLock()
	snaps := make([]*snapshot, 0, len(c.feeds))
	for _, s := range c.feeds {
		snaps = append(snaps, s)
	}
	c.mu.RUnlock()

	now := c.now()
	for _, s := range snaps {
		if now.Sub(s.feedTS) > c.ttl {
			continue
		}
		cs, ok := s.byStop[stopID]
		if !ok {
			continue
		}
		calls = append(calls, cs...)
		if !found || s.feedTS.Before(asOf) {
			asOf = s.feedTS
		}
		found = true
	}
	return calls, asOf, found
}

// Feeds returns each feed's latest snapshot timestamp.
func (c *Cache) Feeds() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]time.Time, len(c.feeds))
	for id, s := range c.feeds {
		out[id] = s.feedTS
	}
	return out
}

// Newest returns the most recent feed timestamp across all feeds, or the zero
// time before the first poll lands. Readiness uses it: a snapshot exists only
// after a fetch and a decode have both succeeded.
func (c *Cache) Newest() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var newest time.Time
	for _, s := range c.feeds {
		if s.feedTS.After(newest) {
			newest = s.feedTS
		}
	}
	return newest
}
