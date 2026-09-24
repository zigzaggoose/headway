package gtfsstatic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/servicetime"
)

// retryAfter is how soon a failed load is retried. A day's wait would leave
// the matcher on a stale timetable for a day; retrying every poll interval
// would spend quota downloading 11 MB into the same failure.
const retryAfter = 15 * time.Minute

// loadTimeout is §10.1's budget for one schedule load. The real bundle loads
// in about 20 s.
const loadTimeout = 5 * time.Minute

// Publish hands every feed's active version, if it has one, to the matcher,
// without downloading anything. main calls it before the pollers start: on a
// restart the timetable is already in Postgres, and without this the first
// poll is matched against nothing and written twice, once unmatched and once
// matched (2,275 redundant rows on one restart, 2026-09-23).
func (l *Loader) Publish(ctx context.Context, feeds []config.Feed, m *match.Matcher) error {
	var errs []error
	for _, f := range feeds {
		if err := l.publishActive(ctx, f.ID, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// publishActive reads the active version into memory and hands it to the
// matcher, unless the matcher already has it. Matches in flight finish on the
// old one (§9.1 step 4). No active version at all is not an error: it is a
// first start whose download failed.
func (l *Loader) publishActive(ctx context.Context, feedID string, m *match.Matcher) error {
	var active int64
	err := l.pool.QueryRow(ctx, `SELECT id FROM schedule_versions WHERE feed_id = $1 AND active`, feedID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && active == m.Loaded(feedID)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active schedule (feed=%s): %w", feedID, err)
	}
	start := l.now()
	s, err := l.Schedule(ctx, feedID)
	if err != nil {
		return err
	}
	m.Swap(s)
	today := servicetime.CandidateServiceDates(start, 0)[0]
	share := s.ActiveShare(today)
	l.log.Info("schedule in use", "feed_id", feedID, "version_id", s.VersionID, "trips", s.Trips(),
		"active_share", share, "build_ms", l.now().Sub(start).Milliseconds())
	if share == 0 {
		// §9.1 case 17: matching by trip_id still works, but a calendar that
		// has stopped covering today means the bundle is stale or broken.
		// Not a fraction: TfNSW defines a service per weekday pattern, so on
		// a normal Wednesday only 15 % of services run (measured 2026-09-23).
		l.log.Warn("no service in the schedule runs today", "feed_id", feedID, "version_id", s.VersionID)
	}
	return nil
}

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
// cancelled, and publishes each newly active version to m. A load in flight
// at cancellation rolls back, leaving the active version as it was (§9.4
// case 5).
func (l *Loader) Run(ctx context.Context, feeds []config.Feed, r Refresh, m *match.Matcher) {
	for {
		failed := false
		for _, f := range feeds {
			if f.ScheduleURL == "" {
				continue
			}
			lctx, cancel := context.WithTimeout(ctx, loadTimeout)
			_, _, err := l.Load(lctx, f)
			if err != nil {
				failed = true
				l.log.Error("schedule load failed", "feed_id", f.ID, "err", err.Error())
			}
			// Publish whatever is active even when the download failed: at
			// startup the matcher is empty, and yesterday's timetable beats
			// none (§9.1 case 16).
			if err := l.publishActive(lctx, f.ID, m); err != nil {
				failed = true
				l.log.Error("schedule publish failed", "feed_id", f.ID, "err", err.Error())
			}
			cancel()
			if ctx.Err() != nil {
				return
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
