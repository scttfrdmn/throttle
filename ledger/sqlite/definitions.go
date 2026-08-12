package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
)

// maxChainDepth bounds ancestor walks. Parent links are validated acyclic on
// write, but a bound means a corrupted database produces an error rather than an
// infinite loop.
const maxChainDepth = 64

// PutDefinition implements ledger.Ledger.
//
// A definition is immutable through this path: re-storing an identical definition
// succeeds, and storing a semantically different one under the same ID fails with
// ErrDefinitionConflict. That is what stops two processes from governing shared
// spend under different rules; changing the rules requires UpdateDefinition and
// an explicit revision.
func (s *Store) PutDefinition(ctx context.Context, def budget.Definition) error {
	if err := def.Validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM budgets WHERE id = ?`, def.ID).Scan(&stored)
		switch {
		case err == nil:
			if stored == def.Fingerprint() {
				return nil // Idempotent: the same definition, already stored.
			}
			return fmt.Errorf("%w: %q is already defined with different semantics",
				ledger.ErrDefinitionConflict, def.ID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if err := checkParent(ctx, tx, def); err != nil {
			return err
		}
		return insertDefinition(ctx, tx, def, 1)
	})
}

// UpdateDefinition implements ledger.Ledger.
//
// The expected revision makes concurrent edits detectable: a process holding a
// stale view of the definition cannot overwrite a newer one. Periods already
// materialized keep their snapshot, so an update changes future periods only.
func (s *Store) UpdateDefinition(ctx context.Context, def budget.Definition, expectRevision int) error {
	if err := def.Validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var revision int
		err := tx.QueryRowContext(ctx, `SELECT revision FROM budgets WHERE id = ?`, def.ID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", ledger.ErrBudgetNotFound, def.ID)
		}
		if err != nil {
			return err
		}
		if revision != expectRevision {
			return fmt.Errorf("%w: %q is at revision %d, caller expected %d",
				ledger.ErrRevisionMismatch, def.ID, revision, expectRevision)
		}
		if err := checkParent(ctx, tx, def); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE budgets SET
				parent_id = ?, name = ?, allocation = ?, borrow_ns = ?,
				rollover_mode = ?, rollover_cap = ?, rollover_cap_bp = ?,
				recurrence = ?, recurrence_ns = ?, timezone = ?,
				anchor_at = ?, end_at = ?, fingerprint = ?,
				revision = revision + 1, updated_at = ?
			 WHERE id = ? AND revision = ?`,
			nullableParent(def.ParentID), def.Name, int64(def.Allocation), int64(def.Borrow),
			rolloverMode(def.Rollover.Mode), int64(def.Rollover.Cap), def.Rollover.CapBasisPoints,
			string(def.Recurrence), int64(def.Every), locationName(def.Location),
			def.AnchorAt.UTC().UnixNano(), toUnix(def.EndAt), def.Fingerprint(),
			toUnix(time.Now()), def.ID, expectRevision)
		return err
	})
}

