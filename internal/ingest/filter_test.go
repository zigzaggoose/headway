package ingest

import (
	"fmt"
	"testing"
	"time"
)

func day(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }

func obs(trip, stop string, delay *int32) Observation {
	return Observation{
		ServiceDate:    day(21),
		FeedID:         "sydneytrains",
		TripID:         trip,
		StopID:         stop,
		FeedTS:         time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC),
		ObservedDelayS: delay,
	}
}

func i32(v int32) *int32 { return &v }

func TestFilter_SuppressesTheUnchangedAndAdmitsTheRest(t *testing.T) {
	f := NewFilter(0, 1000)

	if !f.Admit(obs("t1", "s1", i32(60))) {
		t.Fatal("the first observation for a key was suppressed")
	}
	if f.Admit(obs("t1", "s1", i32(60))) {
		t.Error("an identical observation was admitted")
	}
	if !f.Admit(obs("t1", "s1", i32(61))) {
		t.Error("a changed delay was suppressed")
	}
	if !f.Admit(obs("t1", "s2", i32(60))) {
		t.Error("a different stop was suppressed")
	}
	if !f.Admit(obs("t2", "s1", i32(60))) {
		t.Error("a different trip was suppressed")
	}
	if f.Stats().Suppressed != 1 || f.Stats().Admitted != 4 {
		t.Errorf("suppressed=%d admitted=%d", f.Stats().Suppressed, f.Stats().Admitted)
	}
}

// A vehicle running exactly to schedule writes one row per stop per day, not
// one per stop per poll. This is the storage design in one test.
func TestFilter_APunctualTrainWritesOneRowPerStop(t *testing.T) {
	f := NewFilter(0, 10000)
	const polls = 240 // an hour at fifteen seconds

	var admitted int
	for range polls {
		for stop := range 20 {
			if f.Admit(obs("t1", fmt.Sprintf("s%d", stop), i32(0))) {
				admitted++
			}
		}
	}
	if admitted != 20 {
		t.Errorf("%d rows for 20 stops over %d polls, want 20", admitted, polls)
	}
	if ratio := float64(f.Stats().Suppressed) / float64(f.Stats().Suppressed+f.Stats().Admitted); ratio < 0.99 {
		t.Errorf("suppression ratio %.3f", ratio)
	}
}

func TestFilter_MinDelta(t *testing.T) {
	f := NewFilter(30, 1000)

	f.Admit(obs("t1", "s1", i32(100)))
	if f.Admit(obs("t1", "s1", i32(130))) {
		t.Error("a change of exactly minDelta was admitted; the bound is exclusive")
	}
	if !f.Admit(obs("t1", "s1", i32(131))) {
		t.Error("a change beyond minDelta was suppressed")
	}
}

// A status change is the most important thing the feed says, and a cancelled
// trip's delay may not move at all.
func TestFilter_StatusChange_IsAlwaysAdmitted(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Observation)
	}{
		{"a trip becomes cancelled", func(o *Observation) { o.TripRel = 3 }},
		{"a stop becomes skipped", func(o *Observation) { o.StopTimeRel = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFilter(600, 1000) // a delta wide enough to suppress any delay move
			first := obs("t1", "s1", i32(60))
			f.Admit(first)

			second := obs("t1", "s1", i32(60))
			tc.mutate(&second)
			if !f.Admit(second) {
				t.Error("a status change was suppressed")
			}
		})
	}
}

// A delay appearing or disappearing is a change: "no longer reporting a delay"
// is different from "still 60 seconds late".
func TestFilter_DelayPresenceChange_IsAdmitted(t *testing.T) {
	f := NewFilter(0, 1000)
	f.Admit(obs("t1", "s1", i32(60)))
	if !f.Admit(obs("t1", "s1", nil)) {
		t.Error("a delay disappearing was suppressed")
	}
	if f.Admit(obs("t1", "s1", nil)) {
		t.Error("two successive absent delays were not suppressed")
	}
	if !f.Admit(obs("t1", "s1", i32(60))) {
		t.Error("a delay reappearing was suppressed")
	}
}

// Every trip id turns over daily, so without expiry the map grows without
// bound across midnight (§9.3 case 1).
func TestFilter_ExpireBefore_DropsYesterday(t *testing.T) {
	f := NewFilter(0, 10000)

	for i := range 50 {
		o := obs(fmt.Sprintf("t%d", i), "s1", i32(0))
		o.ServiceDate = day(20)
		f.Admit(o)
	}
	for i := range 30 {
		o := obs(fmt.Sprintf("t%d", i), "s1", i32(0))
		o.ServiceDate = day(21)
		f.Admit(o)
	}
	if got := f.Len(); got != 80 {
		t.Fatalf("filter holds %d keys, want 80", got)
	}

	if removed := f.ExpireBefore(day(21)); removed != 50 {
		t.Errorf("expired %d, want 50", removed)
	}
	if got := f.Len(); got != 30 {
		t.Errorf("filter holds %d keys after expiry, want 30", got)
	}
}

// Exceeding the bound costs redundant writes, never correctness (§8).
func TestFilter_MaxEntries_EvictsOldestFirst(t *testing.T) {
	const max = 100
	f := NewFilter(0, max)

	for i := range max * 2 {
		f.Admit(obs(fmt.Sprintf("t%d", i), "s1", i32(0)))
	}
	if got := f.Len(); got > max {
		t.Errorf("filter holds %d keys, want at most %d", got, max)
	}
	if f.Stats().Evicted == 0 {
		t.Error("nothing was recorded as evicted")
	}

	// The most recent key must still be remembered: eviction takes the oldest.
	if f.Admit(obs(fmt.Sprintf("t%d", max*2-1), "s1", i32(0))) {
		t.Error("the newest key was evicted")
	}
}

func TestFilter_Forget_MakesTheNextObservationAdmissible(t *testing.T) {
	f := NewFilter(0, 1000)
	o := obs("t1", "s1", i32(60))

	f.Admit(o)
	if f.Admit(o) {
		t.Fatal("an identical observation was admitted")
	}
	f.Forget(o.Key())
	if !f.Admit(o) {
		t.Error("a forgotten key was still suppressed")
	}
}

// The key must not depend on how a time.Time was built: a location pointer or
// a monotonic reading would make two values naming the same day unequal, and
// the filter would silently stop suppressing.
func TestObservation_Key_DependsOnlyOnTheCalendarDate(t *testing.T) {
	syd, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	a := obs("t1", "s1", i32(0))
	a.ServiceDate = time.Date(2026, 9, 21, 0, 0, 0, 0, syd)
	b := obs("t1", "s1", i32(0))
	b.ServiceDate = time.Date(2026, 9, 21, 13, 45, 0, 0, time.UTC)

	if a.Key() != b.Key() {
		t.Errorf("keys differ for the same service date: %+v vs %+v", a.Key(), b.Key())
	}
}

func TestFilter_ConcurrentAdmits_AreCountedExactlyOnce(t *testing.T) {
	f := NewFilter(0, 100000)
	done := make(chan struct{})

	for g := range 20 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 100 {
				f.Admit(obs(fmt.Sprintf("t%d-%d", g, i), "s1", i32(0)))
			}
		}()
	}
	for range 20 {
		<-done
	}
	if st := f.Stats(); st.Admitted != 2000 || st.Entries != 2000 {
		t.Errorf("admitted=%d entries=%d, want 2000 each", f.Stats().Admitted, f.Stats().Entries)
	}
}
