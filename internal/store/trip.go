package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StopObservation is the last thing the feed said about one trip at one stop.
type StopObservation struct {
	DelayS      *int32
	TripRel     int32
	StopTimeRel int32
}

// TripStops returns, by stop_id, the newest observation of each stop of one
// trip on one service date: the final delay at a stop the trip has left, the
// latest prediction at one it has not reached. A stop the feed never
// mentioned is absent.
func (s *Store) TripStops(ctx context.Context, serviceDate time.Time, feedID, tripID string) (map[string]StopObservation, error) {
	// The three equalities are the leading primary-key columns, so this is
	// one index range scan in one partition. DISTINCT ON keeps the newest
	// row per stop; a loop service's second visit to a stop shares its key
	// and is already lost at write time (migrations/0005).
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (stop_id) stop_id, observed_delay_s, trip_rel, stop_time_rel
		FROM observations
		WHERE service_date = $1 AND feed_id = $2 AND trip_id = $3
		ORDER BY stop_id, feed_ts DESC`,
		serviceDate, feedID, tripID)
	if err != nil {
		return nil, fmt.Errorf("trip %q on %s: %w", tripID, serviceDate.Format(time.DateOnly), err)
	}
	out := make(map[string]StopObservation)
	var stopID string
	var o StopObservation
	_, err = pgx.ForEachRow(rows, []any{&stopID, &o.DelayS, &o.TripRel, &o.StopTimeRel}, func() error {
		out[stopID] = o
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("trip %q on %s: %w", tripID, serviceDate.Format(time.DateOnly), err)
	}
	return out, nil
}
