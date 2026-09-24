// Command transitlateagain polls TfNSW GTFS-realtime feeds, matches each stop-time
// update against the published timetable, and serves the resulting delay
// observations over HTTP. See PROJECT.md for the design.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	// The runtime image is distroless and carries no timezone database. Without
	// this import time.LoadLocation("Australia/Sydney") fails, and code that
	// ignores that error runs in UTC, which is wrong by ten or eleven hours and
	// corrupts every service date. PROJECT.md §9.2.
	_ "time/tzdata"

	"github.com/zigzaggoose/transitlateagain"
	"github.com/zigzaggoose/transitlateagain/internal/api"
	"github.com/zigzaggoose/transitlateagain/internal/cache"
	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/feed"
	"github.com/zigzaggoose/transitlateagain/internal/gtfsrt"
	"github.com/zigzaggoose/transitlateagain/internal/gtfsstatic"
	"github.com/zigzaggoose/transitlateagain/internal/ingest"
	"github.com/zigzaggoose/transitlateagain/internal/match"
	"github.com/zigzaggoose/transitlateagain/internal/metrics"
	"github.com/zigzaggoose/transitlateagain/internal/rollup"
	"github.com/zigzaggoose/transitlateagain/internal/store"
)

func main() {
	// Migrations run at startup either way; this flag only stops afterwards,
	// so `make migrate` and a starting container take the same code path.
	migrateOnly := flag.Bool("migrate-only", false, "apply database migrations and exit")
	// One maintenance tick — partitions, rollup, retention — then exit. For
	// the disk-full runbook (§10.3): RETENTION_DAYS=3 transitlateagain -maintain-once.
	// A flag rather than cmd/maintain for the reason -migrate-only is one: the
	// distroless image carries exactly one binary.
	maintainOnce := flag.Bool("maintain-once", false, "run one maintenance tick and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		// No logger yet: the configuration is what says how to log. Exit code 2
		// is reserved for a configuration the process refuses to start with, so
		// a restart loop is distinguishable from a crash loop (§10.1).
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	log := newLogger(cfg)
	log.Info("starting",
		"component", "main",
		"feeds", feedIDs(cfg),
		"http_addr", cfg.HTTP.Addr,
		"poll_interval", cfg.Poll.Interval.String(),
		"retention_days", cfg.Maintain.RetentionDays,
	)

	// An unreachable database at startup is fatal; at runtime it is not
	// (§10.1). Exit code 2 covers both refusals to start.
	db, err := store.Open(context.Background(), cfg.DatabaseURL, cfg.DB.MaxConns, log)
	if err != nil {
		log.Error("cannot start", "component", "main", "err", err.Error())
		os.Exit(2)
	}

	migrations, err := fs.Sub(transitlateagain.Migrations, "migrations")
	if err != nil {
		log.Error("cannot start", "component", "main", "err", err.Error())
		os.Exit(2)
	}
	applied, err := db.Migrate(context.Background(), migrations)
	if err != nil {
		log.Error("cannot start", "component", "main", "err", err.Error())
		os.Exit(2)
	}
	if len(applied) > 0 {
		log.Info("migrations applied", "component", "main", "count", len(applied), "versions", applied)
	}
	if *migrateOnly {
		log.Info("migrate-only: nothing left to do", "component", "main")
		db.Close()
		return
	}
	if *maintainOnce {
		job := rollup.New(db.Pool(), rollup.Options{
			OnTime:        cfg.OnTime,
			RetentionDays: cfg.Maintain.RetentionDays,
			LookaheadDays: cfg.Maintain.PartitionLookahead,
			Settle:        rollupSettle,
		}, time.Now, log)
		tickCtx, cancelTick := context.WithTimeout(context.Background(), 10*time.Minute)
		err := job.Tick(tickCtx)
		cancelTick()
		db.Close()
		if err != nil {
			log.Error("maintenance failed", "component", "main", "err", err.Error())
			os.Exit(1)
		}
		log.Info("maintenance done", "component", "main")
		return
	}

	// Bind before anything starts, so a port already in use is a refusal to
	// start (exit 2) rather than a service that ingests with no API.
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		log.Error("cannot start", "component", "main", "err", err.Error())
		db.Close()
		os.Exit(2)
	}
	// On the VM the API serves Cloudflare's origin certificate, so the hop
	// from Cloudflare is encrypted too (Full (strict), §15). Loaded here so a
	// missing or mismatched file refuses to start instead of failing the
	// first handshake.
	if cfg.HTTP.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile)
		if err != nil {
			log.Error("cannot start", "component", "main", "err", err.Error())
			db.Close()
			os.Exit(2)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}

	pipeline := ingest.NewPipeline(db.Pool(), ingest.PipelineConfig{
		QueueSize:        cfg.Ingest.QueueSize,
		BatchSize:        cfg.Ingest.BatchSize,
		FlushInterval:    cfg.Ingest.FlushInterval,
		FilterMinDeltaS:  int32(cfg.Ingest.FilterMinDeltaS),
		FilterMaxEntries: cfg.Ingest.FilterMaxEntries,
	}, log)
	pipeline.Start()
	latest := cache.New(cfg.HTTP.CacheTTL, time.Now)
	// One client and one limiter for every feed: the connection pool is worth
	// sharing, and both the rate limit and the daily quota are per account.
	client := feed.NewClient(cfg.APIKey, cfg.Poll.HTTPTimeout, time.Now)
	limiter := feed.NewLimiter(cfg.Poll.RateLimit, cfg.Poll.DailyBudget, time.Now)

	matcher := match.NewMatcher(feedIDs(cfg), match.Options{
		DayOverlap:    cfg.Service.DayOverlap,
		DateTolerance: cfg.Service.DateTolerance,
		ReconcileS:    int32(cfg.Ingest.DelayReconcileToleranceS),
	})

	// Shutdown is ordered and the order matters (§9.4): cancel the pollers,
	// drain HTTP, wait for every producer to return, and only then close what
	// they write into.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiOpts := api.Options{
		Cache: latest,
		Ping:  db.Ping,
		ScheduleLoaded: func() bool {
			for _, f := range cfg.Feeds {
				if matcher.Loaded(f.ID) != 0 {
					return true
				}
			}
			return false
		},
		OnTime:          cfg.OnTime,
		ReadyMaxFeedAge: cfg.HTTP.ReadyMaxFeedAge,
		Schedules:       matcher.Schedules,
		History:         db.History,
		TripStops:       db.TripStops,
		HistoryMaxDays:  cfg.HTTP.HistoryMaxDays,
		RateLimit:       cfg.HTTP.RateLimit,
		ClientIPHeader:  cfg.HTTP.ClientIPHeader,
		CORSOrigin:      cfg.HTTP.CORSOrigin,
		PipelineStats:   pipeline.Stats,
		MatchCounts:     matcher.LastCounts,
		RequestsToday:   limiter.Used,
		Maintenance:     db.Maintenance,
		Now:             time.Now,
		Log:             log,
	}
	if cfg.Log.MetricsEnabled {
		metrics.Watch(metrics.Sources{
			FeedIDs:         feedIDs(cfg),
			QueueLen:        func() int { return pipeline.Stats().QueueLen },
			QueueDropped:    func() uint64 { return pipeline.Stats().Dropped },
			RowsWritten:     func() uint64 { return pipeline.Stats().Written },
			ScheduleVersion: matcher.Loaded,
			Stale:           func(id string) bool { return latest.Stale(id, cfg.Poll.StalePolls) },
			Maintenance: func(ctx context.Context) (metrics.Maintenance, error) {
				m, err := db.Maintenance(ctx)
				return metrics.Maintenance{Watermark: m.Watermark, Partitions: m.Partitions, DefaultRows: m.DefaultRows}, err
			},
			Now: time.Now,
		})
		apiOpts.Metrics = metrics.Handler()
	}
	srv := &http.Server{
		Handler:           api.NewHandler(apiOpts),
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			// The API dying is not a reason to lose ingestion silently, and
			// not a reason to keep running without it either: shut down in
			// order and exit non-zero so the restart policy brings it back.
			serveErr <- err
			stop()
		}
	}()
	log.Info("http listening", "component", "main", "addr", ln.Addr().String())

	// Profiling, opt-in and loopback-only (config enforces it): a second
	// server so its routes can never be reached through the API's address.
	var profiler *http.Server
	if cfg.HTTP.PprofAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		profiler = &http.Server{Addr: cfg.HTTP.PprofAddr, Handler: mux, ReadHeaderTimeout: cfg.HTTP.ReadTimeout}
		go func() {
			if err := profiler.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				// Losing the profiler is no reason to stop ingesting.
				log.Error("pprof server stopped", "component", "main", "err", err.Error())
			}
		}()
		log.Warn("pprof listening", "component", "main", "addr", cfg.HTTP.PprofAddr)
	}

	// Admin stats on their own server, like the profiler, but not forced to
	// loopback: in Compose the container must listen on all interfaces, and
	// the host-side port binding (127.0.0.1 only) is what keeps it private.
	var admin *http.Server
	if cfg.HTTP.AdminAddr != "" {
		admin = &http.Server{
			Addr:              cfg.HTTP.AdminAddr,
			Handler:           api.NewAdminHandler(apiOpts),
			ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
		}
		go func() {
			if err := admin.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				// Losing the stats page is no reason to stop ingesting.
				log.Error("admin server stopped", "component", "main", "err", err.Error())
			}
		}()
		log.Info("admin listening", "component", "main", "addr", cfg.HTTP.AdminAddr)
	}

	// The schedule gets its own client because its timeout is the schedule's
	// (§10.1), not a realtime poll's: the bundle is 11 MB. It shares the
	// limiter, because the quota is per account.
	schedules := gtfsstatic.NewLoader(db.Pool(), feed.NewClient(cfg.APIKey, 5*time.Minute, time.Now), limiter,
		cfg.Service.MaxStopTimeS, cfg.Schedule.KeepVersions, time.Now, log)
	// The timetable already in Postgres goes to the matcher before any poller
	// starts, so a restart's first poll is matched. A fresh database has none,
	// and its first polls are unmatched until the download finishes.
	pubCtx, cancelPub := context.WithTimeout(ctx, time.Minute)
	if err := schedules.Publish(pubCtx, cfg.Feeds, matcher); err != nil {
		log.Error("stored schedule not published at startup", "component", "main", "err", err.Error())
	}
	cancelPub()

	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		schedules.Run(ctx, cfg.Feeds, gtfsstatic.Refresh{
			Hour: cfg.Schedule.RefreshHour, Minute: cfg.Schedule.RefreshMinute, Interval: cfg.Schedule.RefreshInterval,
		}, matcher)
	}()

	maintenance := rollup.New(db.Pool(), rollup.Options{
		OnTime:        cfg.OnTime,
		RetentionDays: cfg.Maintain.RetentionDays,
		LookaheadDays: cfg.Maintain.PartitionLookahead,
		Interval:      cfg.Maintain.Interval,
		Settle:        rollupSettle,
		ExpireFilter:  func(before time.Time) { pipeline.ExpireFilterBefore(before) },
	}, time.Now, log)
	background.Add(1)
	go func() {
		defer background.Done()
		maintenance.Run(ctx)
	}()

	var pollers sync.WaitGroup
	for _, f := range cfg.Feeds {
		p := feed.NewPoller(f, client, limiter, decodeAndIngest(log, matcher, pipeline, latest), feed.PollerOptions{
			Interval: cfg.Poll.Interval,
			Jitter:   cfg.Poll.Jitter,
			Now:      time.Now,
			Logger:   log,
		})
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			if err := p.Run(ctx); err != nil {
				log.Error("poller exited", "component", "main", "feed_id", f.ID, "err", err.Error())
			}
		}()
	}

	<-ctx.Done()
	stop()

	log.Info("shutdown started", "component", "main")
	// Step 2. The deadline is its own, not ctx: ctx is already cancelled.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownGrace)
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Error("http did not drain", "component", "main", "err", err.Error())
	}
	if profiler != nil {
		_ = profiler.Close() // a profile in progress is not worth waiting for
	}
	if admin != nil {
		_ = admin.Close() // a stats snapshot in progress is not worth waiting for
	}
	cancelDrain()
	pollers.Wait()    // step 3: no new observations can be produced
	background.Wait() // schedule load and maintenance have stopped and let go of the pool
	// Steps 4 and 5: close the channel and let the writer flush. The grace
	// is the HTTP one because config already requires it to exceed a flush.
	if err := pipeline.Close(cfg.HTTP.ShutdownGrace); err != nil {
		log.Error("writer did not drain", "component", "main", "err", err.Error())
	}
	db.Close() // step 6: after the writer has finished, never before

	st := pipeline.Stats()
	log.Info("shutdown complete",
		"component", "main",
		"requests_today", limiter.Used(),
		"rows_written", st.Written,
		"rows_failed", st.Failed,
		"dropped", st.Dropped,
	)
	select {
	case err := <-serveErr:
		log.Error("exiting after the http server failed", "component", "main", "err", err.Error())
		os.Exit(1)
	default:
	}
}

