// Package gtfsstatic turns a static GTFS bundle into a complete, immutable
// schedule version in Postgres and marks it active (§4.2 component 1, §9.1).
// It knows nothing about realtime data, delays or the API.
package gtfsstatic

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/feed"
	"github.com/zigzaggoose/transitlateagain/internal/metrics"
)

// Loader downloads and loads schedule bundles. Load is not safe to call
// concurrently for the same feed; main calls it from one goroutine.
type Loader struct {
	pool         *pgxpool.Pool
	client       *feed.Client
	limiter      *feed.Limiter
	maxStopTimeS int
	keep         int
	now          func() time.Time
	log          *slog.Logger
}

// NewLoader returns a Loader. client should have the schedule's own timeout
// (§10.1: five minutes), not the realtime one: the bundle is 10.7 MB.
func NewLoader(pool *pgxpool.Pool, client *feed.Client, limiter *feed.Limiter, maxStopTimeS, keepVersions int, now func() time.Time, log *slog.Logger) *Loader {
	return &Loader{
		pool: pool, client: client, limiter: limiter,
		maxStopTimeS: maxStopTimeS, keep: keepVersions,
		now: now, log: log.With("component", "gtfsstatic"),
	}
}

// Load makes the feed's current bundle the active schedule version. changed
// is false when the downloaded bundle's content hash is the active one's.
//
// The download is unconditional. TfNSW answers an If-Modified-Since the
// bundle has not moved past with 502, not 304 (measured 2026-09-24), so a
// conditional request can never skip a download: after a 502 the bundle has
// to be fetched anyway to tell "unchanged" from a real outage (§15).
func (l *Loader) Load(ctx context.Context, f config.Feed) (versionID int64, changed bool, err error) {
	start := l.now()

	// The schedule download spends the same account quota as the pollers.
	if err := l.limiter.Wait(ctx); err != nil {
		return 0, false, fmt.Errorf("schedule download (feed=%s): %w", f.ID, err)
	}
	resp, err := l.client.Fetch(ctx, f.ID, f.ScheduleURL, feed.Conditional{})
	if err != nil {
		return 0, false, fmt.Errorf("schedule download (feed=%s): %w", f.ID, err)
	}
	sum := sha256.Sum256(resp.Body)

	// TfNSW republishes identical content under a new Last-Modified, so the
	// hash, not the validator, decides whether anything changed.
	var existing int64
	var wasActive bool
	err = l.pool.QueryRow(ctx,
		`SELECT id, active FROM schedule_versions WHERE feed_id = $1 AND sha256 = $2`, f.ID, sum[:],
	).Scan(&existing, &wasActive)
	switch {
	case err == nil:
		if !wasActive {
			// Content identical to an older version is the upstream rolling
			// back; what is published now is what should be active.
			if err := l.activate(ctx, f.ID, existing); err != nil {
				return 0, false, err
			}
		}
		return existing, !wasActive, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, fmt.Errorf("look up schedule hash (feed=%s): %w", f.ID, err)
	}

	var lm *time.Time
	if t, err := http.ParseTime(resp.LastModified); err == nil {
		lm = &t
	}
	versionID, counts, err := l.insert(ctx, f.ID, sum[:], lm, resp.Body)
	if err != nil {
		return 0, false, err
	}
	if err := l.activate(ctx, f.ID, versionID); err != nil {
		return 0, false, err
	}
	pruned, err := l.prune(ctx, f.ID)
	if err != nil {
		// The new version is active and correct; old rows lingering one more
		// day is a disk cost, not a correctness one.
		l.log.Warn("pruning old schedule versions failed", "feed_id", f.ID, "err", err.Error())
	}
	metrics.ScheduleLoadDuration.WithLabelValues(f.ID).Observe(l.now().Sub(start).Seconds())
	l.log.Info("schedule version activated",
		"feed_id", f.ID,
		"version_id", versionID,
		"bytes", len(resp.Body),
		"trip_count", counts["trips"],
		"stop_time_count", counts["stop_times"],
		"pruned", pruned,
		"load_ms", l.now().Sub(start).Milliseconds(),
	)
	return versionID, true, nil
}