func insertDefinition(ctx context.Context, tx *sql.Tx, def budget.Definition, revision int) error {
	now := toUnix(time.Now())
	_, err := tx.ExecContext(ctx,
		`INSERT INTO budgets
		 (id, parent_id, name, allocation, borrow_ns, rollover_mode, rollover_cap,
		  rollover_cap_bp, recurrence, recurrence_ns, timezone, anchor_at, end_at,
		  fingerprint, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		def.ID, nullableParent(def.ParentID), def.Name, int64(def.Allocation), int64(def.Borrow),
		rolloverMode(def.Rollover.Mode), int64(def.Rollover.Cap), def.Rollover.CapBasisPoints,
		string(def.Recurrence), int64(def.Every), locationName(def.Location),
		def.AnchorAt.UTC().UnixNano(), toUnix(def.EndAt), def.Fingerprint(),
		revision, now, now)
	return err
}

// checkParent verifies the parent exists and that adding this edge introduces no
// cycle. A cycle would make the ancestor walk in Reserve nonterminating, and
// worse, would make "the set of scopes a request consumes" undefined.
func checkParent(ctx context.Context, tx *sql.Tx, def budget.Definition) error {
	if def.ParentID == "" {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM budgets WHERE id = ?`, def.ParentID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: parent %q of %q", ledger.ErrBudgetNotFound, def.ParentID, def.ID)
	}

	// Walk up from the proposed parent; reaching this budget means a cycle.
	seen := map[string]bool{def.ID: true}
	cur := def.ParentID
	for depth := 0; cur != "" && depth < maxChainDepth; depth++ {
		if seen[cur] {
			return fmt.Errorf("%w: %q via %q", ledger.ErrCycle, def.ID, def.ParentID)
		}
		seen[cur] = true

		var parent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT parent_id FROM budgets WHERE id = ?`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		cur = parent.String
	}
	if cur != "" {
		return fmt.Errorf("%w: %q exceeds the maximum depth of %d", ledger.ErrCycle, def.ID, maxChainDepth)
	}
	return nil
}

// Definition implements ledger.Ledger.
func (s *Store) Definition(ctx context.Context, budgetID string) (budget.Definition, int, error) {
	def, revision, err := scanDefinition(s.db.QueryRowContext(ctx, definitionColumns+` WHERE id = ?`, budgetID))
	if errors.Is(err, sql.ErrNoRows) {
		return budget.Definition{}, 0, fmt.Errorf("%w: %q", ledger.ErrBudgetNotFound, budgetID)
	}
	return def, revision, err
}

const definitionColumns = `
	SELECT id, parent_id, name, allocation, borrow_ns, rollover_mode, rollover_cap,
	       rollover_cap_bp, recurrence, recurrence_ns, timezone, anchor_at, end_at, revision
	  FROM budgets`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDefinition(row rowScanner) (budget.Definition, int, error) {
	var (
		def                             budget.Definition
		parent                          sql.NullString
		allocation, borrow, cap_, every int64
		capBP                           int64
		anchor, endAt                   int64
		mode, recurrence, tz            string
		revision                        int
	)
	err := row.Scan(&def.ID, &parent, &def.Name, &allocation, &borrow, &mode, &cap_,
		&capBP, &recurrence, &every, &tz, &anchor, &endAt, &revision)
	if err != nil {
		return budget.Definition{}, 0, err
	}

	def.ParentID = parent.String
	def.Allocation = money.Money(allocation)
	def.Borrow = time.Duration(borrow)
	def.Rollover = budget.RolloverPolicy{
		Mode:           budget.RolloverMode(mode),
		Cap:            money.Money(cap_),
		CapBasisPoints: capBP,
	}
	def.Recurrence = budget.Recurrence(recurrence)
	def.Every = time.Duration(every)
	def.AnchorAt = time.Unix(0, anchor).UTC()
	def.EndAt = fromUnix(endAt)

	loc, err := time.LoadLocation(tz)
	if err != nil {
		// A database naming a timezone this binary cannot load must not be
		// silently reinterpreted as UTC: that would move every period boundary.
		return budget.Definition{}, 0, fmt.Errorf("sqlite: budget %q: load timezone %q: %w", def.ID, tz, err)
	}
	def.Location = loc
	return def, revision, nil
}

// Definitions implements ledger.Ledger.
func (s *Store) Definitions(ctx context.Context) ([]budget.Definition, error) {
	rows, err := s.db.QueryContext(ctx, definitionColumns+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []budget.Definition
	for rows.Next() {
		def, _, err := scanDefinition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// Chain implements ledger.Ledger, returning the budget and its ancestors nearest
// first. This is the authoritative scope set for a reservation: the ledger derives
// it from stored parent links rather than trusting a caller-supplied list, so a
// caller cannot accidentally omit an ancestor and escape its cap.
func (s *Store) Chain(ctx context.Context, budgetID string) ([]budget.Definition, error) {
	var out []budget.Definition
	err := s.read(ctx, func(q querier) error {
		var err error
		out, err = chain(ctx, q, budgetID)
		return err
	})
	return out, err
}

func chain(ctx context.Context, q querier, budgetID string) ([]budget.Definition, error) {
	var out []budget.Definition
	seen := map[string]bool{}

	for id := budgetID; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("%w: at %q", ledger.ErrCycle, id)
		}
		seen[id] = true
		if len(out) >= maxChainDepth {
			return nil, fmt.Errorf("%w: chain from %q exceeds depth %d", ledger.ErrCycle, budgetID, maxChainDepth)
		}

		def, _, err := scanDefinition(q.QueryRowContext(ctx, definitionColumns+` WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ledger.ErrBudgetNotFound, id)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, def)
		id = def.ParentID
	}
	return out, nil
}

func nullableParent(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// rolloverMode is the spelling stored in the database.
//
// Always canonical, so a mode that arrived as the zero value reads back as
// "none" rather than as an empty string that a later comparison would treat as a
// different policy.
func rolloverMode(m budget.RolloverMode) string {
	return string(m.Normalized())
}

func locationName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	return loc.String()
}
