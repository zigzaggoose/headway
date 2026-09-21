package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"time"

	"github.com/zigzaggoose/headway/internal/config"
)

// Handler receives one fetched body. It is called on the poller's own
// goroutine and the next fetch does not start until it returns, so decoding
// and matching happen inline rather than across a channel: at four polls a
// minute per feed the work is short and CPU-bound, and a channel here would
// add a hop, a buffer to size and a second shutdown ordering problem for no
// measured gain. PROJECT.md §9.4. Stage 3 may put a bounded worker pool behind
// this signature without changing it, if profiling shows a reason.
type Handler func(context.Context, Response)

// Poller fetches one feed on a schedule. Exactly one goroutine runs each
// poller, and it fetches sequentially, so a feed never has two requests in
// flight and a slow response delays the next poll rather than stacking behind
// it (§9.4 cases 3 and 4).
type Poller struct {
	feed    config.Feed
	client  *Client
	limiter *Limiter
	handle  Handler
	log     *slog.Logger

	interval time.Duration
	jitter   time.Duration

	now      func() time.Time
	randFrac func() float64 // injected so the jitter bounds are testable

	failures    int
	lastAuthLog time.Time
}

// PollerOptions carries the tunables from §8. Zero values are not defaults:
// config has already applied and validated those.
type PollerOptions struct {
	Interval time.Duration
	Jitter   time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

// NewPoller wires one feed to the shared client and limiter.
func NewPoller(f config.Feed, c *Client, l *Limiter, h Handler, opts PollerOptions) *Poller {
	return &Poller{
		feed:     f,
		client:   c,
		limiter:  l,
		handle:   h,
		log:      opts.Logger.With("component", "feed", "feed_id", f.ID),
		interval: opts.Interval,
		jitter:   opts.Jitter,
		now:      opts.Now,
		randFrac: rand.Float64,
	}
}

// Run polls until ctx is cancelled. It returns nil on cancellation: a clean
// shutdown is not a failure, and the caller distinguishes the two by whether
// it asked for one.
//
// The first poll happens immediately. A restart should produce data at once
// rather than after a poll interval of silence, and the change filter is empty
// after a restart anyway, so that first fetch is the one that repopulates it
// (§9.3 case 2).
func (p *Poller) Run(ctx context.Context) error {
	p.log.Info("poller started", "interval", p.interval.String(), "url", p.feed.RealtimeURL)
	defer p.log.Info("poller stopped")

	for {
		wait := p.pollOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// pollOnce performs one fetch and reports how long to wait before the next.
// Every failure path here is a delay, never an exit: one bad feed must not
// take down ingestion for the others, and a process that exits cannot serve
// reads either (§9.4 case 7, §10.1).
func (p *Poller) pollOnce(ctx context.Context) (wait time.Duration) {
	defer func() {
		// §10.1 allows recover in exactly three places and this is one of them.
		if r := recover(); r != nil {
			p.failures++
			p.log.Error("poller panic recovered", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			wait = p.nextDelay()
		}
	}()

	if err := p.limiter.Wait(ctx); err != nil {
		if errors.Is(err, ErrBudgetExhausted) {
			return p.budgetPause(err)
		}
		return p.nextDelay() // context cancelled; Run checks ctx before using this
	}

	resp, err := p.client.Fetch(ctx, p.feed.ID, p.feed.RealtimeURL, Conditional{})
	if err != nil {
		return p.classify(ctx, err)
	}

	p.failures = 0
	p.handle(ctx, resp)
	return p.nextDelay()
}

// classify turns a fetch error into a wait, and logs it at the level its
// recoverability deserves.
func (p *Poller) classify(ctx context.Context, err error) time.Duration {
	// Shutdown cancels the fetch in flight. Depending on which deadline fires
	// first that arrives as Canceled or as DeadlineExceeded, and neither is a
	// failure of the feed: asking our own context is the only reliable way to
	// tell "we stopped" from "the upstream hung". Run discards this wait.
	if ctx.Err() != nil {
		return p.interval
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// FEED_HTTP_TIMEOUT fired: the upstream accepted the connection and
		// then stalled. A real failure, and the backoff should reflect it.
		p.failures++
		p.log.Warn("feed request timed out", "consecutive_failures", p.failures,
			"timeout", p.client.http.Timeout.String())
		return p.nextDelay()

	case errors.Is(err, ErrNotModified):
		// A successful poll carrying no new data. Not a failure, and it must
		// not feed the staleness counter (§9.5 case 9).
		p.failures = 0
		return p.nextDelay()

	case errors.Is(err, ErrQuotaExhausted):
		// The quota is per account, so one feed's 403 stops every feed. The
		// upstream outranks our own counter here.
		p.limiter.Exhaust()
		return p.budgetPause(err)

	case errors.Is(err, ErrRateLimited):
		p.failures++
		p.log.Warn("feed rate limited", "consecutive_failures", p.failures,
			"hint", "lower FEED_RATE_LIMIT_RPS or raise FEED_POLL_INTERVAL")
		return p.nextDelay()

	case errors.Is(err, ErrUnauthorized):
		p.failures++
		// The key may be rotated underneath a running process, so keep trying,
		// but at most one line a minute: this can persist for hours.
		if now := p.now(); now.Sub(p.lastAuthLog) >= time.Minute {
			p.lastAuthLog = now
			p.log.Error("feed rejected the api key", "err", err.Error(),
				"hint", "check TFNSW_API_KEY; the service keeps retrying")
		}
		return p.nextDelay()

	case errors.Is(err, ErrEmptyBody):
		// 200 with nothing in it is a failed poll, and must not reset the
		// failure counter (§9.5 case 4).
		p.failures++
		p.log.Warn("feed returned an empty body", "consecutive_failures", p.failures)
		return p.nextDelay()

	default:
		p.failures++
		p.log.Warn("feed request failed", "err", err.Error(), "consecutive_failures", p.failures)
		return p.nextDelay()
	}
}

// budgetPause sleeps out the rest of the UTC day's quota window. It logs once
// per pause rather than once per attempt, because the pause is hours long.
func (p *Poller) budgetPause(err error) time.Duration {
	resets := p.limiter.ResetsIn()
	p.log.Error("daily request budget exhausted, pausing until reset",
		"err", err.Error(), "resets_in", resets.Round(time.Second).String())
	return resets
}

// nextDelay is the interval, with full jitter over the backoff that the
// consecutive-failure count earns: interval * 2^min(failures,4), capped at
// five minutes (§10.1).
//
// The jitter is added to the interval rather than centred on it, so the poller
// never fires faster than FEED_POLL_INTERVAL. Polling faster would spend quota
// that was budgeted at that interval, and FEED_DAILY_BUDGET is computed from
// it.
func (p *Poller) nextDelay() time.Duration {
	backoff := p.interval
	if p.failures > 0 {
		backoff = p.interval << min(p.failures, 4)
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}
	spread := backoff - p.interval + p.jitter
	if spread <= 0 {
		return p.interval
	}
	return p.interval + time.Duration(p.randFrac()*float64(spread))
}
