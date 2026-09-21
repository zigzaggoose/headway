// Command headway polls TfNSW GTFS-realtime feeds, matches each stop-time
// update against the published timetable, and serves the resulting delay
// observations over HTTP. See PROJECT.md for the design.
package main

import (
	"context"
	"fmt"
	"log/slog"
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

	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/feed"
)

func main() {
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

	// Shutdown is ordered and the order matters (§9.4): cancel the pollers,
	// then wait for every producer to return, and only then close what they
	// write into. The later steps arrive with the writer.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One client and one limiter for every feed: the connection pool is worth
	// sharing, and both the rate limit and the daily quota are per account.
	client := feed.NewClient(cfg.APIKey, cfg.Poll.HTTPTimeout, time.Now)
	limiter := feed.NewLimiter(cfg.Poll.RateLimit, cfg.Poll.DailyBudget, time.Now)

	var pollers sync.WaitGroup
	for _, f := range cfg.Feeds {
		p := feed.NewPoller(f, client, limiter, decodeAndIngest(log), feed.PollerOptions{
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
	pollers.Wait() // step 3: no new observations can be produced
	log.Info("shutdown complete", "component", "main", "requests_today", limiter.Used())
}

// decodeAndIngest is where internal/gtfsrt and internal/match will hang. Until
// they exist the handler records that bytes arrived, at debug: a line per poll
// is far above the rate §10.2 allows for info.
func decodeAndIngest(log *slog.Logger) feed.Handler {
	return func(_ context.Context, r feed.Response) {
		log.Debug("feed fetched",
			"component", "feed",
			"feed_id", r.FeedID,
			"bytes", len(r.Body),
			"fetched_at", r.FetchedAt.Format(time.RFC3339),
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
