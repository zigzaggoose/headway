package gtfsstatic

import (
	"context"
	"time"

	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/servicetime"
)

// retryAfter is how soon a failed load is retried. A day's wait would leave
// the matcher on a stale timetable for a day; retrying every poll interval
// would spend quota downloading 11 MB into the same failure.
const retryAfter = 15 * time.Minute

// loadTimeout is §10.1's budget for one schedule load. The real bundle loads
// in about 20 s.
const loadTimeout = 5 * time.Minute

// Refresh says when schedules are reloaded after the load at startup.
type Refresh struct {
	Hour, Minute int           // SCHEDULE_REFRESH_AT, Sydney time
	Interval     time.Duration // SCHEDULE_REFRESH_INTERVAL
}

// next is the earlier of the next Hour:Minute in Sydney and now+Interval.
// time.Date keeps the wall-clock time across a DST change, so the default
// daily reload stays at 03:30 all year rather than drifting by an hour.
func (r Refresh) next(now time.Time) time.Time {
	local := now.In(servicetime.Loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), r.Hour, r.Minute, 0, 0, servicetime.Loc)
	if !at.After(now) {
		at = time.Date(local.Year(), local.Month(), local.Day()+1, r.Hour, r.Minute, 0, 0, servicetime.Loc)
	}
	if soon := now.Add(r.Interval); soon.Before(at) {
		return soon
	}
	return at
}

// Run loads every feed's schedule now and again at each refresh, until ctx is
// cancelled. A load in flight at cancellation rolls back, leaving the active
// version as it was (§9.4 case 5).
func (l *Loader) Run(ctx context.Context, feeds []config.Feed, r Refresh) {
	for {
		failed := false
		for _, f := range feeds {
			if f.ScheduleURL == "" {
				continue
			}
			lctx, cancel := context.WithTimeout(ctx, loadTimeout)
			_, _, err := l.Load(lctx, f)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				failed = true
				l.log.Error("schedule load failed", "feed_id", f.ID, "err", err.Error())
			}
		}

		now := l.now()
		wake := r.next(now)
		if failed && now.Add(retryAfter).Before(wake) {
			wake = now.Add(retryAfter)
		}
		t := time.NewTimer(wake.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
