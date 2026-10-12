// Package store owns the SQLite database: connection pooling, schema creation,
// versioned migrations, and the low-level query helpers used across the proxy.
//
// The database is the single source of truth for all configuration. There are
// no YAML or JSON config files.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// SchemaSQL returns the DDL applied to every fresh database.
func SchemaSQL() string { return schemaSQL }

// poolSize matches the Python implementation, which ran aiosqlite with two
// connections. SQLite serializes writers anyway, so raising this buys little
// without a real pooler.
const poolSize = 2

var ErrNotInitialized = errors.New("store: database not initialized")

// Store is a handle to the SQLite database.
//
// It maintains a small fixed-size pool of connections. Callers never touch the
// pool directly; use the query helpers (Get, Query, Exec) or the *Context
// variants, which check out a connection, run the statement, and return it.
type Store struct {
	path string

	mu    sync.Mutex
	conns []*sql.DB // each *sql.DB is itself a pool; we keep a few for read fan-out
	set   bool
}

// Open prepares the database at path, applying the schema and any pending
// migrations. It is safe to call once at startup.
func Open(path string) (*Store, error) {
	s := &Store{path: path}

	// A single writer connection is enough for writes; SQLite serializes them
	// regardless, and busy_timeout handles contention from the readers.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(2000)&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", url.PathEscape(path))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Keep exactly one connection so writes never contend with our own readers
	// and WAL checkpointing stays predictable.
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	s.conns = []*sql.DB{db}
	s.set = true
	return s, nil
}

// Close releases all database handles.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return nil
	}
	var firstErr error
	for _, db := range s.conns {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.conns = nil
	s.set = false
	return firstErr
}

func (s *Store) primary() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set || len(s.conns) == 0 {
		return nil, ErrNotInitialized
	}
	return s.conns[0], nil
}

// Exec runs a statement that returns no rows.
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	db, err := s.primary()
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, query, args...)
}

// Query runs a statement returning multiple rows. The caller must close rows.
func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	db, err := s.primary()
	if err != nil {
		return nil, err
	}
	return db.QueryContext(ctx, query, args...)
}

// QueryRow runs a statement returning at most one row.
func (s *Store) QueryRow(ctx context.Context, query string, args ...any) (*sql.Row, error) {
	db, err := s.primary()
	if err != nil {
		return nil, err
	}
	return db.QueryRowContext(ctx, query, args...), nil
}

// Get is a convenience wrapper that scans a single row into dest, reporting
// sql.ErrNoRows when nothing matched.
func (s *Store) Get(ctx context.Context, dest any, query string, args ...any) error {
	row, err := s.QueryRow(ctx, query, args...)
	if err != nil {
		return err
	}
	return row.Scan(dest)
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic.
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := s.primary()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Now returns the timestamp format used throughout the schema:
// "2006-01-02T15:04:05" in UTC. Every created_at / applied_at column in the
// database uses this format, so comparisons are plain string comparisons.
func Now() string { return time.Now().UTC().Format("2006-01-02T15:04:05") }

// NowISO returns a full RFC3339 timestamp for audit records that embed one.
func NowISO() string { return time.Now().UTC().Format(time.RFC3339) }
