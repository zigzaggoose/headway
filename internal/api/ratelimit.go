package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ipLimiter is a token bucket per client IP (§7.2's 429, HTTP_RATE_LIMIT_RPS).
// Each bucket holds one second of requests, so a client may burst to the
// rate and sustain it, and no more.
//
// It is keyed on the connection's address, not X-Forwarded-For: nothing sits
// in front of the service, so that header is whatever the client chose to
// send. Behind a proxy this would limit the proxy, and the key would need to
// change.
type ipLimiter struct {
	rate float64 // tokens per second, and the bucket size
	now  func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// idleAfter is how long a bucket sits unused before it is forgotten. It
// refills completely in one second, so an idle bucket carries no state.
const idleAfter = time.Minute

func newIPLimiter(rate float64, now func() time.Time) *ipLimiter {
	return &ipLimiter{rate: rate, now: now, buckets: make(map[string]*bucket), lastSweep: now()}
}

// allow takes a token for ip, or reports how long until one is available.
func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweeping here rather than on a timer needs no goroutine: the map is
	// only ever as large as the clients seen in the last minute or so.
	if now.Sub(l.lastSweep) > idleAfter {
		for k, b := range l.buckets {
			if now.Sub(b.last) > idleAfter {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: l.rate, last: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(l.rate, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// size is the number of clients tracked, for tests.
func (l *ipLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// limit wraps next with the per-IP limit. Health probes are exempt: an
// orchestrator polling /readyz must never be told to back off.
func (s *server) limit(next http.Handler) http.Handler {
	if s.RateLimit <= 0 {
		return next
	}
	l := newIPLimiter(s.RateLimit, s.Now)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr // not host:port, as in some tests; still a stable key
		}
		ok, wait := l.allow(ip)
		if !ok {
			secs := int(math.Ceil(wait.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(1, secs)))
			writeError(w, r, http.StatusTooManyRequests, "too_many_requests", "rate limit exceeded; see Retry-After")
			return
		}
		next.ServeHTTP(w, r)
	})
}