// insert loads a bundle as a new, inactive version in one transaction. A
// bundle that fails anywhere leaves nothing behind, so there are no orphan
// versions to clean up (§9.4 case 5) and the active version is untouched.
func (l *Loader) insert(ctx context.Context, feedID string, sum []byte, lastModified *time.Time, body []byte) (int64, map[string]int64, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return 0, nil, fmt.Errorf("open schedule zip (feed=%s, %d bytes): %w", feedID, len(body), err)
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("begin schedule load (feed=%s): %w", feedID, err)
	}
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx)) // a no-op after Commit; after a failure the load error is the one to report
	}()

	var id int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO schedule_versions (feed_id, sha256, last_modified) VALUES ($1, $2, $3) RETURNING id`,
		feedID, sum, lastModified,
	).Scan(&id); err != nil {
		return 0, nil, fmt.Errorf("insert schedule version (feed=%s): %w", feedID, err)
	}

	counts := make(map[string]int64)
	for _, t := range tables(l.maxStopTimeS) {
		zf, ok := files[t.file]
		if !ok {
			if t.required {
				return 0, nil, fmt.Errorf("schedule bundle (feed=%s) has no %s", feedID, t.file)
			}
			continue
		}
		src, closer, err := open(zf, t, id)
		if err != nil {
			return 0, nil, fmt.Errorf("schedule bundle (feed=%s): %w", feedID, err)
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{t.name}, t.columns, src)
		_ = closer.Close() // reading a zip entry from memory; a close error cannot lose data
		if err != nil {
			return 0, nil, fmt.Errorf("load %s (feed=%s, version=%d): %w", t.file, feedID, id, err)
		}
		counts[t.name] = n
	}
	if _, ok := files["calendar.txt"]; !ok {
		if _, ok := files["calendar_dates.txt"]; !ok {
			return 0, nil, fmt.Errorf("schedule bundle (feed=%s) has neither calendar.txt nor calendar_dates.txt", feedID)
		}
	}
	// A bundle with no trips parses cleanly and would silently make every
	// realtime update unmatchable, so it is refused rather than activated.
	if counts["trips"] == 0 || counts["stop_times"] == 0 {
		return 0, nil, fmt.Errorf("schedule bundle (feed=%s) has %d trips and %d stop times; refusing to activate an empty timetable",
			feedID, counts["trips"], counts["stop_times"])
	}

	if _, err := tx.Exec(ctx,
		`UPDATE schedule_versions SET trip_count = $2, stop_time_count = $3 WHERE id = $1`,
		id, counts["trips"], counts["stop_times"],
	); err != nil {
		return 0, nil, fmt.Errorf("record schedule counts (feed=%s, version=%d): %w", feedID, id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("commit schedule load (feed=%s, version=%d): %w", feedID, id, err)
	}
	return id, counts, nil
}

// activate makes id the feed's one active version. Both updates are in one
// transaction and the partial unique index allows only one active row per
// feed, so there is never a moment with zero or two (§9.1 step 3).
func (l *Loader) activate(ctx context.Context, feedID string, id int64) error {
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE schedule_versions SET active = false WHERE feed_id = $1 AND active`, feedID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE schedule_versions SET active = true, activated_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("activate schedule version (feed=%s, version=%d): %w", feedID, id, err)
	}
	return nil
}

// prune deletes all but the newest keep versions, never the active one. The
// cascade removes their timetable rows.
func (l *Loader) prune(ctx context.Context, feedID string) (int64, error) {
	tag, err := l.pool.Exec(ctx, `
		DELETE FROM schedule_versions
		WHERE feed_id = $1
		  AND NOT active
		  AND id NOT IN (
		      SELECT id FROM schedule_versions WHERE feed_id = $1 ORDER BY id DESC LIMIT $2
		  )`, feedID, l.keep)
	if err != nil {
		return 0, fmt.Errorf("prune schedule versions (feed=%s): %w", feedID, err)
	}
	return tag.RowsAffected(), nil
}
