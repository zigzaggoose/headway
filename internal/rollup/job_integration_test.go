//go:build integration

package rollup

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/headway"
	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST is not set")
	}
	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), rand.IntN(1000))
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") // best effort; a leftover test schema is harmless
		admin.Close()
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := store.Open(ctx, config.Secret(dsn+sep+"search_path="+schema), 4, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	sub, err := fs.Sub(headway.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if _, err := s.Migrate(ctx, sub); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s.Pool()
}

var aest = time.FixedZone("AEST", 10*3600)

// sydney is a wall-clock instant on 2026-09-23 in Sydney.
func sydney(h, m int) time.Time { return time.Date(2026, 9, 23, h, m, 0, 0, aest) }

func date(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }

type harness struct {
	pool *pgxpool.Pool
	job  *Job
	now  time.Time
}

func newHarness(t *testing.T, now time.Time) *harness {
	h := &harness{pool: testPool(t), now: now}
	h.job = New(h.pool, Options{
		OnTime:        config.Thresholds{EarlyS: -60, LateS: 300, VeryLateS: 900},
		RetentionDays: 7, LookaheadDays: 3, Interval: time.Hour, Settle: time.Hour,
	}, func() time.Time { return h.now }, slog.New(slog.DiscardHandler))
	return h
}

type row struct {
	serviceDate time.Time
	trip, stop  string
	route       string // "" is NULL
	delay       *int32
	tripRel     int16
	feedTS      time.Time
	scheduledAt *time.Time // set on a matched visit; nil buckets by feedTS
}

func (h *harness) insert(t *testing.T, rows ...row) {
	t.Helper()
	for _, r := range rows {
		var route *string
		if r.route != "" {
			route = &r.route
		}
		if _, err := h.pool.Exec(context.Background(), `
			INSERT INTO observations (service_date, feed_id, trip_id, stop_id, feed_ts, route_id, observed_delay_s, trip_rel, stop_time_rel, matched, scheduled_at)
			VALUES ($1, 'sydneytrains', $2, $3, $4, $5, $6, $7, 0, $8, $9)`,
			r.serviceDate, r.trip, r.stop, r.feedTS, route, r.delay, r.tripRel, r.scheduledAt != nil, r.scheduledAt); err != nil {
			t.Fatalf("insert %+v: %v", r, err)
		}
	}
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (h *harness) rollup(t *testing.T) Result {
	t.Helper()
	res, err := h.job.Rollup(context.Background())
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	return res
}

func d(v int32) *int32 { return &v }

func TestEnsurePartitions_CreatesTheRangeAndIsIdempotent(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	ctx := context.Background()

	created, err := h.job.EnsurePartitions(ctx, date(22), date(26))
	if err != nil || len(created) != 5 {
		t.Fatalf("created %v, err %v; want five partitions", created, err)
	}
	again, err := h.job.EnsurePartitions(ctx, date(22), date(26))
	if err != nil || len(again) != 0 {
		t.Errorf("second run created %v, err %v; want nothing", again, err)
	}

	h.insert(t, row{serviceDate: date(23), trip: "T", stop: "A", feedTS: sydney(8, 0)})
	if n := h.count(t, `SELECT count(*) FROM observations_default`); n != 0 {
		t.Errorf("%d rows in the default partition, want 0 once a daily partition exists", n)
	}
	if n := h.count(t, `SELECT count(*) FROM observations_2026_09_23`); n != 1 {
		t.Errorf("%d rows in observations_2026_09_23, want 1", n)
	}
}

// §9.3 case 4 recovery: every row written before this package existed is in
// the default partition, and a plain CREATE ... PARTITION OF refuses.
func TestEnsurePartitions_RowsStrandedInDefault_AreMovedIntoTheNewPartition(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	h.insert(t,
		row{serviceDate: date(23), trip: "T", stop: "A", feedTS: sydney(8, 0)},
		row{serviceDate: date(23), trip: "T", stop: "B", feedTS: sydney(8, 5)},
		row{serviceDate: date(24), trip: "T", stop: "A", feedTS: sydney(8, 0).AddDate(0, 0, 1)},
	)

	if _, err := h.job.EnsurePartitions(context.Background(), date(23), date(23)); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if n := h.count(t, `SELECT count(*) FROM observations_2026_09_23`); n != 2 {
		t.Errorf("%d rows moved into the partition, want 2", n)
	}
	if n := h.count(t, `SELECT count(*) FROM observations_default`); n != 1 {
		t.Errorf("%d rows left in default, want only the other date's 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM observations`); n != 3 {
		t.Errorf("%d rows through the parent, want all 3", n)
	}
}

func TestRollup_FinalObservation_IsCountedOnceInTheHourItWasMade(t *testing.T) {
	h := newHarness(t, sydney(11, 5)) // lag 1h: hours up to 10:00 are due
	h.insert(t,
		// A stop predicted at 07:10 and 08:20, and finally observed at 08:55.
		row{serviceDate: date(23), trip: "T1", stop: "A", route: "R", delay: d(60), feedTS: sydney(7, 10)},
		row{serviceDate: date(23), trip: "T1", stop: "A", route: "R", delay: d(200), feedTS: sydney(8, 20)},
		row{serviceDate: date(23), trip: "T1", stop: "A", route: "R", delay: d(400), feedTS: sydney(8, 55)},
		// Another stop, on time, in the same hour.
		row{serviceDate: date(23), trip: "T2", stop: "A", route: "R", delay: d(30), feedTS: sydney(8, 40)},
		// A cancelled call: no delay, counted as cancelled, not as on time.
		row{serviceDate: date(23), trip: "T3", stop: "A", route: "R", tripRel: 3, feedTS: sydney(8, 45)},
		// Unmatched: no route.
		row{serviceDate: date(23), trip: "T4", stop: "A", delay: d(1000), feedTS: sydney(8, 50)},
	)

	res := h.rollup(t)

	if res.Finals != 4 {
		t.Errorf("finals = %d, want 4 stop visits", res.Finals)
	}
	if n := h.count(t, `SELECT count(*) FROM otp_stop_hourly WHERE bucket_start = $1`, sydney(7, 0)); n != 0 {
		t.Errorf("the 07:00 bucket has %d rows: an early prediction was counted as well as the final word", n)
	}
	var nObs, early, onTime, late, veryLate, cancelled, p50 int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT n_obs, n_early, n_on_time, n_late, n_very_late, n_cancelled, delay_p50_s
		FROM otp_route_hourly WHERE bucket_start = $1 AND route_id = 'R' AND direction_id = -1`, sydney(8, 0),
	).Scan(&nObs, &early, &onTime, &late, &veryLate, &cancelled, &p50); err != nil {
		t.Fatalf("read route bucket: %v", err)
	}
	if nObs != 3 || early != 0 || onTime != 1 || late != 1 || veryLate != 0 || cancelled != 1 || p50 != 30 {
		t.Errorf("R at 08:00: n %d early %d on %d late %d very %d cancelled %d p50 %d; want 3 0 1 1 0 1 30",
			nObs, early, onTime, late, veryLate, cancelled, p50)
	}
	if n := h.count(t, `SELECT n_very_late FROM otp_route_hourly WHERE route_id = '~unmatched'`); n != 1 {
		t.Errorf("unmatched bucket very_late = %d, want 1", n)
	}
	var wm time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT watermark FROM rollup_state WHERE name = 'hourly'`).Scan(&wm); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if !wm.Equal(sydney(10, 0)) {
		t.Errorf("watermark = %s, want %s", wm, sydney(10, 0))
	}
}

// Regression for §6.3 as first written: grouping on service_date puts two
// rows with one key into a single INSERT ... ON CONFLICT, which Postgres
// rejects outright.
func TestRollup_TwoServiceDatesInOneBucket_AreOneRow(t *testing.T) {
	h := newHarness(t, sydney(3, 5))
	h.insert(t,
		row{serviceDate: date(22), trip: "LATE", stop: "A", route: "R", delay: d(0), feedTS: sydney(0, 40)},
		row{serviceDate: date(23), trip: "EARLY", stop: "A", route: "R", delay: d(0), feedTS: sydney(0, 50)},
	)

	h.rollup(t)

	var n int
	var sd time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT n_obs, service_date FROM otp_stop_hourly WHERE stop_id = 'A' AND bucket_start = $1`, sydney(0, 0)).Scan(&n, &sd); err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if n != 2 || !sd.Equal(date(23)) {
		t.Errorf("n_obs %d service_date %s; want 2 and the later date", n, sd.Format(time.DateOnly))
	}
}

func TestRollup_Settle_HoldsBackTheNewestHour(t *testing.T) {
	h := newHarness(t, sydney(10, 10))
	h.insert(t,
		row{serviceDate: date(23), trip: "T1", stop: "A", route: "R", delay: d(0), feedTS: sydney(8, 10)},
		row{serviceDate: date(23), trip: "T2", stop: "A", route: "R", delay: d(0), feedTS: sydney(9, 30)},
	)

	first := h.rollup(t)
	if first.Finals != 1 || !first.To.Equal(sydney(9, 0)) {
		t.Fatalf("first run: finals %d to %s; want 1 up to 09:00", first.Finals, first.To)
	}

	// T2's stop gets a later word before its hour is rolled up, and moves.
	h.insert(t, row{serviceDate: date(23), trip: "T2", stop: "A", route: "R", delay: d(500), feedTS: sydney(10, 5)})
	h.now = sydney(11, 20)
	second := h.rollup(t)

	if second.Finals != 0 || !second.From.Equal(sydney(9, 0)) || !second.To.Equal(sydney(10, 0)) {
		t.Errorf("second run: %+v; want nothing final in 09:00, since T2's last word moved to 10:00", second)
	}
	h.now = sydney(12, 20)
	h.rollup(t)
	if n := h.count(t, `SELECT sum(n_obs) FROM otp_route_hourly`); n != 2 {
		t.Errorf("%d stop visits rolled up in total, want exactly 2", n)
	}
}

func TestRollup_NothingDue_IsANoOp(t *testing.T) {
	h := newHarness(t, sydney(9, 0))
	if res := h.rollup(t); res.To.After(res.From) {
		t.Errorf("empty database rolled up %+v", res)
	}
	h.insert(t, row{serviceDate: date(23), trip: "T", stop: "A", route: "R", delay: d(0), feedTS: sydney(8, 30)})
	if res := h.rollup(t); res.To.After(res.From) {
		t.Errorf("rolled up %+v before the hour cleared the lag", res)
	}
}

// §9.3 case 3: raw data the rollup has not passed must never be dropped.
func TestDropPartitionsBefore_KeepsAnythingNotYetRolledUp(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	ctx := context.Background()
	if _, err := h.job.EnsurePartitions(ctx, date(10), date(13)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	h.insert(t,
		row{serviceDate: date(10), trip: "T", stop: "A", route: "R", delay: d(0), feedTS: time.Date(2026, 9, 10, 8, 0, 0, 0, aest)},
		row{serviceDate: date(11), trip: "T", stop: "A", route: "R", delay: d(0), feedTS: time.Date(2026, 9, 11, 8, 0, 0, 0, aest)},
	)
	if _, err := h.pool.Exec(ctx, `UPDATE rollup_state SET watermark = $1`, time.Date(2026, 9, 11, 0, 0, 0, 0, aest)); err != nil {
		t.Fatalf("set watermark: %v", err)
	}

	dropped, err := h.job.DropPartitionsBefore(ctx, date(13))
	if err != nil {
		t.Fatalf("drop: %v", err)
	}

	if strings.Join(dropped, ",") != "observations_2026_09_10,observations_2026_09_12" {
		t.Errorf("dropped %v; want the 10th (rolled up) and the empty 12th, not the 11th", dropped)
	}
	if n := h.count(t, `SELECT count(*) FROM observations WHERE service_date = $1`, date(11)); n != 1 {
		t.Errorf("the 11th's row is gone: %d left", n)
	}
	if n := h.count(t, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'observations'::regclass AND inhrelid::regclass::text = 'observations_2026_09_13'`); n != 1 {
		t.Error("the cutoff date itself was dropped")
	}
}

func TestTick_RunsEveryStepAndExpiresTheFilter(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	var expired time.Time
	h.job.opts.ExpireFilter = func(before time.Time) { expired = before }
	h.insert(t, row{serviceDate: date(23), trip: "T", stop: "A", route: "R", delay: d(0), feedTS: sydney(9, 0)})

	if err := h.job.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if n := h.count(t, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'observations'::regclass AND inhrelid::regclass::text LIKE 'observations_2026_09_%'`); n != 5 {
		t.Errorf("%d daily partitions, want yesterday through three days ahead", n)
	}
	if n := h.count(t, `SELECT count(*) FROM otp_stop_hourly WHERE route_id <> '~all'`); n != 1 {
		t.Errorf("%d stop buckets, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM otp_stop_hourly WHERE route_id = '~all'`); n != 1 {
		t.Errorf("%d all-routes stop buckets, want 1", n)
	}
	if !expired.Equal(date(22)) {
		t.Errorf("filter expired before %s, want yesterday", expired.Format(time.DateOnly))
	}
}

func sched(h, m int) *time.Time { t := sydney(h, m); return &t }

// §16 q12, the reason for scheduled_at: TfNSW keeps a cancelled trip in the
// feed all day, so its last observation can be many hours after its time.
// The visit belongs to the hour it was scheduled, not the hour the feed last
// mentioned it.
func TestRollup_MatchedVisit_IsBucketedByItsScheduledHour(t *testing.T) {
	h := newHarness(t, sydney(23, 30))
	h.insert(t,
		// A 03:49 cancellation, first seen at 03:00 and still reported at 22:50.
		row{serviceDate: date(23), trip: "C", stop: "A", route: "R", tripRel: 3, feedTS: sydney(3, 0), scheduledAt: sched(3, 49)},
		row{serviceDate: date(23), trip: "C", stop: "A", route: "R", tripRel: 3, feedTS: sydney(22, 50), scheduledAt: sched(3, 49)},
		// A 09:00 visit predicted at 07:10 and last reported at 09:05, 300 s late.
		row{serviceDate: date(23), trip: "P", stop: "A", route: "R", delay: d(60), feedTS: sydney(7, 10), scheduledAt: sched(9, 0)},
		row{serviceDate: date(23), trip: "P", stop: "A", route: "R", delay: d(300), feedTS: sydney(9, 5), scheduledAt: sched(9, 0)},
	)

	h.rollup(t)

	bucket := func(hour int) (n, cancelled, onTime int) {
		t.Helper()
		err := h.pool.QueryRow(context.Background(),
			`SELECT coalesce(sum(n_obs), 0), coalesce(sum(n_cancelled), 0), coalesce(sum(n_on_time), 0) FROM otp_route_hourly WHERE bucket_start = $1`,
			sydney(hour, 0)).Scan(&n, &cancelled, &onTime)
		if err != nil {
			t.Fatalf("read bucket: %v", err)
		}
		return
	}
	if n, c, _ := bucket(3); n != 1 || c != 1 {
		t.Errorf("03:00: %d visits, %d cancelled; want the cancellation, once", n, c)
	}
	if n, _, _ := bucket(22); n != 0 {
		t.Errorf("22:00 holds %d visits: a cancellation was counted when the feed last mentioned it", n)
	}
	if n, _, _ := bucket(7); n != 0 {
		t.Errorf("07:00 holds %d visits: a prediction was counted in the hour it was made", n)
	}
	if n, _, on := bucket(9); n != 1 || on != 1 {
		t.Errorf("09:00: %d visits, %d on time; want one visit at its final 300 s", n, on)
	}
}

// Each hour is rolled up once. A word about a visit that arrives after its
// hour has settled is not counted, and in particular not counted twice.
func TestRollup_WordAfterSettling_IsNeverCountedTwice(t *testing.T) {
	h := newHarness(t, sydney(10, 10)) // settle 1 h: hours up to 09:00 are due
	h.insert(t, row{serviceDate: date(23), trip: "L", stop: "A", route: "R", delay: d(600), feedTS: sydney(8, 50), scheduledAt: sched(8, 30)})
	h.rollup(t)

	h.insert(t, row{serviceDate: date(23), trip: "L", stop: "A", route: "R", delay: d(1200), feedTS: sydney(10, 20), scheduledAt: sched(8, 30)})
	h.now = sydney(14, 0)
	h.rollup(t)

	if n := h.count(t, `SELECT sum(n_obs) FROM otp_route_hourly`); n != 1 {
		t.Errorf("%d visits rolled up, want exactly 1", n)
	}
	if n := h.count(t, `SELECT n_late FROM otp_route_hourly WHERE bucket_start = $1`, sydney(8, 0)); n != 1 {
		t.Errorf("08:00 n_late = %d; want the 600 s value it had when the hour settled", n)
	}
}

// Retention decides "rolled up yet" with the same expression the rollup
// buckets on: a partition whose visits are scheduled past the watermark is
// kept even if every row was written before it.
func TestDropPartitionsBefore_UsesTheScheduledHour(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	ctx := context.Background()
	if _, err := h.job.EnsurePartitions(ctx, date(10), date(10)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	late := time.Date(2026, 9, 11, 1, 0, 0, 0, aest) // 25:00 on the 10th's service day
	h.insert(t, row{serviceDate: date(10), trip: "T", stop: "A", route: "R", delay: d(0),
		feedTS: time.Date(2026, 9, 10, 20, 0, 0, 0, aest), scheduledAt: &late})
	if _, err := h.pool.Exec(ctx, `UPDATE rollup_state SET watermark = $1`, time.Date(2026, 9, 11, 0, 0, 0, 0, aest)); err != nil {
		t.Fatalf("set watermark: %v", err)
	}

	dropped, err := h.job.DropPartitionsBefore(ctx, date(13))
	if err != nil || len(dropped) != 0 {
		t.Errorf("dropped %v (err %v); the 10th has a visit scheduled after the watermark", dropped, err)
	}
}

// The all-routes row is computed from the visits, not from the per-route rows,
// so its percentiles are exact where combining rows could only approximate.
func TestRollup_AllRoutesRow_HasExactPercentilesAcrossRoutes(t *testing.T) {
	h := newHarness(t, sydney(12, 0))
	// Stop A at 08:00: route R has delays 0 and 10; route Q has 100, 200, 300.
	for i, v := range []int32{0, 10} {
		h.insert(t, row{serviceDate: date(23), trip: fmt.Sprintf("R%d", i), stop: "A", route: "R", delay: d(v), feedTS: sydney(8, 10), scheduledAt: sched(8, 10)})
	}
	for i, v := range []int32{100, 200, 300} {
		h.insert(t, row{serviceDate: date(23), trip: fmt.Sprintf("Q%d", i), stop: "A", route: "Q", delay: d(v), feedTS: sydney(8, 20), scheduledAt: sched(8, 20)})
	}

	h.rollup(t)

	var n, p50, p90 int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT n_obs, delay_p50_s, delay_p90_s FROM otp_stop_hourly WHERE stop_id = 'A' AND route_id = '~all' AND bucket_start = $1`,
		sydney(8, 0)).Scan(&n, &p50, &p90); err != nil {
		t.Fatalf("read all-routes row: %v", err)
	}
	// Over 0, 10, 100, 200, 300: the median is 100 and the 90th is 300. The
	// per-route rows alone (R p50 0, Q p50 200) could not give 100.
	if n != 5 || p50 != 100 || p90 != 300 {
		t.Errorf("all routes: n %d p50 %d p90 %d; want 5, 100, 300", n, p50, p90)
	}
}
