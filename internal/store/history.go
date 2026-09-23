package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HistoryQuery selects rollup rows for one stop or one route. StopID empty
// means a route history, with RouteID as its subject rather than a filter.
type HistoryQuery struct {
	StopID    string
	RouteID   string
	Direction *int16
	From, To  time.Time // [From, To)
}

// HourRow is one otp_*_hourly row.
type HourRow struct {
	BucketStart                             time.Time
	NObs, NEarly, NOnTime, NLate, NVeryLate int
	NSkipped, NCancelled                    int
	P50, P90                                *int32
	Mean                                    *float32
}

// History is what the history endpoints need from the database.
type History struct {
	// ScheduleLoaded is false when no feed has an active schedule version,
	// which is §7.2's 503 not_ready for data endpoints.
	ScheduleLoaded bool
	// Known says the stop or route is in an active schedule; false is 404.
	Known bool
	// Name is the stop's name, or the route's short name.
	Name string
	Rows []HourRow
}

// History reads a stop's or a route's hourly rollups, oldest first.
func (s *Store) History(ctx context.Context, q HistoryQuery) (History, error) {
	var h History
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schedule_versions WHERE active)`).Scan(&h.ScheduleLoaded); err != nil {
		return History{}, fmt.Errorf("history: check schedule: %w", err)
	}
	if !h.ScheduleLoaded {
		return h, nil
	}

	lookup := `SELECT s.name FROM stops s JOIN schedule_versions v ON v.id = s.version_id AND v.active WHERE s.stop_id = $1 LIMIT 1`
	subject := q.StopID
	if q.StopID == "" {
		lookup = `SELECT coalesce(r.short_name, r.long_name, '') FROM routes r JOIN schedule_versions v ON v.id = r.version_id AND v.active WHERE r.route_id = $1 LIMIT 1`
		subject = q.RouteID
	}
	err := s.pool.QueryRow(ctx, lookup, subject).Scan(&h.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return History{}, fmt.Errorf("history: look up %q: %w", subject, err)
	}
	h.Known = true

	// Optional filters as "$n IS NULL OR ...", so one statement serves every
	// combination and each is still answered from the (subject, bucket_start)
	// index.
	var rows pgx.Rows
	var route *string
	if q.RouteID != "" {
		route = &q.RouteID
	}
	const cols = `bucket_start, n_obs, n_early, n_on_time, n_late, n_very_late, n_skipped, n_cancelled, delay_p50_s, delay_p90_s, delay_mean_s`
	if q.StopID != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT `+cols+` FROM otp_stop_hourly
			WHERE stop_id = $1 AND bucket_start >= $2 AND bucket_start < $3
			  AND ($4::text IS NULL OR route_id = $4)
			  AND ($5::smallint IS NULL OR direction_id = $5)
			ORDER BY bucket_start`, q.StopID, q.From, q.To, route, q.Direction)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT `+cols+` FROM otp_route_hourly
			WHERE route_id = $1 AND bucket_start >= $2 AND bucket_start < $3
			  AND ($4::smallint IS NULL OR direction_id = $4)
			ORDER BY bucket_start`, q.RouteID, q.From, q.To, q.Direction)
	}
	if err != nil {
		return History{}, fmt.Errorf("history %q: %w", subject, err)
	}
	h.Rows, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (HourRow, error) {
		var x HourRow
		err := r.Scan(&x.BucketStart, &x.NObs, &x.NEarly, &x.NOnTime, &x.NLate, &x.NVeryLate,
			&x.NSkipped, &x.NCancelled, &x.P50, &x.P90, &x.Mean)
		return x, err
	})
	if err != nil {
		return History{}, fmt.Errorf("history %q: %w", subject, err)
	}
	return h, nil
}