// rollupSettle is how long after an hour ends it is rolled up: a train later
// than three hours is rare, and /history is then at most four hours behind
// (§16 q12).
const rollupSettle = 3 * time.Hour

// decodeAndIngest decodes and converts on the poller's goroutine (§9.4), then
// publishes the poll to the latest-state cache and hands each observation to
// the pipeline. The cache takes every observation, before the change filter,
// because "unchanged since the last write" still means "current". It reports at
// debug because a line per poll is far above the rate §10.2 allows for info.
func decodeAndIngest(log *slog.Logger, matcher *match.Matcher, pipeline *ingest.Pipeline, latest *cache.Cache) feed.Handler {
	return func(_ context.Context, r feed.Response) {
		decoded, err := gtfsrt.Decode(r.FeedID, r.Body, r.FetchedAt)
		if err != nil {
			// A gateway error page served with a 200 lands here, not in the
			// poller: the transport succeeded and the payload did not.
			log.Error("decode failed", "component", "gtfsrt", "feed_id", r.FeedID, "err", err.Error())
			return
		}
		obs, counts := matcher.Match(r.FeedID, decoded.Updates)
		latest.Update(r.FeedID, decoded.HeaderTS, obs)
		matchedBy, unmatchedBy, dropped := counts.Labels()
		for reason, n := range decoded.Dropped {
			dropped[string(reason)] += n
		}
		metrics.RecordPoll(r.FeedID, metrics.Poll{
			HeaderTS:  decoded.HeaderTS,
			FetchedAt: decoded.FetchedAt,
			Decoded:   len(decoded.Updates),
			Dropped:   dropped,
			Matched:   matchedBy,
			Unmatched: unmatchedBy,
			MatchRate: counts.MatchRate(),
		})
		queued := 0
		for _, o := range obs {
			if pipeline.Submit(o) {
				queued++
			}
		}
		log.Debug("feed ingested",
			"component", "gtfsrt",
			"feed_id", r.FeedID,
			"bytes", len(r.Body),
			"entities", decoded.Entities,
			"updates", len(decoded.Updates),
			"dropped", decoded.TotalDropped(),
			"feed_age_s", decoded.Age().Seconds(),
			"header_ts_missing", decoded.HeaderTSMissing,
			"observations", len(obs),
			"queued", queued,
			"match_rate", counts.MatchRate(),
			"full_match", counts.FullMatch,
			"trip_only", counts.TripOnly,
			"unknown_trip", counts.UnknownTrip,
			"added", counts.Added,
			"no_schedule", counts.NoSchedule,
			"cancelled_stops", counts.CancelledStops,
			"skipped_trip_level", counts.TripLevelSkipped,
			"delay_disagreements", counts.DelayDisagreements,
		)
	}
}

// newLogger builds the process logger. It moves to internal/obs when that
// package arrives with the standard field names and the metrics collectors.
func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Log.Level}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.Log.Format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func feedIDs(cfg *config.Config) []string {
	out := make([]string, len(cfg.Feeds))
	for i, f := range cfg.Feeds {
		out[i] = f.ID
	}
	return out
}
