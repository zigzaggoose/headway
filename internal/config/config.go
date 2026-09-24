// Package config loads the process environment into a validated Config exactly
// once at startup. PROJECT.md §8 is the reference for every variable, its
// default, and what breaks when it is wrong.
//
// Nothing here reads the environment after Load returns, and nothing here
// exits: Load reports every problem it found at once and main decides what to
// do about it (§14 — no log.Fatal outside main).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"
)

// Secret is a string that must never reach a log line, an error message or a
// metric label. It is a type rather than a redact() helper at the call site
// because a helper can be forgotten and a type cannot: every print verb goes
// through String, GoString or MarshalText, all of which yield the same
// placeholder. Use Reveal at the one place the real value is needed, where it
// greps as an obvious exception.
type Secret string

// Redacted is what a Secret renders as, whatever the verb.
const Redacted = "[redacted]"

func (s Secret) String() string               { return Redacted }
func (s Secret) GoString() string             { return `"` + Redacted + `"` }
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }

// Reveal returns the underlying value. Call it only where the secret is used.
func (s Secret) Reveal() string { return string(s) }

// Config is the whole of Transit Late Again's configuration, grouped by the component
// that consumes it.
type Config struct {
	APIKey      Secret // TFNSW_API_KEY
	DatabaseURL Secret // DATABASE_URL, which carries a password

	FeedsFile string // FEEDS_FILE
	Feeds     []Feed // the resolved set to poll, not the whole catalogue

	Poll     PollConfig
	Schedule ScheduleConfig
	Service  ServiceConfig
	Ingest   IngestConfig
	OnTime   Thresholds
	Maintain MaintainConfig
	HTTP     HTTPConfig
	DB       DBConfig
	Log      LogConfig
}

// PollConfig governs how the feed pollers talk to the upstream API. The rate
// limit and the daily budget are per account, not per feed.
type PollConfig struct {
	Interval    time.Duration // FEED_POLL_INTERVAL
	Jitter      time.Duration // FEED_POLL_JITTER
	HTTPTimeout time.Duration // FEED_HTTP_TIMEOUT
	RateLimit   float64       // FEED_RATE_LIMIT_RPS
	DailyBudget int           // FEED_DAILY_BUDGET
	StalePolls  int           // FEED_STALE_POLLS
	MaxSkew     time.Duration // FEED_MAX_SKEW
}

// ScheduleConfig governs the daily static GTFS reload.
type ScheduleConfig struct {
	RefreshInterval time.Duration // SCHEDULE_REFRESH_INTERVAL
	RefreshHour     int           // SCHEDULE_REFRESH_AT, local
	RefreshMinute   int
	KeepVersions    int // SCHEDULE_KEEP_VERSIONS
}

// ServiceConfig governs the service-day arithmetic in internal/servicetime.
type ServiceConfig struct {
	DayOverlap    time.Duration // SERVICE_DAY_OVERLAP_H, as a duration
	DateTolerance time.Duration // SERVICE_DATE_TOLERANCE
	MaxStopTimeS  int           // SERVICE_TIME_MAX_S
}

// IngestConfig governs the bounded channel, the change filter and the writer.
type IngestConfig struct {
	QueueSize                int           // INGEST_QUEUE_SIZE
	BatchSize                int           // INGEST_BATCH_SIZE
	FlushInterval            time.Duration // INGEST_FLUSH_INTERVAL
	FilterMinDeltaS          int           // FILTER_MIN_DELTA_S
	FilterMaxEntries         int           // FILTER_MAX_ENTRIES
	DelayReconcileToleranceS int           // DELAY_RECONCILE_TOLERANCE_S
}

// Thresholds is Transit Late Again's on-time classification, in seconds of delay. It is
// Transit Late Again's own definition and not TfNSW's; see PROJECT.md §16 question 2.
// Changing any of these invalidates comparison with rollups already computed.
type Thresholds struct {
	EarlyS    int // ON_TIME_EARLY_S
	LateS     int // ON_TIME_LATE_S
	VeryLateS int // ON_TIME_VERY_LATE_S
}

// MaintainConfig governs rollup, retention and partition pre-creation.
type MaintainConfig struct {
	Interval           time.Duration // MAINTENANCE_INTERVAL
	RetentionDays      int           // RETENTION_DAYS
	PartitionLookahead int           // PARTITION_LOOKAHEAD_DAYS
}

