package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCatalogue = `{"feeds":[
  {"id":"sydneytrains","label":"Sydney Trains","realtime_url":"https://api.example.test/v2/gtfs/realtime/sydneytrains","schedule_url":"https://api.example.test/v2/gtfs/schedule/sydneytrains","route_type":2,"enabled":true},
  {"id":"ferries","label":"Sydney Ferries","realtime_url":"https://api.example.test/v1/gtfs/realtime/ferries","schedule_url":"https://api.example.test/v1/gtfs/schedule/ferries","route_type":4,"enabled":false}
]}`

// baseEnv is the smallest environment that loads: the two required variables
// and a catalogue to point at.
func baseEnv(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feeds.json")
	if err := os.WriteFile(path, []byte(testCatalogue), 0o600); err != nil {
		t.Fatalf("write catalogue: %v", err)
	}
	return map[string]string{
		"TFNSW_API_KEY": "test-key",
		"DATABASE_URL":  "postgres://headway:hunter2@localhost:5432/headway?sslmode=disable",
		"FEEDS_FILE":    path,
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	return load(lookupFrom(env))
}

func TestLoad_MinimalEnvironment_AppliesDocumentedDefaults(t *testing.T) {
	cfg, err := loadWith(t, baseEnv(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Every default here is quoted from PROJECT.md §8. A change to one of them
	// is a change to that table first.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"FEED_POLL_INTERVAL", cfg.Poll.Interval, 15 * time.Second},
		{"FEED_POLL_JITTER", cfg.Poll.Jitter, 2 * time.Second},
		{"FEED_HTTP_TIMEOUT", cfg.Poll.HTTPTimeout, 20 * time.Second},
		{"FEED_RATE_LIMIT_RPS", cfg.Poll.RateLimit, 4.0},
		{"FEED_DAILY_BUDGET", cfg.Poll.DailyBudget, 55000},
		{"FEED_STALE_POLLS", cfg.Poll.StalePolls, 8},
		{"FEED_MAX_SKEW", cfg.Poll.MaxSkew, 5 * time.Minute},
		{"SCHEDULE_REFRESH_INTERVAL", cfg.Schedule.RefreshInterval, 24 * time.Hour},
		{"SCHEDULE_REFRESH_AT hour", cfg.Schedule.RefreshHour, 3},
		{"SCHEDULE_REFRESH_AT minute", cfg.Schedule.RefreshMinute, 30},
		{"SCHEDULE_KEEP_VERSIONS", cfg.Schedule.KeepVersions, 3},
		{"SERVICE_DAY_OVERLAP_H", cfg.Service.DayOverlap, 6 * time.Hour},
		{"SERVICE_DATE_TOLERANCE", cfg.Service.DateTolerance, 6 * time.Hour},
		{"SERVICE_TIME_MAX_S", cfg.Service.MaxStopTimeS, 172800},
		{"INGEST_QUEUE_SIZE", cfg.Ingest.QueueSize, 8192},
		{"INGEST_BATCH_SIZE", cfg.Ingest.BatchSize, 500},
		{"INGEST_FLUSH_INTERVAL", cfg.Ingest.FlushInterval, 2 * time.Second},
		{"FILTER_MIN_DELTA_S", cfg.Ingest.FilterMinDeltaS, 0},
		{"FILTER_MAX_ENTRIES", cfg.Ingest.FilterMaxEntries, 500000},
		{"DELAY_RECONCILE_TOLERANCE_S", cfg.Ingest.DelayReconcileToleranceS, 60},
		{"ON_TIME_EARLY_S", cfg.OnTime.EarlyS, -60},
		{"ON_TIME_LATE_S", cfg.OnTime.LateS, 300},
		{"ON_TIME_VERY_LATE_S", cfg.OnTime.VeryLateS, 900},
		{"MAINTENANCE_INTERVAL", cfg.Maintain.Interval, 15 * time.Minute},
		{"RETENTION_DAYS", cfg.Maintain.RetentionDays, 14},
		{"PARTITION_LOOKAHEAD_DAYS", cfg.Maintain.PartitionLookahead, 3},
		{"HTTP_ADDR", cfg.HTTP.Addr, ":8080"},
		{"HTTP_READ_TIMEOUT", cfg.HTTP.ReadTimeout, 10 * time.Second},
		{"HTTP_WRITE_TIMEOUT", cfg.HTTP.WriteTimeout, 30 * time.Second},
		{"HTTP_SHUTDOWN_GRACE", cfg.HTTP.ShutdownGrace, 20 * time.Second},
		{"HTTP_RATE_LIMIT_RPS", cfg.HTTP.RateLimit, 50.0},
		{"HISTORY_MAX_DAYS", cfg.HTTP.HistoryMaxDays, 90},
		{"CACHE_TTL", cfg.HTTP.CacheTTL, 45 * time.Minute},
		{"READY_MAX_FEED_AGE", cfg.HTTP.ReadyMaxFeedAge, 120 * time.Second},
		{"DB_MAX_CONNS", cfg.DB.MaxConns, 10},
		{"LOG_LEVEL", cfg.Log.Level, slog.LevelInfo},
		{"LOG_FORMAT", cfg.Log.Format, "json"},
		{"METRICS_ENABLED", cfg.Log.MetricsEnabled, true},
		{"TZ", cfg.Log.TZ, "Australia/Sydney"},
		{"ENABLED_FEEDS resolves to the enabled feeds", len(cfg.Feeds), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoad_RequiredVariableMissing_NamesIt(t *testing.T) {
	for _, key := range []string{"TFNSW_API_KEY", "DATABASE_URL"} {
		t.Run(key, func(t *testing.T) {
			env := baseEnv(t)
			delete(env, key)
			_, err := loadWith(t, env)
			if err == nil {
				t.Fatalf("load succeeded without %s", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error does not name the variable: %v", err)
			}
			// One mistake, one error: an unset DATABASE_URL must not also be
			// reported as a malformed one.
			if n := strings.Count(err.Error(), key); n != 1 {
				t.Errorf("%s reported %d times, want once: %v", key, n, err)
			}
		})
	}
}

// An empty value is a missing value: an unset variable and one set to "" are
// the same mistake, and a shell makes the second one easy to produce.
func TestLoad_RequiredVariableEmpty_IsTreatedAsMissing(t *testing.T) {
	env := baseEnv(t)
	env["TFNSW_API_KEY"] = "   "
	if _, err := loadWith(t, env); err == nil {
		t.Fatal("load succeeded with a blank TFNSW_API_KEY")
	}
}

func TestLoad_ValidatedRange_RejectsItsBoundary(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		accepted map[string]string
	}{
		{
			name:     "poll interval below five seconds",
			env:      map[string]string{"FEED_POLL_INTERVAL": "4999ms", "READY_MAX_FEED_AGE": "120s"},
			accepted: map[string]string{"FEED_POLL_INTERVAL": "5s"},
		},
		{
			name:     "one schedule version leaves nothing to fall back to",
			env:      map[string]string{"SCHEDULE_KEEP_VERSIONS": "1"},
			accepted: map[string]string{"SCHEDULE_KEEP_VERSIONS": "2"},
		},
		{
			name:     "no partition lookahead sends writes to the default partition",
			env:      map[string]string{"PARTITION_LOOKAHEAD_DAYS": "0"},
			accepted: map[string]string{"PARTITION_LOOKAHEAD_DAYS": "1"},
		},
		{
			name:     "zero day retention",
			env:      map[string]string{"RETENTION_DAYS": "0"},
			accepted: map[string]string{"RETENTION_DAYS": "1"},
		},
		{
			name:     "a single connection leaves the API none",
			env:      map[string]string{"DB_MAX_CONNS": "1"},
			accepted: map[string]string{"DB_MAX_CONNS": "2"},
		},
		{
			name:     "a stop time cap of exactly one day rejects legal past-midnight times",
			env:      map[string]string{"SERVICE_TIME_MAX_S": "86400"},
			accepted: map[string]string{"SERVICE_TIME_MAX_S": "86401"},
		},
		{
			name:     "zero day history range",
			env:      map[string]string{"HISTORY_MAX_DAYS": "0"},
			accepted: map[string]string{"HISTORY_MAX_DAYS": "1"},
		},
		{
			name:     "a batch that cannot fit in the queue",
			env:      map[string]string{"INGEST_QUEUE_SIZE": "100", "INGEST_BATCH_SIZE": "101"},
			accepted: map[string]string{"INGEST_QUEUE_SIZE": "100", "INGEST_BATCH_SIZE": "100"},
		},
		{
			name:     "shutdown grace equal to the flush interval loses the last batch",
			env:      map[string]string{"INGEST_FLUSH_INTERVAL": "2s", "HTTP_SHUTDOWN_GRACE": "2s"},
			accepted: map[string]string{"INGEST_FLUSH_INTERVAL": "2s", "HTTP_SHUTDOWN_GRACE": "2001ms"},
		},
		{
			name:     "readiness expiring within one poll interval flaps",
			env:      map[string]string{"FEED_POLL_INTERVAL": "15s", "READY_MAX_FEED_AGE": "15s"},
			accepted: map[string]string{"FEED_POLL_INTERVAL": "15s", "READY_MAX_FEED_AGE": "16s"},
		},
		{
			name:     "on-time bands out of order",
			env:      map[string]string{"ON_TIME_LATE_S": "900", "ON_TIME_VERY_LATE_S": "900"},
			accepted: map[string]string{"ON_TIME_LATE_S": "899", "ON_TIME_VERY_LATE_S": "900"},
		},
		{
			name:     "pprof on a public address",
			env:      map[string]string{"PPROF_ADDR": "0.0.0.0:6060"},
			accepted: map[string]string{"PPROF_ADDR": "127.0.0.1:6060"},
		},
		{
			name:     "pprof on an address with no port",
			env:      map[string]string{"PPROF_ADDR": "localhost"},
			accepted: map[string]string{"PPROF_ADDR": "localhost:6060"},
		},
		{
			name:     "a rate limit of zero admits no request",
			env:      map[string]string{"FEED_RATE_LIMIT_RPS": "0"},
			accepted: map[string]string{"FEED_RATE_LIMIT_RPS": "0.5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(t)
			for k, v := range tc.env {
				env[k] = v
			}
			if _, err := loadWith(t, env); err == nil {
				t.Errorf("load accepted %v", tc.env)
			}

			env = baseEnv(t)
			for k, v := range tc.accepted {
				env[k] = v
			}
			if _, err := loadWith(t, env); err != nil {
				t.Errorf("load rejected the accepted neighbour %v: %v", tc.accepted, err)
			}
		})
	}
}

func TestLoad_UnparseableValue_NamesTheVariable(t *testing.T) {
	cases := map[string]string{
		"FEED_POLL_INTERVAL":  "fifteen",
		"DB_MAX_CONNS":        "ten",
		"FEED_RATE_LIMIT_RPS": "four",
		"LOG_LEVEL":           "chatty",
		"LOG_FORMAT":          "yaml",
		"METRICS_ENABLED":     "maybe",
		"SCHEDULE_REFRESH_AT": "25:00",
	}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			env := baseEnv(t)
			env[key] = value
			_, err := loadWith(t, env)
			if err == nil {
				t.Fatalf("load accepted %s=%q", key, value)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error does not name the variable: %v", err)
			}
			if !strings.Contains(err.Error(), value) {
				t.Errorf("error does not quote the offending value: %v", err)
			}
		})
	}
}

// A misconfigured deployment should need one restart to learn about all of its
// mistakes, not one restart per mistake.
func TestLoad_SeveralProblems_AreAllReported(t *testing.T) {
	env := baseEnv(t)
	delete(env, "TFNSW_API_KEY")
	env["FEED_POLL_INTERVAL"] = "1s"
	env["DB_MAX_CONNS"] = "1"

	_, err := loadWith(t, env)
	if err == nil {
		t.Fatal("load succeeded")
	}
	for _, want := range []string{"TFNSW_API_KEY", "FEED_POLL_INTERVAL", "DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %s: %v", want, err)
		}
	}
}

// PROJECT.md §8.2: a Config must not leak the API key or the database password
// into a log line, whatever verb prints it.
func TestConfig_Printed_DoesNotRevealSecrets(t *testing.T) {
	env := baseEnv(t)
	env["TFNSW_API_KEY"] = "super-secret-key"
	cfg, err := loadWith(t, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rendered := []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		//lint:ignore S1025 the %s verb is the path under test, not a style choice
		fmt.Sprintf("%s", cfg.APIKey),
		fmt.Sprintf("%q", cfg.DatabaseURL),
		fmt.Sprint(*cfg),
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("startup", "config", cfg, "key", cfg.APIKey)
	rendered = append(rendered, buf.String())

	for _, out := range rendered {
		for _, secret := range []string{"super-secret-key", "hunter2"} {
			if strings.Contains(out, secret) {
				t.Errorf("rendered config contains %q: %s", secret, out)
			}
		}
	}

	// The value is still available where it is actually needed.
	if cfg.APIKey.Reveal() != "super-secret-key" {
		t.Errorf("Reveal() = %q", cfg.APIKey.Reveal())
	}
}
