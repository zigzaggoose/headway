package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// healthz is liveness only. It checks nothing, because a Postgres outage
// that restarted the process would turn into a restart loop (§7.1).
func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz checks the database and feed freshness. §7.1 also requires an
// active schedule, which does not exist until Stage 2; that check joins then.
func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	var reasons []string

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		// The reason is for an operator; the error itself goes to the log,
		// because pgx errors can carry connection details.
		s.Log.Warn("readiness: database ping failed", "component", "api", "request_id", requestID(r.Context()), "err", err.Error())
		reasons = append(reasons, "database did not answer within 2s")
	}

	// A snapshot lands in the cache only after a fetch and a decode have both
	// succeeded, so its age is how long ago a feed last succeeded.
	if newest := s.Cache.Newest(); newest.IsZero() {
		reasons = append(reasons, "no feed has succeeded yet")
	} else if age := s.Now().Sub(newest); age > s.ReadyMaxFeedAge {
		reasons = append(reasons, fmt.Sprintf("no feed has succeeded within %s (newest is %s old)", s.ReadyMaxFeedAge, age.Round(time.Second)))
	}

	if len(reasons) > 0 {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "not ready", reasons...)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
