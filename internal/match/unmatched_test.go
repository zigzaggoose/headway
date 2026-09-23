package match

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zigzaggoose/headway/internal/gtfsrt"
	"github.com/zigzaggoose/headway/internal/ingest"
)

const overlap = 6 * time.Hour

// 00:30 AEST on 2026-09-23: inside the overlap, so the two candidates differ
// and a test can tell which one was taken.
var justAfterMidnight = time.Date(2026, 9, 22, 14, 30, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func update() gtfsrt.RawUpdate {
	return gtfsrt.RawUpdate{
		FeedID:   "sydneytrains",
		HeaderTS: justAfterMidnight,
		TripID:   "trip-1",
		RouteID:  "route-1",
		StopID:   "stop-1",
	}
}

func one(t *testing.T, u gtfsrt.RawUpdate) ingest.Observation {
	t.Helper()
	obs, skipped := Unmatched([]gtfsrt.RawUpdate{u}, overlap)
	if len(obs) != 1 {
		t.Fatalf("got %d observations (skipped %+v), want 1", len(obs), skipped)
	}
	return obs[0]
}

func TestUnmatched_ServiceDate_PrefersStartDateOverTheClock(t *testing.T) {
	t.Run("start_date is used when the producer sends it", func(t *testing.T) {
		u := update()
		u.StartDate = "20260922"
		if got := one(t, u).ServiceDate.Format(time.DateOnly); got != "2026-09-22" {
			t.Errorf("service date = %s, want the producer's 2026-09-22", got)
		}
	})
	t.Run("without start_date the newest candidate is used", func(t *testing.T) {
		if got := one(t, update()).ServiceDate.Format(time.DateOnly); got != "2026-09-23" {
			t.Errorf("service date = %s, want 2026-09-23", got)
		}
	})
}

func TestUnmatched_UnkeyableUpdates_AreSkippedAndCounted(t *testing.T) {
	tripLevel := update()
	tripLevel.TripLevel, tripLevel.StopID = true, ""
	noStop := update()
	noStop.StopID, noStop.StopSequence = "", ptr(uint32(4))
	badDate := update()
	badDate.StartDate = "2026-09-22"

	obs, skipped := Unmatched([]gtfsrt.RawUpdate{tripLevel, noStop, badDate, update()}, overlap)

	if len(obs) != 1 {
		t.Errorf("got %d observations, want only the one keyable update", len(obs))
	}
	if want := (Skipped{TripLevel: 1, NoStopID: 1, BadStartDate: 1}); skipped != want {
		t.Errorf("skipped = %+v, want %+v", skipped, want)
	}
}

func TestUnmatched_ObservedDelay_IsDepartureThenArrival(t *testing.T) {
	cases := []struct {
		name       string
		arr, dep   *int32
		wantPtrNil bool
		want       int32
	}{
		{"departure wins when both are present", ptr(int32(60)), ptr(int32(90)), false, 90},
		{"arrival is used when departure is absent", ptr(int32(60)), nil, false, 60},
		{"an on-time departure is zero, not absent", ptr(int32(60)), ptr(int32(0)), false, 0},
		{"neither present leaves the delay unknown", nil, nil, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := update()
			u.ArrivalDelay, u.DepartureDelay = c.arr, c.dep
			got := one(t, u).ObservedDelayS
			if c.wantPtrNil {
				if got != nil {
					t.Errorf("observed delay = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != c.want {
				t.Errorf("observed delay = %v, want %d", got, c.want)
			}
		})
	}
}

func TestUnmatched_RawFields_PassThroughUnchanged(t *testing.T) {
	t.Run("direction 0 is a real direction and is kept", func(t *testing.T) {
		u := update()
		u.DirectionID = ptr(uint32(0))
		if d := one(t, u).DirectionID; d == nil || *d != 0 {
			t.Errorf("direction = %v, want 0", d)
		}
	})
	t.Run("a direction GTFS does not define is stored as unknown", func(t *testing.T) {
		u := update()
		u.DirectionID = ptr(uint32(7))
		if d := one(t, u).DirectionID; d != nil {
			t.Errorf("direction = %d, want nil", *d)
		}
	})
	t.Run("REPLACEMENT survives as the raw value 5", func(t *testing.T) {
		u := update()
		u.TripRel = 5
		if o := one(t, u); o.TripRel != 5 || o.Matched {
			t.Errorf("trip rel = %d, matched = %v; want 5, false", o.TripRel, o.Matched)
		}
	})
	t.Run("feed_ts is the header timestamp, never the fetch time", func(t *testing.T) {
		u := update()
		u.FetchedAt = u.HeaderTS.Add(3 * time.Second)
		if got := one(t, u).FeedTS; !got.Equal(u.HeaderTS) {
			t.Errorf("feed ts = %s, want header %s", got, u.HeaderTS)
		}
	})
}

// On a real response every keyable update must become an observation with a
// distinct key; a collision here is a row silently lost by ON CONFLICT.
func TestUnmatched_RecordedResponse_EveryKeyableUpdateHasADistinctKey(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sydneytrains_tripupdate_0001.pb"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoded, err := gtfsrt.Decode("sydneytrains", body, time.Date(2026, 9, 21, 2, 38, 26, 0, time.UTC))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	obs, skipped := Unmatched(decoded.Updates, overlap)

	if got := len(obs) + skipped.TripLevel + skipped.NoStopID + skipped.BadStartDate; got != len(decoded.Updates) {
		t.Fatalf("%d observations + %+v skipped != %d updates", len(obs), skipped, len(decoded.Updates))
	}
	if skipped.BadStartDate != 0 {
		t.Errorf("%d recorded updates had an unreadable start_date", skipped.BadStartDate)
	}
	seen := make(map[ingest.Key]bool, len(obs))
	for _, o := range obs {
		if seen[o.Key()] {
			t.Fatalf("duplicate key %+v", o.Key())
		}
		seen[o.Key()] = true
	}
	t.Logf("%d updates -> %d observations, skipped %+v", len(decoded.Updates), len(obs), skipped)
}
