package store

import (
	"context"
	"fmt"
	"time"
)

// Maintenance is the database's side of /v1/admin/stats.
type Maintenance struct {
	OldestPartition *time.Time // nil before the first daily partition exists
	Partitions      int
	DefaultRows     int64 // should be 0: anything here escapes retention (§9.3 case 4)
	Watermark       time.Time
	DatabaseBytes   int64
}

// Maintenance reports partition, rollup and size state.
func (s *Store) Maintenance(ctx context.Context) (Maintenance, error) {
	var m Maintenance
	// Partition names carry their date (observations_YYYY_MM_DD), so the
	// oldest is the smallest name; the default partition does not match.
	err := s.pool.QueryRow(ctx, `
		SELECT to_date(min(substring(c.relname FROM 14)), 'YYYY_MM_DD'), count(*)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'observations'::regclass
		  AND c.relname ~ '^observations_[0-9]{4}_[0-9]{2}_[0-9]{2}$'`,
	).Scan(&m.OldestPartition, &m.Partitions)
	if err != nil {
		return Maintenance{}, fmt.Errorf("maintenance: partitions: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM observations_default),
		       (SELECT watermark FROM rollup_state WHERE name = 'hourly'),
		       pg_database_size(current_database())`,
	).Scan(&m.DefaultRows, &m.Watermark, &m.DatabaseBytes); err != nil {
		return Maintenance{}, fmt.Errorf("maintenance: state: %w", err)
	}
	return m, nil
}
