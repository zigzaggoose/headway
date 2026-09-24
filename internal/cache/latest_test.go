package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/ingest"
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

func TestNewest_AcrossFeeds_IsTheLatestFeedTimestamp(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	if got := c.Newest(); !got.IsZero() {
		t.Fatalf("empty cache: newest = %s, want zero", got)
	}
	c.Update("trains", t0, nil)
	c.Update("metro", t0.Add(-time.Minute), nil)
	if got := c.Newest(); !got.Equal(t0) {
		t.Errorf("newest = %s, want %s", got, t0)
	}
}

// §9.1 case 18: the producers keep finished trips, and served stops ahead of
// the next one.
func TestUpdate_ServedStops_AreNotNextStopsOrCalls(t *testing.T) {
	at := func(tripID, stopID string, scheduled time.Duration, delay int32) ingest.Observation {
		o := obs("trains", "R1", tripID, stopID, delay, t0)
		o.ScheduledAt = ptr(t0.Add(scheduled))
		return o
	}
	unmatched := obs("trains", "R1", "trip-u", "stop-1", 0, t0)
	unmatched.ObservedDelayS = nil
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, []ingest.Observation{
		at("trip-a", "stop-1", -40*time.Minute, 0),
		at("trip-a", "stop-2", -20*time.Minute, 0),
		at("trip-a", "stop-3", 5*time.Minute, 0),
		at("trip-done", "stop-3", -3*time.Hour, 0),
		at("trip-late", "stop-2", -30*time.Minute, 27*60),
		at("trip-edge", "stop-2", -5*time.Minute, 0),
		unmatched,
	})

	trips, _, _ := c.Route("R1")
	next := map[string]string{}
	for _, tr := range trips {
		next[tr.TripID] = tr.NextStopID
	}

	t.Run("a trip whose served stops are still listed is at its first unserved stop", func(t *testing.T) {
		if next["trip-a"] != "stop-3" {
			t.Errorf("next stop = %q, want stop-3", next["trip-a"])
		}
	})
	t.Run("a trip with every stop served is not active", func(t *testing.T) {
		if _, ok := next["trip-done"]; ok {
			t.Error("trip-done is listed")
		}
	})
	t.Run("a late trip is judged by its predicted time, not its scheduled one", func(t *testing.T) {
		if next["trip-late"] != "stop-2" {
			t.Errorf("next stop = %q, want stop-2: 30 min behind the timetable, 27 min of delay", next["trip-late"])
		}
	})
	t.Run("a stop exactly five minutes behind is not yet served", func(t *testing.T) {
		if next["trip-edge"] != "stop-2" {
			t.Errorf("next stop = %q, want stop-2", next["trip-edge"])
		}
	})
	t.Run("an unmatched trip has no time to judge by and is kept", func(t *testing.T) {
		if next["trip-u"] != "stop-1" {
			t.Errorf("next stop = %q, want stop-1", next["trip-u"])
		}
	})
	t.Run("a served call is not listed at its stop", func(t *testing.T) {
		calls, _, _ := c.Stop("stop-3")
		if len(calls) != 1 || calls[0].TripID != "trip-a" {
			t.Errorf("calls at stop-3 = %+v, want trip-a only", calls)
		}
	})
}

func TestStop_AfterUpdate_ReturnsEveryCallAtTheStop(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, []ingest.Observation{
		obs("trains", "R1", "trip-a", "stop-2", 60, t0),
		obs("trains", "R1", "trip-a", "stop-3", 90, t0),
		obs("trains", "R2", "trip-b", "stop-3", 0, t0),
	})

	calls, asOf, found := c.Stop("stop-3")

	if !found || !asOf.Equal(t0) || len(calls) != 2 {
		t.Fatalf("found %v as of %s with %d calls, want 2", found, asOf, len(calls))
	}
	if calls[0].TripID != "trip-a" || *calls[0].DelayS != 90 || calls[1].RouteID != "R2" {
		t.Errorf("calls = %+v", calls)
	}
	if _, _, found := c.Stop("nowhere"); found {
		t.Error("a stop no feed reports was found")
	}
	stale := New(45*time.Minute, fixedClock(t0.Add(time.Hour)))
	stale.Update("trains", t0, []ingest.Observation{obs("trains", "R1", "trip-a", "stop-2", 60, t0)})
	if _, _, found := stale.Stop("stop-2"); found {
		t.Error("a stop from a feed past the TTL was served")
	}
}

func TestFeeds_ReportsEachFeedsLatestTimestamp(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))
	c.Update("trains", t0, nil)
	c.Update("metro", t0.Add(-time.Minute), nil)
	if f := c.Feeds(); len(f) != 2 || !f["metro"].Equal(t0.Add(-time.Minute)) {
		t.Errorf("feeds = %v", f)
	}
}

func TestStale_HeaderTimestampNotAdvancing_CountsPolls(t *testing.T) {
	c := New(45*time.Minute, fixedClock(t0))

	t.Run("a feed never polled is not stale", func(t *testing.T) {
		if c.Stale("trains", 1) {
			t.Error("stale before any poll")
		}
	})
	t.Run("stale once the timestamp has held for the given number of polls", func(t *testing.T) {
		for i := range 3 {
			c.Update("trains", t0, nil)
			if got, want := c.Stale("trains", 2), i >= 2; got != want {
				t.Errorf("after poll %d: stale = %v, want %v", i+1, got, want)
			}
		}
	})
	t.Run("an advancing timestamp resets it", func(t *testing.T) {
		c.Update("trains", t0.Add(15*time.Second), nil)
		if c.Stale("trains", 1) {
			t.Error("still stale after the timestamp moved")
		}
	})
	t.Run("a timestamp that goes backwards is not an advance", func(t *testing.T) {
		c.Update("trains", t0, nil)
		if !c.Stale("trains", 1) {
			t.Error("a step back counted as fresh data")
		}
	})
}
