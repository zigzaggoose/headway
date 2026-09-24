package metrics

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var t0 = time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)

// Watch registers on the default registry, which panics on a second
// registration, so every test that needs it shares this one call.
var maintenanceErr error

func init() {
	Watch(Sources{
		FeedIDs:         []string{"trains"},
		QueueLen:        func() int { return 12 },
		QueueDropped:    func() uint64 { return 3 },
		RowsWritten:     func() uint64 { return 4200 },
		ScheduleVersion: func(string) int64 { return 41 },
		Stale:           func(string) bool { return true },
		Maintenance: func(context.Context) (Maintenance, error) {
			return Maintenance{Watermark: t0.Add(-2 * time.Hour), Partitions: 10}, maintenanceErr
		},
		Now: func() time.Time { return t0 },
	})
}

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// §10.3 is the list dashboards and alerts are written against. A vector
// appears only once a label value has been used, so every event-driven one is
// touched first; the rest come from Watch.
func TestHandler_ExposesEveryMetricInTheDesignDocument(t *testing.T) {
	FeedRequests.WithLabelValues("trains", "ok").Inc()
	FeedRequestDuration.WithLabelValues("trains").Observe(0.2)
	RecordPoll("trains", Poll{HeaderTS: t0, FetchedAt: t0.Add(7 * time.Second), Decoded: 1,
		Dropped: map[string]int{"no_time": 1}, Matched: map[string]int{"2": 1}, Unmatched: map[string]int{"added": 1}, MatchRate: 1})
	Filtered.WithLabelValues("trains").Inc()
	Admitted.WithLabelValues("trains").Inc()
	WriteBatchDuration.Observe(0.01)
	WriteFailures.WithLabelValues("true").Inc()
	FreshnessLag.WithLabelValues("trains").Observe(9)
	ScheduleLoadDuration.WithLabelValues("trains").Observe(20)
	RollupDuration.Observe(1)
	HTTPRequests.WithLabelValues("GET /v1/lines", "200").Inc()
	HTTPRequestDuration.WithLabelValues("GET /v1/lines").Observe(0.001)
	Panics.WithLabelValues("feed").Inc()

	body := scrape(t)

	for _, name := range []string{
		"feed_requests_total", "feed_request_duration_seconds", "feed_age_seconds", "feed_stale",
		"feed_skew_seconds", "updates_decoded_total", "updates_dropped_total", "matched_total",
		"unmatched_total", "match_rate", "filtered_total", "admitted_total", "queue_length",
		"dropped_total", "rows_written_total", "write_batch_duration_seconds", "write_failures_total",
		"freshness_lag_seconds", "schedule_version", "schedule_load_duration_seconds",
		"rollup_duration_seconds", "rollup_lag_seconds", "partitions", "default_partition_rows",
		"http_requests_total", "http_request_duration_seconds", "panics_total",
	} {
		if !strings.Contains(body, "\n# TYPE transitlateagain_"+name+" ") {
			t.Errorf("transitlateagain_%s is not exposed", name)
		}
	}
	if !strings.Contains(body, "process_start_time_seconds") {
		t.Error("process_start_time_seconds is missing; §1 measures uptime with it")
	}
}

func TestRecordPoll_SetsAgeAndSkewFromTheFeedsOwnTimestamp(t *testing.T) {
	RecordPoll("ferries", Poll{HeaderTS: t0, FetchedAt: t0.Add(9 * time.Second)})

	if got := testutil.ToFloat64(feedAge.WithLabelValues("ferries")); got != 9 {
		t.Errorf("feed age = %v, want 9", got)
	}
	if got := testutil.ToFloat64(feedSkew.WithLabelValues("ferries")); got != -9 {
		t.Errorf("feed skew = %v, want -9: the header is behind our clock", got)
	}
}

func TestRecordPoll_AccumulatesCountersAcrossPolls(t *testing.T) {
	before := testutil.ToFloat64(matched.WithLabelValues("metro", "1"))
	for range 2 {
		RecordPoll("metro", Poll{HeaderTS: t0, FetchedAt: t0, Matched: map[string]int{"1": 5}})
	}
	if got := testutil.ToFloat64(matched.WithLabelValues("metro", "1")) - before; got != 10 {
		t.Errorf("matched order 1 grew by %v over two polls of 5, want 10", got)
	}
}

func TestWatch_ReadsSnapshotsAtScrapeTime(t *testing.T) {
	t.Run("the maintenance values come from one query", func(t *testing.T) {
		body := scrape(t)
		for _, line := range []string{
			"transitlateagain_rollup_lag_seconds 7200",
			"transitlateagain_partitions 10",
			"transitlateagain_queue_length 12",
			`transitlateagain_dropped_total{reason="queue_full"} 3`,
			`transitlateagain_schedule_version{feed_id="trains"} 41`,
			`transitlateagain_feed_stale{feed_id="trains"} 1`,
		} {
			if !strings.Contains(body, "\n"+line+"\n") {
				t.Errorf("scrape lacks %q", line)
			}
		}
	})
	t.Run("a failing database drops its three gauges, not the scrape", func(t *testing.T) {
		maintenanceErr = errors.New("connection refused")
		defer func() { maintenanceErr = nil }()

		body := scrape(t)

		if strings.Contains(body, "transitlateagain_partitions ") {
			t.Error("partitions was reported with the database down")
		}
		if !strings.Contains(body, "transitlateagain_queue_length 12") {
			t.Error("the rest of the scrape was lost")
		}
	})
}
