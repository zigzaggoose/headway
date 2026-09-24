package feed

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPoller wires a poller to a server with a poll interval short enough
// for a test and no jitter, so timing assertions stay deterministic.
func newTestPoller(t *testing.T, h http.HandlerFunc, got Handler) (*Poller, *httptest.Server, *Limiter) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	feed := config.Feed{ID: "sydneytrains", Label: "Sydney Trains", RealtimeURL: srv.URL, Enabled: true}
	client := NewClient(config.Secret(testKey), time.Second, time.Now)
	limiter := NewLimiter(1000, 100000, time.Now)
	p := NewPoller(feed, client, limiter, got, PollerOptions{
		Interval: 5 * time.Millisecond,
		Jitter:   0,
		Now:      time.Now,
		Logger:   quietLogger(),
	})
	return p, srv, limiter
}

func TestPoller_Run_PollsRepeatedlyAndHandsBodiesToTheHandler(t *testing.T) {
	var served atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte

	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			w.Write([]byte("body"))
		},
		func(_ context.Context, r Response) {
			mu.Lock()
			defer mu.Unlock()
			bodies = append(bodies, r.Body)
		})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil on cancellation", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("handler saw %d responses, want several", len(bodies))
	}
	if string(bodies[0]) != "body" {
		t.Errorf("body = %q", bodies[0])
	}
	// Every response the poller accepted reached the handler. The one
	// permitted discrepancy is a fetch that was in flight when cancellation
	// landed: the server served it, the poller correctly dropped it.
	switch lost := int(served.Load()) - len(bodies); {
	case lost < 0:
		t.Errorf("%d handler calls from %d requests", len(bodies), served.Load())
	case lost > 1:
		t.Errorf("%d requests produced only %d handler calls; at most one may be lost to cancellation", served.Load(), len(bodies))
	}
}

// §9.4 case 3: a feed never has two requests in flight. The loop is
// sequential, so a slow response delays the next poll instead of stacking
// behind it.
func TestPoller_SlowUpstream_NeverOverlapsRequests(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32

	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) {
			n := inFlight.Add(1)
			for {
				m := maxInFlight.Load()
				if n <= m || maxInFlight.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			inFlight.Add(-1)
			w.Write([]byte("body"))
		},
		func(context.Context, Response) {})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("%d concurrent requests to one feed, want at most 1", got)
	}
}

// §9.5 case 7: the quota is per account, so one feed's 403 must stop every
// feed, which it does by exhausting the shared limiter.
func TestPoller_QuotaResponse_StopsEveryFeed(t *testing.T) {
	p, _, limiter := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Error-Detail", "Account Over Quota Limit")
			w.WriteHeader(http.StatusForbidden)
		},
		func(context.Context, Response) { t.Error("handler called on a 403") })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	if _, err := limiter.reserve(time.Now()); err == nil {
		t.Error("the shared limiter still admits requests after a quota 403")
	}
}

// §9.4 case 7: a panic in a poller goroutine is recovered and that poller
// keeps going. One bad feed must not take down ingestion for the others.
func TestPoller_HandlerPanic_IsRecoveredAndPollingContinues(t *testing.T) {
	var calls atomic.Int32
	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("body")) },
		func(context.Context, Response) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
		})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("handler called %d times; polling did not resume after the panic", got)
	}
}

// §9.5 cases 4 and 8: neither an empty body nor a 401 may stop the poller, and
// neither may be mistaken for a successful poll.
func TestPoller_RecoverableFailures_KeepPolling(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"200 with an empty body", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }},
		{"401 unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }},
		{"500 from the gateway", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var served atomic.Int32
			p, _, _ := newTestPoller(t,
				func(w http.ResponseWriter, r *http.Request) { served.Add(1); tc.h(w, r) },
				func(context.Context, Response) { t.Error("handler called on a failed poll") })

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			if err := p.Run(ctx); err != nil {
				t.Fatalf("Run returned %v", err)
			}
			if served.Load() < 2 {
				t.Errorf("gave up after %d attempts", served.Load())
			}
		})
	}
}

func TestPoller_Cancellation_ReturnsPromptly(t *testing.T) {
	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("body")) },
		func(context.Context, Response) {})
	p.interval = time.Hour // parked waiting for the next tick

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within a second of cancellation")
	}
}

// The backoff schedule from §10.1, and the floor that keeps it from ever
// polling faster than FEED_POLL_INTERVAL.
func TestPoller_NextDelay_BacksOffWithinBounds(t *testing.T) {
	p := &Poller{interval: 15 * time.Second, jitter: 2 * time.Second}

	cases := []struct {
		failures int
		min, max time.Duration
	}{
		{0, 15 * time.Second, 17 * time.Second},
		{1, 15 * time.Second, 32 * time.Second},
		{2, 15 * time.Second, 62 * time.Second},
		{4, 15 * time.Second, 242 * time.Second},
		{8, 15 * time.Second, 5*time.Minute + 2*time.Second}, // capped at five minutes
		{99, 15 * time.Second, 5*time.Minute + 2*time.Second},
	}

	for _, tc := range cases {
		p.failures = tc.failures

		p.randFrac = func() float64 { return 0 }
		if got := p.nextDelay(); got != tc.min {
			t.Errorf("failures=%d lower bound = %s, want %s", tc.failures, got, tc.min)
		}

		p.randFrac = func() float64 { return 0.999999 }
		if got := p.nextDelay(); got > tc.max || got < tc.min {
			t.Errorf("failures=%d upper bound = %s, want <= %s", tc.failures, got, tc.max)
		}
	}
}

func TestPoller_NextDelay_ResetsAfterASuccessfulPoll(t *testing.T) {
	var served atomic.Int32
	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) {
			if served.Add(1) <= 2 {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte("body"))
		},
		func(context.Context, Response) {})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	if p.failures != 0 {
		t.Errorf("failures = %d after a successful poll, want 0", p.failures)
	}
}

// A fetch killed by our own shutdown is not a failure of the feed. It arrives
// as Canceled or DeadlineExceeded depending on which deadline fires first, and
// counting it would inflate the backoff and log a timeout that never happened.
func TestPoller_ShutdownDuringFetch_IsNotCountedAsAFailure(t *testing.T) {
	release := make(chan struct{})
	p, _, _ := newTestPoller(t,
		func(w http.ResponseWriter, r *http.Request) { <-release },
		func(context.Context, Response) { t.Error("handler called for an abandoned fetch") })
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if p.failures != 0 {
		t.Errorf("failures = %d after a shutdown mid-fetch, want 0", p.failures)
	}
}
