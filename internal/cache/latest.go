// Package cache holds the latest state of every active trip, so "now" queries
// never touch Postgres (§4.2 component 8).
package cache

import (
	"sync"
	"time"

	"github.com/zigzaggoose/headway/internal/ingest"
)

// Trip is one active trip as of its feed's latest poll. NextStopID and DelayS
// describe the first stop the producer still reports for it, which is the
// next one: producers list stop-time updates in trip order and drop the
// stops already served.
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
	DelayS      *int32
	LastUpdate  time.Time
}

// snapshot is one feed's trips at one feed timestamp. It is never modified
// after Update publishes it, so readers use it without holding the lock.
type snapshot struct {
	feedTS  time.Time
	byRoute map[string][]Trip
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

// Update replaces feedID's trips with those in obs, one poll's worth. The
// snapshot is built before the lock is taken, so readers wait only for a
// map assignment.
func (c *Cache) Update(feedID string, feedTS time.Time, obs []ingest.Observation) {
	s := &snapshot{feedTS: feedTS, byRoute: make(map[string][]Trip)}
	type tripKey struct{ serviceDate, tripID string }
	seen := make(map[tripKey]bool)
	for _, o := range obs {
		// The first observation of a trip is its next stop; the rest are
		// further down the line.
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
			DelayS:      o.ObservedDelayS,
			LastUpdate:  o.FeedTS,
		})
	}

	c.mu.Lock()
	c.feeds[feedID] = s
	c.mu.Unlock()
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
