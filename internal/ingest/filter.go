package ingest

import (
	"sort"
	"sync"
	"time"
)

// Filter suppresses observations that say nothing new. It is the single
// reason the storage budget works: a vehicle running exactly to schedule
// produces one row per stop per day instead of one row per stop per poll.
// Measured on 2026-09-21, every one of 3,939 updates repeated unchanged across
// a fifteen-second poll (§13).
//
// It is deliberately in front of the queue, not behind it: suppressing before
// the channel means a redundant observation costs nothing downstream at all.
type Filter struct {
	minDelta   int32
	maxEntries int

	mu      sync.Mutex
	entries map[Key]entry
	seq     uint64 // insertion order, for oldest-first eviction

	// Guarded by mu, like everything else here: exported counters beside an
	// unexported mutex invite a caller to read them without holding it.
	suppressed uint64
	admitted   uint64
	evicted    uint64
}

// FilterStats is a snapshot of what the filter has decided.
type FilterStats struct {
	Suppressed uint64
	Admitted   uint64
	Evicted    uint64
	Entries    int
}

// Stats reports the filter's counters and current size.
func (f *Filter) Stats() FilterStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FilterStats{
		Suppressed: f.suppressed,
		Admitted:   f.admitted,
		Evicted:    f.evicted,
		Entries:    len(f.entries),
	}
}

// entry is the last value written for a key. Only the fields a query can
// distinguish are kept: two observations that differ in nothing else are the
// same observation as far as every rollup and every endpoint is concerned.
type entry struct {
	delay       int32
	hasDelay    bool
	stopTimeRel int32
	tripRel     int32
	// matched decides route, direction and stop_sequence, all of which a
	// query sees. Without it, a row written before the timetable loaded
	// stayed unmatched for good: the matched version of the same delay was
	// suppressed as unchanged (found live 2026-09-23).
	matched     bool
	serviceDate string
	seq         uint64
}

// NewFilter returns a change filter admitting an observation when its delay
// moves by more than minDelta seconds, when its status changes, or when the
// key has not been seen. maxEntries bounds the map; exceeding it evicts
// oldest-first, which costs redundant writes and never correctness.
func NewFilter(minDelta int32, maxEntries int) *Filter {
	return &Filter{
		minDelta:   minDelta,
		maxEntries: maxEntries,
		entries:    make(map[Key]entry),
	}
}

// Admit reports whether the observation should be written, and records it as
// the new last-written value when it is.
func (f *Filter) Admit(o Observation) bool {
	k := o.Key()
	now := entry{
		stopTimeRel: o.StopTimeRel,
		tripRel:     o.TripRel,
		matched:     o.Matched,
		serviceDate: k.ServiceDate,
	}
	if o.ObservedDelayS != nil {
		now.delay, now.hasDelay = *o.ObservedDelayS, true
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	prev, seen := f.entries[k]
	if seen && !changed(prev, now, f.minDelta) {
		f.suppressed++
		return false
	}

	if !seen && len(f.entries) >= f.maxEntries {
		f.evictOldestLocked()
	}
	f.seq++
	now.seq = f.seq
	f.entries[k] = now
	f.admitted++
	return true
}

// changed decides whether two successive values differ in a way a query could
// see. A status change always counts: a trip becoming cancelled is the most
// important thing the feed ever says, and its delay may not move at all.
func changed(prev, now entry, minDelta int32) bool {
	if prev.stopTimeRel != now.stopTimeRel || prev.tripRel != now.tripRel || prev.matched != now.matched {
		return true
	}
	if prev.hasDelay != now.hasDelay {
		return true
	}
	if !now.hasDelay {
		return false
	}
	return abs32(now.delay-prev.delay) > minDelta
}

// ExpireBefore drops entries for service dates before the given date. Every
// trip id turns over daily, so without this the map grows without bound across
// midnight (§9.3 case 1). Returns how many were removed.
func (f *Filter) ExpireBefore(serviceDate time.Time) int {
	cutoff := serviceDate.Format(time.DateOnly)

	f.mu.Lock()
	defer f.mu.Unlock()

	var n int
	for k, e := range f.entries {
		if e.serviceDate < cutoff {
			delete(f.entries, k)
			n++
		}
	}
	f.evicted += uint64(n)
	return n
}

// Forget removes a key's last-written value, so the next observation for it is
// admitted rather than suppressed. It exists for the queue-full path: an
// observation the filter recorded but the queue refused was never written, and
// leaving the record in place would suppress its replacement.
//
// It does not adjust the admitted counter. "Admitted" means "passed the change
// filter", which this observation did; the queue refusing it afterwards is a
// different event with its own counter. Decrementing here would also undercount
// whenever the entry being forgotten belongs to a later admission than the one
// that was dropped.
func (f *Filter) Forget(k Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, k)
}

// Len is the number of keys currently remembered.
func (f *Filter) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// evictOldestLocked removes a tenth of the map, oldest first, so the scan is
// amortised over many admissions rather than run on every one. Eviction causes
// a redundant write next time that key appears; it never produces a wrong row.
func (f *Filter) evictOldestLocked() {
	type aged struct {
		key Key
		seq uint64
	}
	all := make([]aged, 0, len(f.entries))
	for k, e := range f.entries {
		all = append(all, aged{k, e.seq})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })

	drop := max(len(all)/10, 1)
	for _, a := range all[:drop] {
		delete(f.entries, a.key)
	}
	f.evicted += uint64(drop)
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
