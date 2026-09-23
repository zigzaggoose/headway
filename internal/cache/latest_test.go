package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/zigzaggoose/headway/internal/ingest"
)

var (
	t0    = time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	today = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
)

func ptr[T any](v T) *T { return &v }

func obs(feedID, routeID, tripID, stopID string, delay int32, ts time.Time) ingest.Observation {
	return ingest.Observation{
		ServiceDate:    today,
		FeedID:         feedID,
		TripID:         tripID,
		StopID:         stopID,
		RouteID:        routeID,
		FeedTS:         ts,
		ObservedDelayS: ptr(delay),
	}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRoute_AfterUpdate_ReturnsEachTripAtItsNextStop(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, []ingest.Observation{
		obs("trains", "R1", "trip-a", "stop-2", 60, t0),
		obs("trains", "R1", "trip-a", "stop-3", 90, t0),
		obs("trains", "R1", "trip-b", "stop-7", 0, t0),
		obs("trains", "R2", "trip-c", "stop-9", 30, t0),
	})

	trips, asOf, found := c.Route("R1")

	if !found || !asOf.Equal(t0) {
		t.Fatalf("found = %v, as of = %s; want true, %s", found, asOf, t0)
	}
	if len(trips) != 2 {
		t.Fatalf("got %d trips on R1, want 2: %+v", len(trips), trips)
	}
	byID := map[string]Trip{}
	for _, tr := range trips {
		byID[tr.TripID] = tr
	}
	if a := byID["trip-a"]; a.NextStopID != "stop-2" || *a.DelayS != 60 {
		t.Errorf("trip-a next stop = %s delay %d, want the first reported stop, stop-2 at 60", a.NextStopID, *a.DelayS)
	}
	if b := byID["trip-b"]; b.NextStopID != "stop-7" {
		t.Errorf("trip-b next stop = %s, want stop-7", b.NextStopID)
	}
}

func TestRoute_TripMissingFromTheNextPoll_IsGone(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0.Add(15*time.Second)))
	c.Update("trains", t0, []ingest.Observation{
		obs("trains", "R1", "trip-a", "stop-2", 60, t0),
		obs("trains", "R1", "trip-b", "stop-7", 0, t0),
	})
	later := t0.Add(15 * time.Second)
	c.Update("trains", later, []ingest.Observation{obs("trains", "R1", "trip-b", "stop-8", 30, later)})

	trips, _, _ := c.Route("R1")

	if len(trips) != 1 || trips[0].TripID != "trip-b" || trips[0].NextStopID != "stop-8" {
		t.Errorf("trips = %+v, want only trip-b at stop-8", trips)
	}
}

func TestRoute_Freshness_HidesFeedsOlderThanTheTTL(t *testing.T) {
	cases := []struct {
		name      string
		age       time.Duration
		wantFound bool
	}{
		{"a feed exactly at the TTL is still served", 45 * time.Minute, true},
		{"a feed past the TTL is not served", 45*time.Minute + time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := New(45*time.Minute, fixedClock(t0.Add(c.age)))
			cache.Update("trains", t0, []ingest.Observation{obs("trains", "R1", "trip-a", "stop-2", 60, t0)})
			if _, _, found := cache.Route("R1"); found != c.wantFound {
				t.Errorf("found = %v, want %v", found, c.wantFound)
			}
		})
	}
}

func TestRoute_AcrossFeeds_MergesTripsAndReportsTheOldestFeed(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0.Add(time.Minute)))
	older := t0.Add(-30 * time.Second)
	c.Update("trains", t0, []ingest.Observation{obs("trains", "R1", "trip-a", "stop-2", 60, t0)})
	c.Update("metro", older, []ingest.Observation{obs("metro", "R1", "trip-m", "stop-5", 0, older)})
	c.Update("ferries", t0, []ingest.Observation{obs("ferries", "F1", "trip-f", "stop-1", 0, t0)})

	trips, asOf, found := c.Route("R1")

	if !found || len(trips) != 2 {
		t.Fatalf("found = %v with %d trips, want 2 from two feeds", found, len(trips))
	}
	if !asOf.Equal(older) {
		t.Errorf("as of = %s, want the older feed's %s so staleness is not understated", asOf, older)
	}
}

func TestRoute_UnknownRoute_IsNotFound(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, []ingest.Observation{obs("trains", "R1", "trip-a", "stop-2", 60, t0)})
	if trips, _, found := c.Route("R9"); found || len(trips) != 0 {
		t.Errorf("found = %v with %d trips, want nothing", found, len(trips))
	}
}

func TestRoute_SameTripIDOnTwoServiceDates_IsTwoTrips(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	late := obs("trains", "R1", "trip-a", "stop-9", 120, t0)
	late.ServiceDate = today.AddDate(0, 0, -1)
	c.Update("trains", t0, []ingest.Observation{late, obs("trains", "R1", "trip-a", "stop-1", 0, t0)})

	if trips, _, _ := c.Route("R1"); len(trips) != 2 {
		t.Errorf("got %d trips, want yesterday's and today's run of trip-a", len(trips))
	}
}

func TestRoute_ReturnedSlice_DoesNotAliasTheCache(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, []ingest.Observation{obs("trains", "R1", "trip-a", "stop-2", 60, t0)})

	trips, _, _ := c.Route("R1")
	trips[0].TripID = "changed"

	if again, _, _ := c.Route("R1"); again[0].TripID != "trip-a" {
		t.Errorf("a caller's edit reached the cache: trip id = %s", again[0].TripID)
	}
}

// Pollers write while HTTP handlers read. Under -race this fails if a reader
// ever sees a snapshot that is still being built.
func TestCache_ReadsDuringUpdates_AreRaceFree(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	var wg sync.WaitGroup
	for _, feed := range []string{"trains", "metro"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				ts := t0.Add(time.Duration(i) * time.Second)
				c.Update(feed, ts, []ingest.Observation{
					obs(feed, "R1", "trip-a", "stop-2", int32(i), ts),
					obs(feed, "R1", "trip-b", "stop-3", int32(i), ts),
				})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			trips, _, found := c.Route("R1")
			if found && len(trips)%2 != 0 {
				t.Errorf("read a partial snapshot: %d trips", len(trips))
				return
			}
		}
	}()
	wg.Wait()
}
