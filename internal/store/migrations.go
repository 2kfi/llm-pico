package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migration is a single schema version bump.
type migration struct {
	Version int
	// Name identifies the migration in logs.
	Name string
	// Stmts are applied in order within one transaction. Statements that fail
	// with "duplicate column name" are skipped, which keeps migrations safe to
	// re-run against databases that already picked up the column out of band
	// (the Python implementation did this, so existing files must still work).
	Stmts []string
}

// migrations is the ordered list of schema evolutions. Append new versions at
// the end; never edit an already-shipped entry.
var migrations = []migration{
	{
		Version: 1,
		Name:    "base schema",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS schema_version (
			     version     INTEGER PRIMARY KEY,
			     applied_at  TEXT NOT NULL
			 )`,
		},
	},
	{
		Version: 2,
		Name:    "model fallback chain",
		Stmts: []string{
			`ALTER TABLE models ADD COLUMN fallback_chain TEXT`,
		},
	},
	{
		Version: 3,
		Name:    "chain of LLMs",
		Stmts: []string{
			`ALTER TABLE teams ADD COLUMN model_chain TEXT`,
			`ALTER TABLE teams ADD COLUMN chain_rewrites_response TEXT`,
			`ALTER TABLE models ADD COLUMN chain_budget_usd REAL`,
		},
	},
	{
		// The Python implementation created these via the settings table and
		// several admin endpoints, but never recorded them in schema_version.
		// Recording them explicitly means a Go process opening a Python-created
		// database sees the same version and applies nothing further.
		Version: 4,
		Name:    "error taxonomy on usage rows",
		Stmts: []string{
			`ALTER TABLE usage_log ADD COLUMN error_type TEXT`,
			`ALTER TABLE teams ADD COLUMN monthly_budget_usd REAL`,
			`ALTER TABLE user_keys ADD COLUMN monthly_budget_usd REAL`,
			`ALTER TABLE user_keys ADD COLUMN hard_limit INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE user_keys ADD COLUMN ip_allowlist TEXT`,
		},
	},
}

// SchemaVersion returns the highest applied migration version, or 0 for a
// database that has never been migrated.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.Get(ctx, &v, `SELECT MAX(version) FROM schema_version`)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// Migrate applies every migration newer than the current schema version and
// records the new version. It is idempotent.
//
// Open calls this automatically; it is exported so tests and tooling can
// force a migration pass.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}

	applied := 0
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return applied, fmt.Errorf("store: migration v%d (%s): %w", m.Version, m.Name, err)
		}
		applied++
		current = m.Version
	}
	return applied, nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range m.Stmts {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				// A pre-existing column is not an error; it just means this
				// database already carries the change from another path.
				if isDuplicateColumn(err) {
					continue
				}
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO schema_version (version, applied_at) VALUES (?, ?)`,
			m.Version, NowISO())
		return err
	})
}

func isDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column name") ||
		strings.Contains(msg, "duplicate column")
}
