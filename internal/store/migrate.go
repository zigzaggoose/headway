package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary constant that two Headway processes agree
// on. It is not distributed locking in the sense §2 rejects — nothing
// coordinates ingestion — it just stops `make migrate` and a starting
// container from running the same DDL at the same instant.
const migrationLockID int64 = 0x48454144 // "HEAD"

// migration is one numbered file.
type migration struct {
	version  int
	name     string
	body     string
	checksum []byte
}

// Migrate applies every migration in fsys that this database has not seen, in
// version order, each in its own transaction. It is safe to call on every
// startup and returns the names it applied.
//
// A migration that has already been applied but whose file has since changed
// is an error, not a re-run: §14 says never edit an applied migration, and the
// checksum is what turns that rule from a convention into a failed startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) ([]string, error) {
	migrations, err := load(fsys)
	if err != nil {
		return nil, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for migrations: %w", err)
	}
	defer conn.Release()

	// The lock is released when the connection returns to the pool, but say so
	// explicitly: a leaked migration lock blocks every future startup.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Error("release migration lock", "err", err.Error())
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    integer     PRIMARY KEY,
		    name       text        NOT NULL,
		    checksum   bytea       NOT NULL,
		    applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, m := range migrations {
		if prev, ok := applied[m.version]; ok {
			if string(prev.checksum) != string(m.checksum) {
				return nil, fmt.Errorf(
					"migration %04d_%s has changed since it was applied (%s): edit is not allowed, add a new numbered migration instead",
					m.version, m.name, prev.appliedAt.Format(time.RFC3339))
			}
			continue
		}
		// A migration that is older than one already applied would run out of
		// order and see a schema its author never anticipated.
		for v := range applied {
			if v > m.version {
				return nil, fmt.Errorf("migration %04d_%s is unapplied but %04d has already run: renumber it above the highest applied version",
					m.version, m.name, v)
			}
		}

		started := time.Now()
		if err := apply(ctx, conn, m); err != nil {
			return ran, err
		}
		log.Info("migration applied",
			"version", m.version,
			"name", m.name,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		ran = append(ran, fmt.Sprintf("%04d_%s", m.version, m.name))
	}
	return ran, nil
}

// apply runs one migration and records it in the same transaction, so a
// half-applied migration cannot be marked as done.
func apply(ctx context.Context, conn *pgxpool.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %04d_%s: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.body); err != nil {
		return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum); err != nil {
		return fmt.Errorf("record migration %04d_%s: %w", m.version, m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %04d_%s: %w", m.version, m.name, err)
	}
	return nil
}

type appliedMigration struct {
	checksum  []byte
	appliedAt time.Time
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int]appliedMigration, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int]appliedMigration)
	for rows.Next() {
		var v int
		var a appliedMigration
		if err := rows.Scan(&v, &a.checksum, &a.appliedAt); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = a
	}
	return out, rows.Err()
}

// load reads and sorts the migration files. Names are NNNN_description.sql;
// the number is the version and the order, which is why it is zero-padded.
func load(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("no migrations found: the binary was built without migrations/*.sql")
	}

	var out []migration
	seen := make(map[int]string, len(names))
	for _, name := range names {
		base := path.Base(name)
		prefix, rest, ok := strings.Cut(strings.TrimSuffix(base, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q is not named NNNN_description.sql", base)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version prefix: %w", base, err)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, base, version)
		}
		seen[version] = base

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", base, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: version, name: rest, body: string(body), checksum: sum[:]})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

// ensure pgx is the only driver in play; a stray database/sql import would
// silently bypass the pool.
var _ = pgx.ErrNoRows
