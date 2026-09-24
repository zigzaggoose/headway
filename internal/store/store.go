// Package store owns every SQL statement Transit Late Again runs. Queries are written by
// hand: the interesting parts of this design are partition pruning, ON CONFLICT
// behaviour and a COPY-based batch write, all of which an ORM hides. §15.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zigzaggoose/transitlateagain/internal/config"
)

// Store is a pgx pool with Transit Late Again's statements hung off it.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open builds the pool and proves the database is reachable before returning.
// At startup an unreachable database is fatal (§10.1); at runtime it is not,
// which is why this only runs once.
func Open(ctx context.Context, dsn config.Secret, maxConns int, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn.Reveal())
	if err != nil {
		// pgx puts the connection string in this error and the connection
		// string carries the password, so the detail is dropped deliberately.
		// config.Load has already checked that it parses as a postgres URL, so
		// what remains here is a pgx-specific parameter problem.
		return nil, errors.New("parse DATABASE_URL: not a connection string pgx accepts")
	}
	cfg.MaxConns = int32(maxConns)
	// The writer holds one connection for the life of a batch; the API must
	// never wait behind it for a connection that a dead TCP session still owns.
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database (host=%s db=%s): %w",
			cfg.ConnConfig.Host, cfg.ConnConfig.Database, err)
	}

	return &Store{pool: pool, log: log.With("component", "store")}, nil
}

// Pool exposes the pool for the components that own their own statements.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close waits for in-flight queries and shuts the pool down. It is step 6 of
// the shutdown sequence in §9.4 and must run after the writer has finished.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database answers, for /readyz.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Migrate applies the embedded migrations. fsys must be rooted where the
// NNNN_*.sql files live.
func (s *Store) Migrate(ctx context.Context, fsys fs.FS) ([]string, error) {
	return Migrate(ctx, s.pool, fsys, s.log)
}

// retryDelays is the schedule from §10.1: three retries after the first
// attempt. Jitter is applied to each so a pool full of goroutines retrying the
// same failed connection does not synchronise.
var retryDelays = []time.Duration{200 * time.Millisecond, 600 * time.Millisecond, 1800 * time.Millisecond}

// Retry runs f, retrying only errors that can plausibly succeed on a second
// attempt. A constraint violation is never retried: the second attempt would
// fail identically, and the delay would be spent for nothing.
func Retry(ctx context.Context, log *slog.Logger, op string, f func(context.Context) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = f(ctx); err == nil {
			return nil
		}
		if attempt >= len(retryDelays) || ctx.Err() != nil || !Retryable(err) {
			return err
		}
		delay := jitter(retryDelays[attempt])
		log.Warn("retrying database operation",
			"op", op, "attempt", attempt+1, "delay_ms", delay.Milliseconds(), "err", err.Error())

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
}

// Retryable reports whether err is worth a second attempt.
//
// The three SQLSTATEs are the ones §10.1 names: a serialization failure and a
// deadlock are by definition transient, and too-many-connections clears when
// something else finishes. Everything else the server reports — a constraint
// violation, a syntax error, an undefined table — will fail the same way next
// time.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	// Our own cancellation is not a database problem.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"53300": // too_many_connections
			return true
		default:
			return false
		}
	}

	// Not an error the server produced: the connection failed before or during
	// the send. pgx knows whether the statement could have taken effect.
	return pgconn.SafeToRetry(err)
}

// jitter spreads a delay over [d/2, d].
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}
