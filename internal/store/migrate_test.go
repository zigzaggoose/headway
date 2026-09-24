package store

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zigzaggoose/transitlateagain"
	"github.com/zigzaggoose/transitlateagain/internal/config"
)

// The migrations that ship in the binary must satisfy the loader's own rules:
// unique ascending versions, NNNN_name.sql, non-empty.
func TestLoad_EmbeddedMigrations_AreWellFormed(t *testing.T) {
	sub, err := fs.Sub(transitlateagain.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	got, err := load(sub)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(got) < 4 {
		t.Fatalf("loaded %d migrations, want the four in migrations/", len(got))
	}
	for i, m := range got {
		if m.version != i+1 {
			t.Errorf("migration %d has version %d; versions must be dense and ascending", i, m.version)
		}
		if len(m.checksum) != 32 {
			t.Errorf("migration %04d has a %d-byte checksum", m.version, len(m.checksum))
		}
	}
}

func TestLoad_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{
			name:  "a file with no version prefix",
			files: fstest.MapFS{"schedule.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
			want:  "not named NNNN_description.sql",
		},
		{
			name:  "a non-numeric version",
			files: fstest.MapFS{"xxxx_schedule.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
			want:  "non-numeric version prefix",
		},
		{
			name: "two files claiming one version",
			files: fstest.MapFS{
				"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
				"001_b.sql":  &fstest.MapFile{Data: []byte("SELECT 2")},
			},
			want: "share version 1",
		},
		{
			name:  "no migrations at all",
			files: fstest.MapFS{"readme.txt": &fstest.MapFile{Data: []byte("nope")}},
			want:  "no migrations found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(tc.files)
			if err == nil {
				t.Fatal("load accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoad_SortsByVersionNotByName(t *testing.T) {
	files := fstest.MapFS{
		"0010_ten.sql": &fstest.MapFile{Data: []byte("SELECT 10")},
		"0002_two.sql": &fstest.MapFile{Data: []byte("SELECT 2")},
		"0001_one.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
	}
	got, err := load(files)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []int{1, 2, 10}
	for i, m := range got {
		if m.version != want[i] {
			t.Fatalf("order = %d at %d, want %d", m.version, i, want[i])
		}
	}
}

// §8.2: the connection string carries a password, so a parse failure must not
// put it in an error that something later logs.
func TestOpen_MalformedDSN_DoesNotLeakThePassword(t *testing.T) {
	const secret = "hunter2"
	dsn := config.Secret("postgres://headway:" + secret + "@localhost:5432/headway?sslmode=bogus-value-that-pgx-rejects")

	_, err := Open(context.Background(), dsn, 2, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Skip("pgx accepted this DSN; the leak path is what matters, not this value")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"unique violation is not transient", &pgconn.PgError{Code: "23505"}, false},
		{"foreign key violation is not transient", &pgconn.PgError{Code: "23503"}, false},
		{"undefined table is a bug, not a blip", &pgconn.PgError{Code: "42P01"}, false},
		{"our own cancellation", context.Canceled, false},
		{"our own deadline", context.DeadlineExceeded, false},
		{"a wrapped retryable error", errors.Join(errors.New("write observations"), &pgconn.PgError{Code: "40001"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retryable(tc.err); got != tc.want {
				t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetry(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)

	t.Run("a transient failure is retried until it succeeds", func(t *testing.T) {
		calls := 0
		err := Retry(context.Background(), quiet, "test", func(context.Context) error {
			calls++
			if calls < 3 {
				return &pgconn.PgError{Code: "40001"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Retry: %v", err)
		}
		if calls != 3 {
			t.Errorf("called %d times, want 3", calls)
		}
	})

	t.Run("a constraint violation is not retried", func(t *testing.T) {
		calls := 0
		err := Retry(context.Background(), quiet, "test", func(context.Context) error {
			calls++
			return &pgconn.PgError{Code: "23505"}
		})
		if err == nil {
			t.Fatal("Retry succeeded")
		}
		if calls != 1 {
			t.Errorf("called %d times, want 1: a unique violation fails identically every time", calls)
		}
	})

	t.Run("it gives up after the documented number of attempts", func(t *testing.T) {
		calls := 0
		start := time.Now()
		err := Retry(context.Background(), quiet, "test", func(context.Context) error {
			calls++
			return &pgconn.PgError{Code: "40P01"}
		})
		if err == nil {
			t.Fatal("Retry succeeded")
		}
		if want := len(retryDelays) + 1; calls != want {
			t.Errorf("called %d times, want %d (one attempt plus %d retries)", calls, want, len(retryDelays))
		}
		// 100+300+900ms at the low end of the jitter.
		if elapsed := time.Since(start); elapsed < 1300*time.Millisecond {
			t.Errorf("gave up after %s; the backoff did not happen", elapsed)
		}
	})

	t.Run("cancellation stops the retries", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := Retry(ctx, quiet, "test", func(context.Context) error {
			return &pgconn.PgError{Code: "40001"}
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want it to carry the deadline", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("ignored the context for %s", elapsed)
		}
	})
}