// HTTPConfig governs the public read-only API.
type HTTPConfig struct {
	Addr            string        // HTTP_ADDR
	ReadTimeout     time.Duration // HTTP_READ_TIMEOUT
	WriteTimeout    time.Duration // HTTP_WRITE_TIMEOUT
	ShutdownGrace   time.Duration // HTTP_SHUTDOWN_GRACE
	RateLimit       float64       // HTTP_RATE_LIMIT_RPS, per IP
	HistoryMaxDays  int           // HISTORY_MAX_DAYS
	CacheTTL        time.Duration // CACHE_TTL
	ReadyMaxFeedAge time.Duration // READY_MAX_FEED_AGE
	PprofAddr       string        // PPROF_ADDR; empty means off
	AdminAddr       string        // ADMIN_ADDR; empty means off
}

// DBConfig governs the pgx pool.
type DBConfig struct {
	MaxConns int // DB_MAX_CONNS
}

// LogConfig governs slog and the metrics endpoint.
type LogConfig struct {
	Level          slog.Level // LOG_LEVEL
	Format         string     // LOG_FORMAT
	MetricsEnabled bool       // METRICS_ENABLED
	TZ             string     // TZ, which affects log rendering only
}

// Load reads and validates the environment. It returns every problem it found
// joined into one error, so a misconfigured deployment reports all of its
// mistakes in a single startup attempt rather than one per restart.
func Load() (*Config, error) { return load(os.LookupEnv) }

