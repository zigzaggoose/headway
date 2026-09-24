package gtfsstatic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zigzaggoose/transitlateagain/internal/match"
)

// ErrNoActiveSchedule means the feed has no active schedule version yet.
var ErrNoActiveSchedule = errors.New("no active schedule version")

// Schedule reads a feed's active version back into memory for the matcher.
// It lives here rather than in internal/store because the loader owns these
// tables' SQL, and because store is imported by ingest, which match imports.
// One REPEATABLE READ snapshot means a version activated or pruned halfway
// through cannot mix two timetables.
func (l *Loader) Schedule(ctx context.Context, feedID string) (*match.Schedule, error) {
	var sched *match.Schedule
	err := pgx.BeginTxFunc(ctx, l.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var version int64
		err := tx.QueryRow(ctx, `SELECT id FROM schedule_versions WHERE feed_id = $1 AND active`, feedID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoActiveSchedule
		}
		if err != nil {
			return err
		}
		sched = match.NewSchedule(version, feedID)

		// A route with no trips can never be matched or have data: TfNSW's
		// bundle carries 34 of 152 such train routes, and /v1/lines listing
		// them was clutter a rider could only pick to see nothing (§15).
		if err := each(ctx, tx, `SELECT route_id, coalesce(short_name, ''), coalesce(long_name, ''), route_type FROM routes r
			WHERE version_id = $1 AND EXISTS (SELECT 1 FROM trips t WHERE t.version_id = r.version_id AND t.route_id = r.route_id)`,
			version, func(r pgx.Rows) error {
				var id string
				var rt match.Route
				if err := r.Scan(&id, &rt.ShortName, &rt.LongName, &rt.Type); err != nil {
					return err
				}
				sched.AddRoute(id, rt)
				return nil
			}); err != nil {
			return fmt.Errorf("routes: %w", err)
		}

		if err := each(ctx, tx, `SELECT stop_id, name FROM stops WHERE version_id = $1`,
			version, func(r pgx.Rows) error {
				var id, name string
				if err := r.Scan(&id, &name); err != nil {
					return err
				}
				sched.AddStop(id, name)
				return nil
			}); err != nil {
			return fmt.Errorf("stops: %w", err)
		}

		if err := each(ctx, tx, `SELECT trip_id, route_id, service_id, direction_id, coalesce(headsign, '') FROM trips WHERE version_id = $1`,
			version, func(r pgx.Rows) error {
				var id string
				var t match.Trip
				if err := r.Scan(&id, &t.RouteID, &t.ServiceID, &t.DirectionID, &t.Headsign); err != nil {
					return err
				}
				sched.AddTrip(id, t)
				return nil
			}); err != nil {
			return fmt.Errorf("trips: %w", err)
		}

		// The matcher needs each trip's calls in order; the primary key
		// (version_id, trip_id, stop_sequence) serves this ORDER BY.
		if err := each(ctx, tx, `
			SELECT trip_id, stop_sequence, stop_id, coalesce(arrival_s, -1), coalesce(departure_s, -1)
			FROM stop_times WHERE version_id = $1
			ORDER BY trip_id, stop_sequence`,
			version, func(r pgx.Rows) error {
				var trip string
				var st match.StopTime
				if err := r.Scan(&trip, &st.Seq, &st.StopID, &st.ArrS, &st.DepS); err != nil {
					return err
				}
				sched.AddStopTime(trip, st)
				return nil
			}); err != nil {
			return fmt.Errorf("stop_times: %w", err)
		}

		if err := each(ctx, tx, `SELECT service_id, mon, tue, wed, thu, fri, sat, sun, start_date, end_date FROM calendar WHERE version_id = $1`,
			version, func(r pgx.Rows) error {
				var id string
				var days [7]bool
				var start, end time.Time
				if err := r.Scan(&id, &days[0], &days[1], &days[2], &days[3], &days[4], &days[5], &days[6], &start, &end); err != nil {
					return err
				}
				sched.AddCalendar(id, days, start, end)
				return nil
			}); err != nil {
			return fmt.Errorf("calendar: %w", err)
		}

		if err := each(ctx, tx, `SELECT service_id, service_date, exception_type FROM calendar_dates WHERE version_id = $1`,
			version, func(r pgx.Rows) error {
				var id string
				var d time.Time
				var et int16
				if err := r.Scan(&id, &d, &et); err != nil {
					return err
				}
				sched.AddCalendarDate(id, d, int(et))
				return nil
			}); err != nil {
			return fmt.Errorf("calendar_dates: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load schedule (feed=%s): %w", feedID, err)
	}
	return sched, nil
}

// each runs a query and calls fn per row, streaming rather than collecting:
// stop_times is 1.24 million rows.
func each(ctx context.Context, tx pgx.Tx, sql string, arg any, fn func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, sql, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
