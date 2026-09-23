package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zigzaggoose/headway/internal/servicetime"
)

// Result is what one rollup did.
type Result struct {
	From, To  time.Time // the window, [From, To); equal when nothing was due
	Finals    int64     // stop visits whose final word fell in the window
	StopRows  int64
	RouteRows int64
}

// sentinel is the watermark migrations/0003 seeds: "never run".
var sentinel = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Rollup aggregates every completed hour since the watermark and advances it,
// in one transaction, so a crash leaves either the old watermark and no new
// rows or both.
//
// §6.3 takes the last observation of each stop visit: the final word on what
// happened there, so a trip updated fifty times weighs the same as one
// updated twice. That last observation is taken across the whole service day
// and then bucketed by the hour it was made in. Taking it within each hour
// instead would count a prediction made at 07:10 for a 09:00 stop in the
// 07:00 bucket as well as the 09:00 one.
func (j *Job) Rollup(ctx context.Context) (Result, error) {
	var res Result
	err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		var watermark time.Time
		// FOR UPDATE: two instances rolling up the same window would both
		// write it; the second waits and then finds nothing due.
		if err := tx.QueryRow(ctx, `SELECT watermark FROM rollup_state WHERE name = 'hourly' FOR UPDATE`).Scan(&watermark); err != nil {
			return fmt.Errorf("read watermark: %w", err)
		}
		from := watermark
		if from.Equal(sentinel) {
			// First run: start at the first hour anything was observed.
			var first *time.Time
			if err := tx.QueryRow(ctx, `SELECT date_trunc('hour', min(feed_ts), 'UTC') FROM observations`).Scan(&first); err != nil {
				return fmt.Errorf("find first observation: %w", err)
			}
			if first == nil {
				res.From, res.To = watermark, watermark
				return nil
			}
			from = *first
		}
		to := j.now().Add(-j.opts.Lag).UTC().Truncate(time.Hour)
		res.From, res.To = from, to
		if !to.After(from) {
			res.To = from
			return nil
		}

		// A stop visit's final observation can only be in [from, to) if the
		// visit has a row at or after from, so rows before the watermark are
		// never read. The service-date bounds only let the planner skip whole
		// partitions: a trip's updates span at most its own service day and
		// the early hours of the next.
		lo := servicetime.CandidateServiceDates(from, 0)[0].AddDate(0, 0, -2)
		hi := servicetime.CandidateServiceDates(to, 0)[0].AddDate(0, 0, 1)
		tag, err := tx.Exec(ctx, `
			CREATE TEMP TABLE final ON COMMIT DROP AS
			SELECT * FROM (
				SELECT DISTINCT ON (service_date, feed_id, trip_id, stop_id)
				       service_date, stop_id, route_id, direction_id,
				       observed_delay_s, stop_time_rel, trip_rel, feed_ts
				FROM observations
				WHERE service_date BETWEEN $1 AND $2
				  AND feed_ts >= $3
				ORDER BY service_date, feed_id, trip_id, stop_id, feed_ts DESC
			) last_word
			WHERE feed_ts < $4`, lo, hi, from, to)
		if err != nil {
			return fmt.Errorf("select final observations: %w", err)
		}
		res.Finals = tag.RowsAffected()

		// Buckets are UTC hours (migrations/0003). service_date is not in
		// the key, and one bucket at one stop can hold visits from two
		// service dates around midnight, so it is aggregated rather than
		// grouped on: grouping on it, as §6.3 first had it, makes ON
		// CONFLICT hit the same row twice in one statement, which Postgres
		// rejects.
		counts := `
			count(*),
			count(*) FILTER (WHERE observed_delay_s < $1),
			count(*) FILTER (WHERE observed_delay_s BETWEEN $1 AND $2),
			count(*) FILTER (WHERE observed_delay_s > $2 AND observed_delay_s <= $3),
			count(*) FILTER (WHERE observed_delay_s > $3),
			count(*) FILTER (WHERE stop_time_rel = 1),
			count(*) FILTER (WHERE trip_rel = 3),
			percentile_disc(0.5) WITHIN GROUP (ORDER BY observed_delay_s),
			percentile_disc(0.9) WITHIN GROUP (ORDER BY observed_delay_s),
			avg(observed_delay_s)::real`
		update := `
			service_date = EXCLUDED.service_date, n_obs = EXCLUDED.n_obs,
			n_early = EXCLUDED.n_early, n_on_time = EXCLUDED.n_on_time,
			n_late = EXCLUDED.n_late, n_very_late = EXCLUDED.n_very_late,
			n_skipped = EXCLUDED.n_skipped, n_cancelled = EXCLUDED.n_cancelled,
			delay_p50_s = EXCLUDED.delay_p50_s, delay_p90_s = EXCLUDED.delay_p90_s,
			delay_mean_s = EXCLUDED.delay_mean_s, computed_at = now()`
		cols := `n_obs, n_early, n_on_time, n_late, n_very_late, n_skipped, n_cancelled, delay_p50_s, delay_p90_s, delay_mean_s`
		t := j.opts.OnTime

		tag, err = tx.Exec(ctx, `
			INSERT INTO otp_stop_hourly (bucket_start, service_date, stop_id, route_id, direction_id, `+cols+`)
			SELECT date_trunc('hour', feed_ts, 'UTC'), max(service_date), stop_id,
			       coalesce(route_id, '~unmatched'), coalesce(direction_id, -1), `+counts+`
			FROM final
			GROUP BY 1, 3, 4, 5
			ON CONFLICT (bucket_start, stop_id, route_id, direction_id) DO UPDATE SET `+update,
			t.EarlyS, t.LateS, t.VeryLateS)
		if err != nil {
			return fmt.Errorf("roll up stops: %w", err)
		}
		res.StopRows = tag.RowsAffected()

		tag, err = tx.Exec(ctx, `
			INSERT INTO otp_route_hourly (bucket_start, service_date, route_id, direction_id, `+cols+`)
			SELECT date_trunc('hour', feed_ts, 'UTC'), max(service_date),
			       coalesce(route_id, '~unmatched'), coalesce(direction_id, -1), `+counts+`
			FROM final
			GROUP BY 1, 3, 4
			ON CONFLICT (bucket_start, route_id, direction_id) DO UPDATE SET `+update,
			t.EarlyS, t.LateS, t.VeryLateS)
		if err != nil {
			return fmt.Errorf("roll up routes: %w", err)
		}
		res.RouteRows = tag.RowsAffected()

		if _, err := tx.Exec(ctx, `UPDATE rollup_state SET watermark = $1, updated_at = now() WHERE name = 'hourly'`, to); err != nil {
			return fmt.Errorf("advance watermark: %w", err)
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("rollup: %w", err)
	}
	if res.To.After(res.From) {
		j.log.Info("rolled up",
			"from", res.From, "to", res.To, "finals", res.Finals,
			"stop_rows", res.StopRows, "route_rows", res.RouteRows)
	}
	return res, nil
}