func load(lookup func(string) (string, bool)) (*Config, error) {
	l := &loader{lookup: lookup}

	cfg := &Config{
		APIKey:      Secret(l.required("TFNSW_API_KEY")),
		DatabaseURL: Secret(l.required("DATABASE_URL")),
		FeedsFile:   l.str("FEEDS_FILE", "config/feeds.json"),

		Poll: PollConfig{
			Interval:    l.dur("FEED_POLL_INTERVAL", 15*time.Second),
			Jitter:      l.dur("FEED_POLL_JITTER", 2*time.Second),
			HTTPTimeout: l.dur("FEED_HTTP_TIMEOUT", 20*time.Second),
			RateLimit:   l.float("FEED_RATE_LIMIT_RPS", 4),
			DailyBudget: l.int("FEED_DAILY_BUDGET", 55000),
			StalePolls:  l.int("FEED_STALE_POLLS", 8),
			MaxSkew:     l.dur("FEED_MAX_SKEW", 5*time.Minute),
		},
		Schedule: ScheduleConfig{
			RefreshInterval: l.dur("SCHEDULE_REFRESH_INTERVAL", 24*time.Hour),
			KeepVersions:    l.int("SCHEDULE_KEEP_VERSIONS", 3),
		},
		Service: ServiceConfig{
			DayOverlap:    time.Duration(l.int("SERVICE_DAY_OVERLAP_H", 6)) * time.Hour,
			DateTolerance: l.dur("SERVICE_DATE_TOLERANCE", 6*time.Hour),
			MaxStopTimeS:  l.int("SERVICE_TIME_MAX_S", 48*3600),
		},
		Ingest: IngestConfig{
			QueueSize:                l.int("INGEST_QUEUE_SIZE", 8192),
			BatchSize:                l.int("INGEST_BATCH_SIZE", 500),
			FlushInterval:            l.dur("INGEST_FLUSH_INTERVAL", 2*time.Second),
			FilterMinDeltaS:          l.int("FILTER_MIN_DELTA_S", 0),
			FilterMaxEntries:         l.int("FILTER_MAX_ENTRIES", 500000),
			DelayReconcileToleranceS: l.int("DELAY_RECONCILE_TOLERANCE_S", 60),
		},
		OnTime: Thresholds{
			EarlyS:    l.int("ON_TIME_EARLY_S", -60),
			LateS:     l.int("ON_TIME_LATE_S", 300),
			VeryLateS: l.int("ON_TIME_VERY_LATE_S", 900),
		},
		Maintain: MaintainConfig{
			Interval:           l.dur("MAINTENANCE_INTERVAL", 15*time.Minute),
			RetentionDays:      l.int("RETENTION_DAYS", 14),
			PartitionLookahead: l.int("PARTITION_LOOKAHEAD_DAYS", 3),
		},
		HTTP: HTTPConfig{
			Addr:            l.str("HTTP_ADDR", ":8080"),
			ReadTimeout:     l.dur("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    l.dur("HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownGrace:   l.dur("HTTP_SHUTDOWN_GRACE", 20*time.Second),
			RateLimit:       l.float("HTTP_RATE_LIMIT_RPS", 50),
			HistoryMaxDays:  l.int("HISTORY_MAX_DAYS", 90),
			CacheTTL:        l.dur("CACHE_TTL", 45*time.Minute),
			ReadyMaxFeedAge: l.dur("READY_MAX_FEED_AGE", 120*time.Second),
			PprofAddr:       l.str("PPROF_ADDR", ""),
			AdminAddr:       l.str("ADMIN_ADDR", "127.0.0.1:8081"),
		},
		DB: DBConfig{
			MaxConns: l.int("DB_MAX_CONNS", 10),
		},
		Log: LogConfig{
			Level:          l.level("LOG_LEVEL", slog.LevelInfo),
			Format:         l.enum("LOG_FORMAT", "json", "json", "text"),
			MetricsEnabled: l.boolean("METRICS_ENABLED", true),
			TZ:             l.str("TZ", "Australia/Sydney"),
		},
	}
	cfg.Schedule.RefreshHour, cfg.Schedule.RefreshMinute = l.timeOfDay("SCHEDULE_REFRESH_AT", 3, 30)

	// Feeds are resolved only when the environment itself parsed, so a typo in
	// a duration does not produce a second, confusing error about a file.
	if len(l.errs) == 0 {
		all, err := LoadFeeds(cfg.FeedsFile)
		if err != nil {
			l.errs = append(l.errs, err)
		} else {
			feeds, err := SelectFeeds(all, l.list("ENABLED_FEEDS"))
			if err != nil {
				l.errs = append(l.errs, err)
			}
			cfg.Feeds = feeds
		}
	}

	l.errs = append(l.errs, cfg.validate()...)
	if err := errors.Join(l.errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// validate holds the cross-field rules and the ranges §8 documents. A rule
// lives here only when violating it breaks something concrete; the column in
// §8 headed "what breaks if it is wrong" is the test for whether a bound
// belongs here or is merely advice.
func (c *Config) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// An unset DATABASE_URL has already been reported as missing; checking its
	// shape as well would print two errors for one mistake.
	if raw := c.DatabaseURL.Reveal(); raw != "" {
		if u, err := url.Parse(raw); err != nil {
			// The error text would carry the password, so it is not wrapped.
			bad("DATABASE_URL is not a valid URL")
		} else if u.Scheme != "postgres" && u.Scheme != "postgresql" {
			bad("DATABASE_URL has scheme %q, want postgres or postgresql", u.Scheme)
		}
	}

	// Below 5s several feeds together trip the upstream per-second throttle,
	// and the feeds only refresh every 15s, so it buys nothing either.
	if c.Poll.Interval < 5*time.Second {
		bad("FEED_POLL_INTERVAL is %s, minimum 5s", c.Poll.Interval)
	}
	if c.Poll.Jitter < 0 {
		bad("FEED_POLL_JITTER is negative (%s)", c.Poll.Jitter)
	}
	if c.Poll.HTTPTimeout <= 0 {
		bad("FEED_HTTP_TIMEOUT is not positive (%s)", c.Poll.HTTPTimeout)
	}
	if c.Poll.RateLimit <= 0 {
		bad("FEED_RATE_LIMIT_RPS is %g; a limiter at or below zero never admits a request", c.Poll.RateLimit)
	}
	if c.Poll.DailyBudget <= 0 {
		bad("FEED_DAILY_BUDGET is %d, want a positive request count", c.Poll.DailyBudget)
	}
	if c.Poll.StalePolls < 1 {
		bad("FEED_STALE_POLLS is %d, want at least 1", c.Poll.StalePolls)
	}
	if c.Poll.MaxSkew <= 0 {
		bad("FEED_MAX_SKEW is not positive (%s)", c.Poll.MaxSkew)
	}

	if c.Schedule.RefreshInterval <= 0 {
		bad("SCHEDULE_REFRESH_INTERVAL is not positive (%s)", c.Schedule.RefreshInterval)
	}
	// Below 2 there is no previous version left to fall back to when a freshly
	// published bundle turns out to be bad.
	if c.Schedule.KeepVersions < 2 {
		bad("SCHEDULE_KEEP_VERSIONS is %d, want at least 2", c.Schedule.KeepVersions)
	}

	if c.Service.DayOverlap < 0 || c.Service.DayOverlap >= 24*time.Hour {
		bad("SERVICE_DAY_OVERLAP_H is %s, want 0 to 23 hours", c.Service.DayOverlap)
	}
	if c.Service.DateTolerance <= 0 {
		bad("SERVICE_DATE_TOLERANCE is not positive (%s)", c.Service.DateTolerance)
	}
	if c.Service.MaxStopTimeS <= 86400 {
		bad("SERVICE_TIME_MAX_S is %d, which rejects legal past-midnight stop times above 24:00:00", c.Service.MaxStopTimeS)
	}

	if c.Ingest.QueueSize < 1 {
		bad("INGEST_QUEUE_SIZE is %d, want at least 1", c.Ingest.QueueSize)
	}
	if c.Ingest.BatchSize < 1 {
		bad("INGEST_BATCH_SIZE is %d, want at least 1", c.Ingest.BatchSize)
	}
	// A batch larger than the queue can never be filled by size, so the writer
	// would only ever flush on the interval.
	if c.Ingest.BatchSize > c.Ingest.QueueSize {
		bad("INGEST_BATCH_SIZE (%d) exceeds INGEST_QUEUE_SIZE (%d)", c.Ingest.BatchSize, c.Ingest.QueueSize)
	}
	if c.Ingest.FlushInterval <= 0 {
		bad("INGEST_FLUSH_INTERVAL is not positive (%s)", c.Ingest.FlushInterval)
	}
	if c.Ingest.FilterMinDeltaS < 0 {
		bad("FILTER_MIN_DELTA_S is negative (%d)", c.Ingest.FilterMinDeltaS)
	}
	if c.Ingest.FilterMaxEntries < 1 {
		bad("FILTER_MAX_ENTRIES is %d, want at least 1", c.Ingest.FilterMaxEntries)
	}
	if c.Ingest.DelayReconcileToleranceS < 0 {
		bad("DELAY_RECONCILE_TOLERANCE_S is negative (%d)", c.Ingest.DelayReconcileToleranceS)
	}

	// The classification is a set of ordered bands; out of order, a rollup
	// would count the same observation into two buckets or none.
	if !(c.OnTime.EarlyS < c.OnTime.LateS && c.OnTime.LateS < c.OnTime.VeryLateS) {
		bad("on-time thresholds are out of order: ON_TIME_EARLY_S (%d) < ON_TIME_LATE_S (%d) < ON_TIME_VERY_LATE_S (%d) must hold",
			c.OnTime.EarlyS, c.OnTime.LateS, c.OnTime.VeryLateS)
	}

	if c.Maintain.Interval <= 0 {
		bad("MAINTENANCE_INTERVAL is not positive (%s)", c.Maintain.Interval)
	}
	if c.Maintain.RetentionDays < 1 {
		bad("RETENTION_DAYS is %d, want at least 1", c.Maintain.RetentionDays)
	}
	// At zero, today's partition is the last one that exists and tomorrow's
	// writes fall into observations_default, which cannot be dropped.
	if c.Maintain.PartitionLookahead < 1 {
		bad("PARTITION_LOOKAHEAD_DAYS is %d, want at least 1", c.Maintain.PartitionLookahead)
	}

	if c.HTTP.Addr == "" {
		bad("HTTP_ADDR is empty")
	}
	if c.HTTP.ReadTimeout <= 0 {
		bad("HTTP_READ_TIMEOUT is not positive (%s)", c.HTTP.ReadTimeout)
	}
	if c.HTTP.WriteTimeout <= 0 {
		bad("HTTP_WRITE_TIMEOUT is not positive (%s)", c.HTTP.WriteTimeout)
	}
	// Shutdown must outlast a pending flush or the final batch is lost, which
	// is the one failure §9.4 refuses to accept.
	if c.HTTP.ShutdownGrace <= c.Ingest.FlushInterval {
		bad("HTTP_SHUTDOWN_GRACE (%s) must exceed INGEST_FLUSH_INTERVAL (%s)", c.HTTP.ShutdownGrace, c.Ingest.FlushInterval)
	}
	if c.HTTP.RateLimit <= 0 {
		bad("HTTP_RATE_LIMIT_RPS is %g; at or below zero no request is ever served", c.HTTP.RateLimit)
	}
	if c.HTTP.HistoryMaxDays < 1 {
		bad("HISTORY_MAX_DAYS is %d, want at least 1", c.HTTP.HistoryMaxDays)
	}
	if c.HTTP.CacheTTL <= 0 {
		bad("CACHE_TTL is not positive (%s)", c.HTTP.CacheTTL)
	}
	// Readiness would flap if a single missed poll could expire it.
	if c.HTTP.ReadyMaxFeedAge <= c.Poll.Interval {
		bad("READY_MAX_FEED_AGE (%s) must exceed FEED_POLL_INTERVAL (%s)", c.HTTP.ReadyMaxFeedAge, c.Poll.Interval)
	}

	// pprof exposes goroutine stacks and heap contents; it must never listen
	// where anyone but the operator on the machine can reach it.
	if a := c.HTTP.PprofAddr; a != "" {
		host, _, err := net.SplitHostPort(a)
		if ip := net.ParseIP(host); err != nil || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			bad("PPROF_ADDR is %q, want a loopback host:port such as 127.0.0.1:6060", a)
		}
	}

	// The writer holds one connection; anything less leaves the API none.
	if c.DB.MaxConns < 2 {
		bad("DB_MAX_CONNS is %d, want at least 2: the writer holds one and the API needs the rest", c.DB.MaxConns)
	}

	return errs
}
