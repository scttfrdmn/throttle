package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A database created before a column existed must gain it, and stay writable.
//
// This is the case the ALTERs exist for, and every other test starts from a fresh schema,
// where the columns are present from the beginning and the ALTERs are already no-ops. Simulated
// by dropping them from a fresh database -- as close to an old file as a test can get without
// checking a binary database into the repository -- and then opening it again.
//
// In the internal package so the list comes from addedColumns itself: a column added there
// later is covered by this test without anybody remembering to update a second copy.
func TestDatabaseMissingAddedColumnsIsMigrated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "activity.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The indexes over those columns go first: SQLite refuses to drop a column an index
	// still references.
	for _, stmt := range addedIndexes {
		if _, err := raw.ExecContext(ctx, "DROP INDEX IF EXISTS "+indexOf(t, stmt)); err != nil {
			t.Fatalf("drop index: %v", err)
		}
	}
	dropped := 0
	for _, stmt := range addedColumns {
		col := columnOf(t, stmt)
		if _, err := raw.ExecContext(ctx, "ALTER TABLE activity DROP COLUMN "+col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
		dropped++
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("no columns dropped, so this test asserts nothing")
	}

	// Opening migrates it: the ALTERs run for real here rather than being swallowed as
	// duplicates, and a write proves the restored table still matches the INSERT.
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a database missing the added columns: %v", err)
	}
	defer second.Close()

	for _, stmt := range addedColumns {
		col := columnOf(t, stmt)
		if !hasColumn(t, second.DB(), col) {
			t.Errorf("column %q is still missing after Open", col)
		}
	}
	for _, stmt := range addedIndexes {
		if !hasIndex(t, second.DB(), indexOf(t, stmt)) {
			t.Errorf("index %q is still missing after Open", indexOf(t, stmt))
		}
	}
}

// columnOf reads the column name out of an ALTER TABLE ... ADD COLUMN statement, so the
// statements stay the single source of truth.
func columnOf(t *testing.T, stmt string) string {
	t.Helper()
	const marker = "ADD COLUMN "
	i := strings.Index(stmt, marker)
	if i < 0 {
		t.Fatalf("not an ADD COLUMN statement: %q", stmt)
	}
	name, _, _ := strings.Cut(strings.TrimSpace(stmt[i+len(marker):]), " ")
	return name
}

// indexOf reads the index name out of a CREATE INDEX IF NOT EXISTS statement.
func indexOf(t *testing.T, stmt string) string {
	t.Helper()
	const marker = "IF NOT EXISTS "
	i := strings.Index(stmt, marker)
	if i < 0 {
		t.Fatalf("not a CREATE INDEX IF NOT EXISTS statement: %q", stmt)
	}
	name, _, _ := strings.Cut(strings.TrimSpace(stmt[i+len(marker):]), " ")
	return name
}

func hasColumn(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pragma_table_info('activity') WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func hasIndex(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}
