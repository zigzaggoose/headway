// Package rollup keeps the database small and history queries fast (§4.2
// component 9): it rolls completed hours into otp_*_hourly, drops raw
// partitions past retention, and creates the partitions ahead. It knows
// nothing about feeds, HTTP or matching.
package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/metrics"
	"github.com/zigzaggoose/transitlateagain/internal/servicetime"
)

// Options are the §8 values the job uses.
type Options struct {
	OnTime        config.Thresholds
	RetentionDays int           // RETENTION_DAYS
	LookaheadDays int           // PARTITION_LOOKAHEAD_DAYS
	Interval      time.Duration // MAINTENANCE_INTERVAL
	// Settle is how long after an hour ends it is rolled up. Visits are
	// bucketed by their scheduled hour, and a late train's final word comes
	// after that hour; each hour is rolled up once, so anything said about it
	// after Settle is not counted. Nothing is ever counted twice, because a
	// visit's scheduled hour does not move.
	Settle time.Duration
	// ExpireFilter, when set, is called every tick with the oldest service
	// date still worth remembering (§9.3 case 1). A function rather than the
	// pipeline, so this package stays ignorant of ingest.
	ExpireFilter func(before time.Time)
}

// Job is the maintenance run. One goroutine calls Run.
type Job struct {
	pool *pgxpool.Pool
	opts Options
	now  func() time.Time
	log  *slog.Logger
}

// New returns a Job.
func New(pool *pgxpool.Pool, o Options, now func() time.Time, log *slog.Logger) *Job {
	return &Job{pool: pool, opts: o, now: now, log: log.With("component", "rollup")}
}

// runTimeout bounds one tick. The first tick after deployment rolls up every
// hour ever stored, which is the slow case.
const runTimeout = 10 * time.Minute

// Run ticks immediately and then every Interval until ctx is cancelled.
func (j *Job) Run(ctx context.Context) {
	for {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		err := j.Tick(tctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			j.log.Error("maintenance tick failed", "err", err.Error())
		}
		t := time.NewTimer(j.opts.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Tick runs the three steps in §4.2's order. Partitions are also ensured
// first, so a fresh database stops filling the default partition on the
// first tick rather than the second. A failed step does not stop the later
// ones, except that retention never runs after a failed rollup: it would be
// guarding on a watermark that did not move.
func (j *Job) Tick(ctx context.Context) error {
	today := servicetime.CandidateServiceDates(j.now(), 0)[0]
	var errs []error
	if _, err := j.EnsurePartitions(ctx, today.AddDate(0, 0, -1), today.AddDate(0, 0, j.opts.LookaheadDays)); err != nil {
		errs = append(errs, err)
	}
	started := j.now()
	_, err := j.Rollup(ctx)
	metrics.RollupDuration.Observe(j.now().Sub(started).Seconds())
	if err != nil {
		errs = append(errs, err)
	} else if _, err := j.DropPartitionsBefore(ctx, today.AddDate(0, 0, -j.opts.RetentionDays)); err != nil {
		errs = append(errs, err)
	}
	if j.opts.ExpireFilter != nil {
		// Yesterday's trips are still running after midnight; anything
		// older will not be observed again.
		j.opts.ExpireFilter(today.AddDate(0, 0, -1))
	}
	return errors.Join(errs...)
}

var partitionName = regexp.MustCompile(`^observations_(\d{4})_(\d{2})_(\d{2})$`)

func nameFor(d time.Time) string { return "observations_" + d.Format("2006_01_02") }

// partitions lists the daily partitions by service date, oldest first. The
// parent is resolved through search_path, which is how tests isolate
// themselves in a schema.
func (j *Job) partitions(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]time.Time, error) {
	rows, err := q.Query(ctx, `
		SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'observations'::regclass`)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	var out []time.Time
	for _, n := range names {
		if m := partitionName.FindStringSubmatch(n); m != nil {
			d, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
			if err == nil {
				out = append(out, d)
			}
		}
	}
	slices.SortFunc(out, time.Time.Compare)
	return out, nil
}

// EnsurePartitions creates a daily partition for every service date in
// [from, to] that lacks one, and returns the names created.
//
// A date that already has rows in observations_default cannot simply get a
// partition: Postgres refuses, because the default would then hold rows the
// new partition claims. Those rows are moved into a standalone table which is
// then attached, in one transaction (§9.3 case 4).
func (j *Job) EnsurePartitions(ctx context.Context, from, to time.Time) ([]string, error) {
	have, err := j.partitions(ctx, j.pool)
	if err != nil {
		return nil, err
	}
	var created []string
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if slices.ContainsFunc(have, d.Equal) {
			continue
		}
		name := nameFor(d)
		ident := pgx.Identifier{name}.Sanitize()
		lo, hi := d.Format(time.DateOnly), d.AddDate(0, 0, 1).Format(time.DateOnly)
		err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
				return err
			}
			var stranded bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM observations_default WHERE service_date = $1)`, d).Scan(&stranded); err != nil {
				return err
			}
			if !stranded {
				_, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF observations FOR VALUES FROM ('%s') TO ('%s')`, ident, lo, hi))
				return err
			}
			for _, stmt := range []string{
				fmt.Sprintf(`CREATE TABLE %s (LIKE observations INCLUDING DEFAULTS)`, ident),
				fmt.Sprintf(`INSERT INTO %s SELECT * FROM observations_default WHERE service_date = '%s'`, ident, lo),
				fmt.Sprintf(`DELETE FROM observations_default WHERE service_date = '%s'`, lo),
				fmt.Sprintf(`ALTER TABLE observations ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`, ident, lo, hi),
			} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			j.log.Warn("moved rows out of the default partition", "partition", name)
			return nil
		})
		if err != nil {
			return created, fmt.Errorf("create partition %s: %w", name, err)
		}
		created = append(created, name)
	}
	if len(created) > 0 {
		j.log.Info("partitions created", "partitions", created)
	}
	return created, nil
}

// DropPartitionsBefore drops daily partitions for service dates before
// cutoff, but only those with nothing at or past the rollup watermark:
// dropping raw data that has not been rolled up loses it for good (§9.3 case
// 3). It never touches the default partition.
func (j *Job) DropPartitionsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	have, err := j.partitions(ctx, j.pool)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, d := range have {
		if !d.Before(cutoff) {
			break
		}
		name := nameFor(d)
		ident := pgx.Identifier{name}.Sanitize()
		err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
			// A DROP queued behind a long read would block every writer
			// behind it; give up and retry next tick instead (§9.3 case 5).
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
				return err
			}
			// Pending means a visit in this partition is bucketed at or past
			// the watermark, by the same expression the rollup buckets on.
			var pending bool
			if err := tx.QueryRow(ctx, fmt.Sprintf(`
				SELECT EXISTS (SELECT 1 FROM %s WHERE coalesce(scheduled_at, feed_ts) >= (SELECT watermark FROM rollup_state WHERE name = 'hourly'))`, ident),
			).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return errNotRolledUp
			}
			_, err := tx.Exec(ctx, `DROP TABLE `+ident)
			return err
		})
		if errors.Is(err, errNotRolledUp) {
			j.log.Warn("retention skipped a partition the rollup has not passed", "partition", name)
			continue
		}
		if err != nil {
			return dropped, fmt.Errorf("drop partition %s: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	if len(dropped) > 0 {
		j.log.Info("partitions dropped", "partitions", dropped)
	}
	return dropped, nil
}

var errNotRolledUp = errors.New("partition has rows past the rollup watermark")
