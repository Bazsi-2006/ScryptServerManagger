// Package db owns the SQLite database: opening it with safe pragmas, applying
// embedded migrations and providing typed accessors for every table.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a uniqueness constraint would be violated.
var ErrConflict = errors.New("conflict")

type Store struct {
	DB *sql.DB
}

// Open opens (creating if necessary) the database at path, enables WAL mode
// and applies pending migrations.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		// Create the file ourselves so it never exists with loose permissions.
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		f.Close()
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&" + q.Encode()
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(8)
	}
	s := &Store{DB: sqlDB}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	// Fold the WAL back into the main file on clean shutdown.
	_, _ = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.DB.Close()
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: invalid version prefix", name)
		}
		data, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

// Migrate applies every embedded migration that has not been applied yet.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		return err
	}
	return s.applyMigrations(ctx, ms)
}

func (s *Store) applyMigrations(ctx context.Context, ms []migration) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade the application", current, latest)
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s failed: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at) VALUES (?,?,?)`,
			m.version, m.name, time.Now().Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

func fromNullUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

// tx runs fn in a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
