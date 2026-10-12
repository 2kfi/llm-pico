package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaParity is the Phase 1 gate: the Go schema must match the Python
// schema in python/core/db.py. Both are the single source of truth for the
// database layout, and a divergence silently corrupts existing deployments.
//
// The comparison is on normalized CREATE TABLE statements: whitespace is
// collapsed and comments stripped, so formatting differences are fine but
// column differences are not.
func TestSchemaParity(t *testing.T) {
	pythonPath := filepath.Join("..", "..", "python", "core", "db.py")
	raw, err := os.ReadFile(pythonPath)
	if err != nil {
		t.Fatalf("read python schema (this test only makes sense in the port branch): %v", err)
	}

	pythonStmts := extractSQLString(raw, "SCHEMA_SQL")
	if len(pythonStmts) == 0 {
		t.Fatal("could not extract SCHEMA_SQL from python/core/db.py")
	}
	goStmts := extractGoStatements(SchemaSQL())

	want := map[string]bool{}
	for _, s := range pythonStmts {
		want[normalizeStmt(s)] = true
	}
	got := map[string]bool{}
	for _, s := range goStmts {
		got[normalizeStmt(s)] = true
	}

	for stmt := range want {
		if !got[stmt] {
			t.Errorf("statement in python/core/db.py but missing from internal/store/schema.sql:\n\n%s", stmt)
		}
	}
	for stmt := range got {
		if !want[stmt] {
			t.Errorf("statement in internal/store/schema.sql but missing from python/core/db.py:\n\n%s", stmt)
		}
	}
}

// TestMigrationsCoverLegacyColumns guards the bug where the Python startup path
// never called run_migrations(), so columns added by ALTER TABLE in later
// versions never materialized. Every column that python/core/db.py adds via its
// ad-hoc ALTER TABLE block must be covered by a Go migration or the base schema.
func TestMigrationsCoverLegacyColumns(t *testing.T) {
	// Columns python/core/db.py adds at runtime outside SCHEMA_SQL.
	legacy := []string{
		"teams.ash_limit", "teams.asd_limit",
		"users.ash_limit", "users.asd_limit",
		"user_keys.ash_limit", "user_keys.asd_limit",
		"usage_log.error_type",
		"user_keys.monthly_budget_usd", "user_keys.hard_limit", "user_keys.ip_allowlist",
		"models.fallback_chain",
		"teams.model_chain", "teams.chain_rewrites_response",
		"models.chain_budget_usd",
	}

	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Open applies migrations itself, so the columns must already be present.
	for _, ref := range legacy {
		table, col, _ := strings.Cut(ref, ".")
		var n int
		err := s.Get(context.Background(), &n,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col)
		if err != nil {
			t.Errorf("%s: %v", ref, err)
			continue
		}
		if n == 0 {
			t.Errorf("column %s does not exist after migrations; Python created it via ALTER TABLE", ref)
		}
	}
}

// TestMigrateIsIdempotent verifies Migrate can be called repeatedly, matching
// the behaviour tests/test_migrations.py asserted for the Python version.
// Open already migrates, so an explicit call afterwards must be a no-op.
func TestMigrateIsIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	first, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 {
		t.Errorf("Migrate after Open applied %d, want 0; Open should have migrated already", first)
	}

	second, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second != 0 {
		t.Errorf("second Migrate applied %d, want 0", second)
	}

	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("schema version = %d, want %d", v, len(migrations))
	}
}

// TestOpenAppliesMigrations is the regression guard for the Python startup bug:
// run_migrations() was defined but never invoked, so live databases never gained
// model_chain and Chain-of-LLMs failed at runtime. Opening must be sufficient.
func TestOpenAppliesMigrations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("schema version after Open = %d, want %d", v, len(migrations))
	}
}

// TestOpenExistingDatabase covers the upgrade path: a database created by an
// older version (or by Python) must open cleanly and reach the current version.
func TestOpenExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Simulate a Python-created database: the base schema only, with none of
	// the columns that migrations add, and an empty schema_version table.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("schema version = %d, want %d", v, len(migrations))
	}

	var n int
	if err := s.Get(context.Background(), &n, `SELECT COUNT(*) FROM pragma_table_info('teams') WHERE name='model_chain'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("model_chain missing after opening a legacy database")
	}
}

// --- helpers ---

// normalizeStmt strips comments and collapses whitespace so that formatting
// differences between the Python and Go schemas do not register as drift.
// String literals are preserved verbatim.
func normalizeStmt(s string) string {
	// Strip line comments first, per line, so the main pass stays simple.
	var stripped []string
	for _, line := range strings.Split(s, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			// Only treat "--" as a comment when it is not inside a quote on
			// this line. Schema DDL never has both, so a plain Index is fine.
			line = line[:idx]
		}
		stripped = append(stripped, line)
	}

	var b strings.Builder
	inString := false
	for _, r := range strings.Join(stripped, "\n") {
		switch {
		case r == '\'':
			inString = !inString
			b.WriteRune(r)
		case !inString && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	for _, r := range script {
		if r == '\'' {
			inString = !inString
		}
		if r == ';' && !inString {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// extractSQLString pulls the contents of a Python triple-quoted string constant
// and splits it into statements.
func extractSQLString(src []byte, constName string) []string {
	s := string(src)
	idx := strings.Index(s, constName)
	if idx < 0 {
		return nil
	}
	start := strings.Index(s[idx:], `"""`)
	if start < 0 {
		return nil
	}
	start += idx + 3
	end := strings.Index(s[start:], `"""`)
	if end < 0 {
		return nil
	}
	return splitStatements(s[start : start+end])
}

// extractGoStatements returns the statements in the embedded schema script.
func extractGoStatements(script string) []string {
	return splitStatements(script)
}
