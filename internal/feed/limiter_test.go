package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLimiter_Reserve_SpacesRequestsAtTheConfiguredRate(t *testing.T) {
	start := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	l := NewLimiter(4, 1000, fixedClock(start))

	// The burst is one second's worth, so the first four go out immediately.
	for i := range 4 {
		if d, err := l.reserve(start); err != nil || d != 0 {
			t.Fatalf("request %d: delay=%s err=%v, want immediate", i, d, err)
		}
	}
	// The fifth is overdrawn and waits a quarter second at 4/s.
	d, err := l.reserve(start)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if want := 250 * time.Millisecond; d != want {
		t.Errorf("delay = %s, want %s", d, want)
	}
	// Two more overdraw further, and the waits accumulate rather than reset.
	d2, _ := l.reserve(start)
	if want := 500 * time.Millisecond; d2 != want {
		t.Errorf("delay = %s, want %s", d2, want)
	}
	// After real time passes, the bucket refills.
	if d3, _ := l.reserve(start.Add(time.Second)); d3 != 0 {
		t.Errorf("delay after a second = %s, want 0", d3)
	}
}

func TestLimiter_DailyBudget_IsEnforcedAndResetsAtUTCMidnight(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 59, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := NewLimiter(1000, 3, clock)

	for i := range 3 {
		if _, err := l.reserve(now); err != nil {
			t.Fatalf("request %d rejected: %v", i, err)
		}
	}
	_, err := l.reserve(now)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !strings.Contains(err.Error(), "3 requests used of 3") {
		t.Errorf("error should say what was spent: %v", err)
	}
	if got := l.Used(); got != 3 {
		t.Errorf("Used() = %d, want 3", got)
	}

	// One minute later it is a new UTC day.
	now = now.Add(time.Minute)
	if _, err := l.reserve(now); err != nil {
		t.Errorf("request after the daily rollover rejected: %v", err)
	}
	if got := l.Used(); got != 1 {
		t.Errorf("Used() after rollover = %d, want 1", got)
	}
}

// The day boundary is UTC, not local: the upstream quota is documented daily
// and UTC is the only boundary both sides agree on. A Sydney-local rollover
// would reset ten or eleven hours early.
func TestLimiter_Rollover_UsesUTCNotLocal(t *testing.T) {
	syd, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	// Local midnight in Sydney, which is 14:00 UTC the previous day.
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, syd)
	l := NewLimiter(1000, 1, func() time.Time { return now })

	if _, err := l.reserve(now); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := l.reserve(now.Add(time.Minute)); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("budget reset at local midnight: %v", err)
	}
}

func TestLimiter_Exhaust_StopsEveryFeedUntilReset(t *testing.T) {
	now := time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)
	l := NewLimiter(5, 10000, fixedClock(now))

	// The upstream is the authority: it says quota, so we stop, even though our
	// own counter has plenty left.
	l.Exhaust()

	if _, err := l.reserve(now); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if got, want := l.ResetsIn(), 18*time.Hour; got != want {
		t.Errorf("ResetsIn() = %s, want %s", got, want)
	}
}

func TestLimiter_Wait_HonoursContextCancellation(t *testing.T) {
	// One token a second, burst one: the second call must wait, and the
	// cancellation must win.
	l := NewLimiter(1, 100, time.Now)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("wait ignored the context for %s", elapsed)
	}
}

func TestLimiter_Wait_ReturnsWhenTokensAreAvailable(t *testing.T) {
	l := NewLimiter(1000, 100, time.Now)
	for i := range 5 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
}

func TestLimiter_ConcurrentReservations_AreCountedExactlyOnce(t *testing.T) {
	now := time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)
	l := NewLimiter(1e6, 500, fixedClock(now))

	done := make(chan struct{})
	for range 50 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 10 {
				_, _ = l.reserve(now)
			}
		}()
	}
	for range 50 {
		<-done
	}
	if got := l.Used(); got != 500 {
		t.Errorf("Used() = %d, want 500", got)
	}
}
