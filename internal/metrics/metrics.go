// Package metrics holds the Prometheus metrics (§10.3). It imports nothing from
// this module, so every component can record into it without a cycle.
//
// The metrics are package variables on the default registry. The components
// that see an event record it where it happens, one line at the call site;
// values that are already kept as snapshots elsewhere (queue length, rows
// written, schedule versions) are read at scrape time instead, through the
// functions main passes to Watch.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "transitlateagain"

// The upstream answers in 0.1–2 s; the bundle download in tens of seconds.
var fetchBuckets = []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60}

var (
	FeedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "feed_requests_total", Help: "Every upstream request, by outcome.",
	}, []string{"feed_id", "outcome"})
	FeedRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "feed_request_duration_seconds", Help: "Upstream latency.", Buckets: fetchBuckets,
	}, []string{"feed_id"})
	feedAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "feed_age_seconds", Help: "Fetch time minus FeedHeader.timestamp at the last successful poll.",
	}, []string{"feed_id"})
	feedSkew = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "feed_skew_seconds", Help: "FeedHeader.timestamp minus our clock at the last successful poll.",
	}, []string{"feed_id"})
	updatesDecoded = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "updates_decoded_total", Help: "Stop-time updates decoded.",
	}, []string{"feed_id"})
	updatesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "updates_dropped_total", Help: "Updates dropped in decode or match, by reason.",
	}, []string{"feed_id", "reason"})
	matched = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "matched_total", Help: "Updates matched, by resolution order.",
	}, []string{"feed_id", "order"})
	unmatched = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "unmatched_total", Help: "Order-4 outcomes, by reason.",
	}, []string{"feed_id", "reason"})
	matchRate = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "match_rate", Help: "Match rate of the last poll, excluding ADDED trips.",
	}, []string{"feed_id"})

	Filtered = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "filtered_total", Help: "Observations suppressed by the change filter.",
	}, []string{"feed_id"})
	Admitted = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "admitted_total", Help: "Observations that passed the change filter.",
	}, []string{"feed_id"})
	WriteBatchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "write_batch_duration_seconds", Help: "Batch write latency, retries included.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})
	WriteFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "write_failures_total", Help: "Batches that failed after retries.",
	}, []string{"retryable"})
	// Freshness is dominated by INGEST_FLUSH_INTERVAL and the producer's own
	// lag, so the buckets are dense between 5 s and a minute.
	FreshnessLag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "freshness_lag_seconds", Help: "Write time minus feed_ts, per row written.",
		Buckets: []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 120, 300},
	}, []string{"feed_id"})

	ScheduleLoadDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "schedule_load_duration_seconds", Help: "Timetable download, parse and insert.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300},
	}, []string{"feed_id"})
	RollupDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "rollup_duration_seconds", Help: "One rollup pass.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	})

	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "http_requests_total", Help: "API requests, by ServeMux pattern and status.",
	}, []string{"route", "status"})
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "http_request_duration_seconds", Help: "API latency, by ServeMux pattern.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"route"})
	Panics = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "panics_total", Help: "Recovered panics, by component.",
	}, []string{"component"})
)

// Poll is what one decoded and matched poll contributes. The maps are keyed
// by label value.
type Poll struct {
	HeaderTS  time.Time
	FetchedAt time.Time
	Decoded   int
	Dropped   map[string]int // reason
	Matched   map[string]int // order
	Unmatched map[string]int // reason
	MatchRate float64
}

// RecordPoll adds one poll's counts. It is called from each feed's poller
// goroutine; the vectors are safe for that.
func RecordPoll(feedID string, p Poll) {
	age := p.FetchedAt.Sub(p.HeaderTS).Seconds()
	feedAge.WithLabelValues(feedID).Set(age)
	feedSkew.WithLabelValues(feedID).Set(-age)
	updatesDecoded.WithLabelValues(feedID).Add(float64(p.Decoded))
	for reason, n := range p.Dropped {
		updatesDropped.WithLabelValues(feedID, reason).Add(float64(n))
	}
	for order, n := range p.Matched {
		matched.WithLabelValues(feedID, order).Add(float64(n))
	}
	for reason, n := range p.Unmatched {
		unmatched.WithLabelValues(feedID, reason).Add(float64(n))
	}
	matchRate.WithLabelValues(feedID).Set(p.MatchRate)
}

// Maintenance is the database's side of the metrics.
type Maintenance struct {
	Watermark   time.Time
	Partitions  int
	DefaultRows int64
}

// Sources are the snapshots read at scrape time. Each must be safe to call
// from the scrape goroutine while the service runs.
type Sources struct {
	FeedIDs         []string
	QueueLen        func() int
	QueueDropped    func() uint64
	RowsWritten     func() uint64
	ScheduleVersion func(feedID string) int64
	// Stale reports whether a feed's header timestamp has not advanced for
	// FEED_STALE_POLLS successful polls.
	Stale       func(feedID string) bool
	Maintenance func(context.Context) (Maintenance, error)
	Now         func() time.Time
}

// Watch registers the scrape-time metrics. Call it once, from main.
func Watch(s Sources) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: ns, Name: "queue_length", Help: "Current ingest channel occupancy.",
	}, func() float64 { return float64(s.QueueLen()) })
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Namespace: ns, Name: "dropped_total", Help: "Observations dropped because the queue was full.",
		ConstLabels: prometheus.Labels{"reason": "queue_full"},
	}, func() float64 { return float64(s.QueueDropped()) })
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Namespace: ns, Name: "rows_written_total", Help: "Rows actually inserted, not attempted.",
	}, func() float64 { return float64(s.RowsWritten()) })
	for _, id := range s.FeedIDs {
		labels := prometheus.Labels{"feed_id": id}
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "schedule_version", Help: "Active schedule version_id; 0 before the first load.", ConstLabels: labels,
		}, func() float64 { return float64(s.ScheduleVersion(id)) })
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Name: "feed_stale", Help: "1 when the feed's header timestamp has stopped advancing.", ConstLabels: labels,
		}, func() float64 {
			if s.Stale(id) {
				return 1
			}
			return 0
		})
	}
	prometheus.MustRegister(maintenanceCollector{s})
}

// maintenanceCollector reads partition and rollup state in one query per
// scrape, rather than one per gauge.
type maintenanceCollector struct{ s Sources }

var (
	rollupLagDesc   = prometheus.NewDesc(ns+"_rollup_lag_seconds", "Now minus the rollup watermark.", nil, nil)
	partitionsDesc  = prometheus.NewDesc(ns+"_partitions", "Daily observation partitions.", nil, nil)
	defaultRowsDesc = prometheus.NewDesc(ns+"_default_partition_rows", "Rows in observations_default; should be 0.", nil, nil)
)

func (c maintenanceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- rollupLagDesc
	ch <- partitionsDesc
	ch <- defaultRowsDesc
}

func (c maintenanceCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := c.s.Maintenance(ctx)
	if err != nil {
		// A scrape without these three is better than a failed scrape: the
		// database being down shows up in every other signal already.
		return
	}
	ch <- prometheus.MustNewConstMetric(rollupLagDesc, prometheus.GaugeValue, c.s.Now().Sub(m.Watermark).Seconds())
	ch <- prometheus.MustNewConstMetric(partitionsDesc, prometheus.GaugeValue, float64(m.Partitions))
	ch <- prometheus.MustNewConstMetric(defaultRowsDesc, prometheus.GaugeValue, float64(m.DefaultRows))
}

// Handler serves the default registry, which also carries the Go runtime and
// process collectors (§1 uses process_start_time_seconds for uptime).
func Handler() http.Handler { return promhttp.Handler() }
