//go:build integration

package gtfsstatic

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/transitlateagain"
	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/feed"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/store"
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
	sub, err := fs.Sub(transitlateagain.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if _, err := s.Migrate(ctx, sub); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s.Pool()
}

// upstream stands in for the TfNSW schedule endpoint. It honours
// If-Modified-Since only when honour304 is set, because the real one
// republishes identical content under a new Last-Modified.
type upstream struct {
	mu           sync.Mutex
	body         []byte
	lastModified time.Time
	honour304    bool
	requests     int
	gotIMS       string
	fail         bool // answer 502, as the real endpoint did on 2026-09-23
}

func (u *upstream) set(body []byte, lm time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.body, u.lastModified = body, lm
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests++
	u.gotIMS = r.Header.Get("If-Modified-Since")
	if u.fail {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if u.honour304 && u.gotIMS == u.lastModified.Format(http.TimeFormat) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Last-Modified", u.lastModified.Format(http.TimeFormat))
	w.Write(u.body)
}

type harness struct {
	matcher *match.Matcher
	pool    *pgxpool.Pool
	up      *upstream
	loader  *Loader
	feed    config.Feed
}

func newHarness(t *testing.T, keep int) *harness {
	t.Helper()
	pool := testPool(t)
	up := &upstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return &harness{
		matcher: match.NewMatcher([]string{"sydneytrains"}, match.Options{DayOverlap: 6 * time.Hour, DateTolerance: 6 * time.Hour}),
		pool:    pool,
		up:      up,
		loader: NewLoader(pool, feed.NewClient("k", 10*time.Second, time.Now), feed.NewLimiter(100, 1000, time.Now),
			48*3600, keep, time.Now, slog.New(slog.DiscardHandler)),
		feed: config.Feed{ID: "sydneytrains", ScheduleURL: srv.URL},
	}
}

func (h *harness) load(t *testing.T) (int64, bool) {
	t.Helper()
	// The production budget for a schedule load (§10.1). The real bundle
	// needs it under -race.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	id, changed, err := h.loader.Load(ctx, h.feed)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return id, changed
}

func (h *harness) active(t *testing.T) (ids []int64) {
	t.Helper()
	r, err := h.pool.Query(context.Background(), `SELECT id FROM schedule_versions WHERE feed_id = $1 AND active`, h.feed.ID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for r.Next() {
		var id int64
		if err := r.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (h *harness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// gtfs is a minimal valid bundle: two trips, one past midnight, with a
// variant string so otherwise identical bundles can differ in content.
func gtfs(t *testing.T, variant string) []byte {
	return bundle(t, map[string]string{
		"agency.txt": "agency_id,agency_name\nSydneyTrains,Sydney Trains\n",
		"routes.txt": "route_id,agency_id,route_short_name,route_long_name,route_type\nAPS_1a,SydneyTrains,T8,Airport " + variant + ",2\n",
		"stops.txt":  "stop_id,stop_name,stop_lat,stop_lon,location_type,parent_station\n200060,Central,-33.88,151.2,1,\n2000341,Central Platform 4,-33.88,151.2,0,200060\n",
		"trips.txt":  "route_id,service_id,trip_id,trip_headsign,direction_id\nAPS_1a,SVC,T-1,Macarthur,0\nAPS_1a,SVC,T-2,City,1\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			"T-1,23:50:00,23:51:00,2000341,1\nT-1,25:10:00,25:10:00,200060,2\nT-2,08:00:00,08:00:30,2000341,1\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\nSVC,1,1,1,1,1,0,0,20260921,20261231\n",
		"shapes.txt":   "shape_id,shape_pt_lat,shape_pt_lon,shape_pt_sequence\nX,1,1,1\n",
	})
}

var lm1 = time.Date(2026, 9, 22, 15, 1, 13, 0, time.UTC)

func TestLoad_FirstBundle_LoadsEveryTableAndActivates(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)

	id, changed := h.load(t)

	if !changed {
		t.Error("first load reported unchanged")
	}
	if got := h.active(t); len(got) != 1 || got[0] != id {
		t.Fatalf("active = %v, want [%d]", got, id)
	}
	for table, want := range map[string]int{"routes": 1, "stops": 2, "trips": 2, "stop_times": 3, "calendar": 1, "calendar_dates": 0} {
		if n := h.count(t, `SELECT count(*) FROM `+table+` WHERE version_id = $1`, id); n != want {
			t.Errorf("%s: %d rows, want %d", table, n, want)
		}
	}
	if n := h.count(t, `SELECT arrival_s FROM stop_times WHERE version_id = $1 AND trip_id = 'T-1' AND stop_sequence = 2`, id); n != 90600 {
		t.Errorf("25:10:00 stored as %d, want 90600", n)
	}
	var trips, stopTimes int
	var storedLM time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT trip_count, stop_time_count, last_modified FROM schedule_versions WHERE id = $1`, id,
	).Scan(&trips, &stopTimes, &storedLM); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if trips != 2 || stopTimes != 3 || !storedLM.Equal(lm1) {
		t.Errorf("version row: trips %d, stop times %d, last modified %s", trips, stopTimes, storedLM)
	}
}

func TestLoad_Unchanged_IsANoOp(t *testing.T) {
	t.Run("a 304 is unchanged and sends the stored Last-Modified", func(t *testing.T) {
		h := newHarness(t, 3)
		h.up.honour304 = true
		h.up.set(gtfs(t, "a"), lm1)
		first, _ := h.load(t)

		second, changed := h.load(t)

		if changed || second != first {
			t.Errorf("second load = %d changed %v, want %d unchanged", second, changed, first)
		}
		if h.up.gotIMS != lm1.Format(http.TimeFormat) {
			t.Errorf("If-Modified-Since = %q, want %q", h.up.gotIMS, lm1.Format(http.TimeFormat))
		}
	})
	t.Run("identical content under a new Last-Modified adds no version", func(t *testing.T) {
		h := newHarness(t, 3)
		h.up.set(gtfs(t, "a"), lm1)
		first, _ := h.load(t)
		h.up.set(gtfs(t, "a"), lm1.Add(24*time.Hour))

		second, changed := h.load(t)

		if changed || second != first {
			t.Errorf("second load = %d changed %v, want %d unchanged", second, changed, first)
		}
		if n := h.count(t, `SELECT count(*) FROM schedule_versions`); n != 1 {
			t.Errorf("%d versions, want 1", n)
		}
	})
}

func TestLoad_NewContent_SwapsTheActiveVersion(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	old, _ := h.load(t)
	h.up.set(gtfs(t, "b"), lm1.Add(24*time.Hour))

	id, changed := h.load(t)

	if !changed || id == old {
		t.Fatalf("load = %d changed %v, want a new version", id, changed)
	}
	if got := h.active(t); len(got) != 1 || got[0] != id {
		t.Errorf("active = %v, want only the new version %d", got, id)
	}
	if n := h.count(t, `SELECT count(*) FROM trips WHERE version_id = $1`, old); n != 2 {
		t.Errorf("the previous version kept %d trips, want 2: it must stay loaded to roll back to", n)
	}

	t.Run("republishing the old content reactivates the old version", func(t *testing.T) {
		h.up.set(gtfs(t, "a"), lm1.Add(48*time.Hour))
		back, changed := h.load(t)
		if !changed || back != old {
			t.Errorf("load = %d changed %v, want %d reactivated", back, changed, old)
		}
		if got := h.active(t); len(got) != 1 || got[0] != old {
			t.Errorf("active = %v, want [%d]", got, old)
		}
	})
}

// A bad bundle must leave nothing behind and must not disturb the version
// being matched against (§9.1 step 2, §9.4 case 5).
func TestLoad_BadBundle_LeavesTheActiveVersionAndNoOrphans(t *testing.T) {
	good := gtfs(t, "a")
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"a stop time past the cap names its line", map[string]string{
			"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\nT-1,08:00:00,08:00:00,S,1\nT-1,49:00:00,49:00:00,S,2\n",
		}, "stop_times.txt line 3"},
		{"no trips is refused", map[string]string{"trips.txt": "route_id,service_id,trip_id\n"}, "refusing to activate an empty timetable"},
		{"no calendar of either kind is refused", map[string]string{"calendar.txt": ""}, "neither calendar.txt nor calendar_dates.txt"},
		{"a required file missing is refused (§9.1 case 16)", map[string]string{"stops.txt": ""}, "has no stops.txt"},
		{"a body that is not a zip is refused (§9.1 case 16)", nil, "open schedule zip"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, 3)
			h.up.set(good, lm1)
			before, _ := h.load(t)

			bad := []byte("<html>gateway error</html>")
			if c.files != nil {
				files := map[string]string{
					"routes.txt":     "route_id,route_type\nR,2\n",
					"stops.txt":      "stop_id,stop_name\nS,Stop\n",
					"trips.txt":      "route_id,service_id,trip_id\nR,SVC,T-1\n",
					"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\nT-1,08:00:00,08:00:00,S,1\n",
					"calendar.txt":   "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\nSVC,1,1,1,1,1,1,1,20260101,20261231\n",
				}
				for name, body := range c.files {
					if body == "" {
						delete(files, name)
					} else {
						files[name] = body
					}
				}
				bad = bundle(t, files)
			}
			h.up.set(bad, lm1.Add(time.Hour))

			_, _, err := h.loader.Load(context.Background(), h.feed)

			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if got := h.active(t); len(got) != 1 || got[0] != before {
				t.Errorf("active = %v, want the previous version %d untouched", got, before)
			}
			if n := h.count(t, `SELECT count(*) FROM schedule_versions`); n != 1 {
				t.Errorf("%d versions after a failed load, want 1: the failure left an orphan", n)
			}
		})
	}
}

func TestLoad_Retention_KeepsTheNewestVersions(t *testing.T) {
	h := newHarness(t, 2)
	var ids []int64
	for i, v := range []string{"a", "b", "c", "d"} {
		h.up.set(gtfs(t, v), lm1.Add(time.Duration(i)*24*time.Hour))
		id, _ := h.load(t)
		ids = append(ids, id)
	}

	if n := h.count(t, `SELECT count(*) FROM schedule_versions`); n != 2 {
		t.Errorf("%d versions kept, want 2", n)
	}
	if n := h.count(t, `SELECT count(*) FROM schedule_versions WHERE id = ANY($1)`, ids[2:]); n != 2 {
		t.Errorf("the two newest versions %v were not both kept", ids[2:])
	}
	if n := h.count(t, `SELECT count(*) FROM stop_times WHERE version_id = ANY($1)`, ids[:2]); n != 0 {
		t.Errorf("%d stop times left from pruned versions; the cascade did not run", n)
	}
}

// §12 Stage 2: the partial unique index, not application code, is what makes
// two active versions impossible.
func TestSchema_SecondActiveVersion_IsRejectedByTheIndex(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	h.load(t)
	h.up.set(gtfs(t, "b"), lm1.Add(time.Hour))
	h.load(t)

	_, err := h.pool.Exec(context.Background(), `UPDATE schedule_versions SET active = true WHERE feed_id = $1`, h.feed.ID)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("err = %v, want a unique violation (23505)", err)
	}
}

// The real bundle is 10.7 MB and not committed. Point GTFS_TEST_BUNDLE at
// a downloaded copy to measure a full load.
func TestLoad_RealBundle(t *testing.T) {
	path := os.Getenv("GTFS_TEST_BUNDLE")
	if path == "" {
		t.Skip("GTFS_TEST_BUNDLE is not set")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	h := newHarness(t, 3)
	h.up.set(body, lm1)

	start := time.Now()
	id, _ := h.load(t)
	elapsed := time.Since(start)

	trips := h.count(t, `SELECT trip_count FROM schedule_versions WHERE id = $1`, id)
	stopTimes := h.count(t, `SELECT stop_time_count FROM schedule_versions WHERE id = $1`, id)
	var size string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT pg_size_pretty(pg_total_relation_size('stop_times') + pg_total_relation_size('trips'))`).Scan(&size); err != nil {
		t.Fatalf("size: %v", err)
	}
	t.Logf("%d bytes -> %d trips, %d stop times in %s; stop_times + trips = %s", len(body), trips, stopTimes, elapsed.Round(time.Millisecond), size)
}

// Run loads at startup and returns promptly on cancellation, which main's
// shutdown relies on before it closes the pool.
func TestRun_LoadsAtStartupAndStopsOnCancel(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.loader.Run(ctx, []config.Feed{h.feed, {ID: "no-schedule"}}, Refresh{Hour: 3, Minute: 30, Interval: 24 * time.Hour}, h.matcher)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for h.matcher.Loaded(h.feed.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no version reached the matcher within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// The read-back is what the matcher trusts, so it must round-trip the
// details that matter: call order, times past midnight, blank times, and
// which days a service runs.
func TestSchedule_ReadBack_RoundTripsTheActiveVersion(t *testing.T) {
	h := newHarness(t, 3)
	b := bundle(t, map[string]string{
		"routes.txt":         "route_id,route_type\nAPS_1a,2\n",
		"stops.txt":          "stop_id,stop_name\nA,Alpha\nB,Beta\n",
		"trips.txt":          "route_id,service_id,trip_id,direction_id,trip_headsign\nAPS_1a,SVC,T-1,1,Macarthur\n",
		"stop_times.txt":     "trip_id,arrival_time,departure_time,stop_id,stop_sequence\nT-1,25:10:00,25:11:00,B,20\nT-1,,,A,3\n",
		"calendar.txt":       "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\nSVC,0,0,1,0,0,0,0,20260921,20261231\n",
		"calendar_dates.txt": "service_id,date,exception_type\nSVC,20260930,2\n",
	})
	h.up.set(b, lm1)
	id, _ := h.load(t)

	s, err := h.loader.Schedule(context.Background(), h.feed.ID)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	trip, ok := s.Trip("T-1")
	if s.VersionID != id || !ok {
		t.Fatalf("version %d, trip found %v", s.VersionID, ok)
	}
	if trip.RouteID != "APS_1a" || *trip.DirectionID != 1 || trip.Headsign != "Macarthur" || len(trip.Stops) != 2 {
		t.Fatalf("trip = %+v", trip)
	}
	if first := trip.Stops[0]; first.Seq != 3 || first.ArrS != match.NoTime || first.DepS != match.NoTime {
		t.Errorf("first call = %+v, want seq 3 with blank times: calls must come back in stop_sequence order", first)
	}
	if second := trip.Stops[1]; second.Seq != 20 || second.ArrS != 90600 || second.DepS != 90660 {
		t.Errorf("second call = %+v", second)
	}
	if r, ok := s.Route("APS_1a"); !ok || r.Type != 2 {
		t.Errorf("route = %+v, found %v", r, ok)
	}
	if name, ok := s.Stop("B"); !ok || name != "Beta" {
		t.Errorf("stop B = %q, found %v", name, ok)
	}
	wed := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	if !s.RunsOn("SVC", wed) || s.RunsOn("SVC", wed.AddDate(0, 0, 1)) || s.RunsOn("SVC", wed.AddDate(0, 0, 7)) {
		t.Error("RunsOn: want Wednesday 23rd yes, Thursday no, Wednesday 30th (removed) no")
	}

	if _, err := h.loader.Schedule(context.Background(), "other"); !errors.Is(err, ErrNoActiveSchedule) {
		t.Errorf("a feed with no version: err = %v, want ErrNoActiveSchedule", err)
	}
}

// Regression: on 2026-09-23 the schedule endpoint answered 502 at startup and
// the matcher stayed empty all run, although Postgres held an active version.
func TestRun_DownloadFailsAtStartup_PublishesTheVersionAlreadyActive(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	id, _ := h.load(t)
	h.up.mu.Lock()
	h.up.fail = true
	h.up.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.loader.Run(ctx, []config.Feed{h.feed}, Refresh{Hour: 3, Minute: 30, Interval: 24 * time.Hour}, h.matcher)
	}()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for h.matcher.Loaded(h.feed.ID) != id {
		if time.Now().After(deadline) {
			t.Fatalf("matcher has version %d after a failed download, want the active %d", h.matcher.Loaded(h.feed.ID), id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// On a restart the timetable is already in Postgres; Publish hands it to the
// matcher with no download at all.
func TestPublish_ActiveVersion_ReachesTheMatcherWithoutADownload(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	id, _ := h.load(t)
	before := h.up.requests

	if err := h.loader.Publish(context.Background(), []config.Feed{h.feed, {ID: "other"}}, h.matcher); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if h.matcher.Loaded(h.feed.ID) != id || h.up.requests != before {
		t.Errorf("matcher has %d (want %d) after %d downloads (want none)", h.matcher.Loaded(h.feed.ID), id, h.up.requests-before)
	}
}

// §12 Stage 3: several feeds each keep exactly one active version, and a new
// version for one never deactivates another's.
func TestLoad_SeveralFeeds_ActivateIndependently(t *testing.T) {
	h := newHarness(t, 3)
	h.up.set(gtfs(t, "a"), lm1)
	trains, _ := h.load(t)

	ferriesSrv := &upstream{}
	ferriesSrv.set(gtfs(t, "ferry"), lm1)
	srv := httptest.NewServer(ferriesSrv)
	t.Cleanup(srv.Close)
	ferries := config.Feed{ID: "sydneyferries", ScheduleURL: srv.URL}
	fid, _, err := h.loader.Load(context.Background(), ferries)
	if err != nil {
		t.Fatalf("load ferries: %v", err)
	}

	h.up.set(gtfs(t, "b"), lm1.Add(time.Hour))
	newTrains, _ := h.load(t)

	active := map[string]int64{}
	rows, err := h.pool.Query(context.Background(), `SELECT feed_id, id FROM schedule_versions WHERE active`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var f string
		var id int64
		if err := rows.Scan(&f, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		active[f] = id
	}
	if len(active) != 2 || active["sydneytrains"] != newTrains || active["sydneyferries"] != fid || newTrains == trains {
		t.Errorf("active = %v; want trains %d (not %d) and ferries %d", active, newTrains, trains, fid)
	}
}
