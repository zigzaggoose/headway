package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/metrics"
)

type ctxKey struct{}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string) // absent only outside the middleware, where "" is right
	return id
}

// recorder captures what the handler sent, for the access log.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// middleware assigns a request id, recovers panics and writes the access log
// (§10.2). This is one of the three places recover is allowed.
func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.Now()
		var raw [6]byte
		_, _ = rand.Read(raw[:]) // since Go 1.24 it never returns an error; it crashes instead
		id := hex.EncodeToString(raw[:])
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
		w.Header().Set("X-Request-Id", id)
		rec := &recorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				metrics.Panics.WithLabelValues("api").Inc()
				s.Log.Error("handler panicked", "component", "api", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if rec.status == 0 {
					writeError(rec, r, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			if rec.status >= 500 && rec.status != http.StatusServiceUnavailable {
				// §10.2: a 5xx also gets an error line, so the request id a
				// client reports leads straight to it. Not a 503: that is
				// not_ready, the expected answer to every readiness probe
				// while the timetable loads, and logging it at ERROR buried
				// real errors in a startup's worth of noise.
				s.Log.Error("request failed", "component", "api", "request_id", id, "status", rec.status)
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK // the handler wrote nothing, so net/http sent 200
			}
			// ServeMux sets Pattern on this same request once it has routed
			// it. Empty means it never got that far: a 429 from the limiter.
			route := r.Pattern
			if route == "" {
				route = "unrouted"
			}
			metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(status)).Inc()
			metrics.HTTPRequestDuration.WithLabelValues(route).Observe(s.Now().Sub(start).Seconds())
			s.Log.Info("http request",
				"component", "api",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", s.Now().Sub(start).Milliseconds(),
				"bytes", rec.bytes,
				"remote", r.RemoteAddr,
				"client_ip", s.clientIP(r),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// sinceSeconds is a whole-second age, never negative: a feed timestamp a
// little ahead of our clock is skew (§9.5), not a negative age.
func sinceSeconds(now, t time.Time) int64 {
	return max(0, int64(now.Sub(t)/time.Second))
}
