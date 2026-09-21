package feed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrBudgetExhausted means the daily request budget is spent. The caller must
// stop polling until the budget resets rather than retrying, because every
// further request would return 403 and count against nothing but the log.
var ErrBudgetExhausted = errors.New("daily request budget exhausted")

// Limiter is the account-wide throttle: a token bucket for the per-second
// limit and a counter for the daily quota. Both limits are per account rather
// than per feed, so exactly one Limiter is shared by every poller and by the
// schedule downloader.
//
// It is hand-rolled rather than golang.org/x/time/rate because the daily
// budget has to be enforced in the same place as the rate — two independent
// limiters cannot answer "may this request go out" without racing — and
// because the token bucket underneath is thirty lines.
type Limiter struct {
	now func() time.Time

	mu     sync.Mutex
	rate   float64   // tokens per second
	burst  float64   // bucket capacity
	tokens float64   // may go negative: a reservation is spent when it is made
	last   time.Time // when tokens was last recomputed

	budget int       // requests allowed per UTC day
	used   int       // requests reserved so far today
	day    time.Time // UTC midnight that opened the current budget window
}

// NewLimiter returns a limiter admitting rps requests a second, at most
// dailyBudget of them per UTC day. The burst is one second's worth, which
// smooths the jitter between pollers without letting a restart fire every
// feed at once.
func NewLimiter(rps float64, dailyBudget int, now func() time.Time) *Limiter {
	t := now()
	return &Limiter{
		now:    now,
		rate:   rps,
		burst:  max(rps, 1),
		tokens: max(rps, 1),
		last:   t,
		budget: dailyBudget,
		day:    t.UTC().Truncate(24 * time.Hour),
	}
}

// Wait blocks until the request may be sent, ctx is done, or the daily budget
// is exhausted.
//
// A reservation is consumed when it is granted, so a cancelled context can
// spend a token that never becomes a request. That is deliberate: returning
// the token would need a second lock round-trip on a path that only runs
// during shutdown, and over-counting the quota is the safe direction to err.
func (l *Limiter) Wait(ctx context.Context) error {
	delay, err := l.reserve(l.now())
	if err != nil {
		return err
	}
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reserve consumes one request's allowance and reports how long the caller
// must wait before sending it. It is pure with respect to the clock, which is
// what makes the rate and the daily rollover testable without sleeping.
func (l *Limiter) reserve(now time.Time) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.rolloverLocked(now)
	if l.used >= l.budget {
		return 0, fmt.Errorf("%w: %d requests used of %d, resets in %s",
			ErrBudgetExhausted, l.used, l.budget, l.resetsInLocked(now).Round(time.Second))
	}
	l.used++

	// Refill by elapsed time, capped at the burst.
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens = min(l.tokens+elapsed.Seconds()*l.rate, l.burst)
		l.last = now
	}
	l.tokens--
	if l.tokens >= 0 {
		return 0, nil
	}
	// The bucket is overdrawn; wait for the deficit to refill.
	return time.Duration(-l.tokens / l.rate * float64(time.Second)), nil
}

// rolloverLocked opens a new budget window when the UTC day has turned. The
// quota is documented as a daily one and UTC is the only day boundary the
// upstream and we can agree on without a timezone argument.
func (l *Limiter) rolloverLocked(now time.Time) {
	if day := now.UTC().Truncate(24 * time.Hour); day.After(l.day) {
		l.day = day
		l.used = 0
	}
}

func (l *Limiter) resetsInLocked(now time.Time) time.Duration {
	return l.day.Add(24 * time.Hour).Sub(now.UTC())
}

// ResetsIn reports how long until the daily budget rolls over.
func (l *Limiter) ResetsIn() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resetsInLocked(l.now())
}

// Exhaust marks the budget spent for the rest of the UTC day. The upstream is
// the authority on the quota, not our counter: when it answers 403 Account
// Over Quota Limit, every feed must stop even though we think we have requests
// left. §9.5 case 7.
func (l *Limiter) Exhaust() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked(l.now())
	l.used = l.budget
}

// Used reports requests reserved in the current UTC day.
func (l *Limiter) Used() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked(l.now())
	return l.used
}
