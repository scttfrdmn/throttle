// Package sqlite is a durable ledger.Ledger backed by SQLite.
//
// It uses modernc.org/sqlite, a pure-Go driver, so throttle needs no CGO
// toolchain and works under the race detector and cross-compilation.
//
// # Concurrency model
//
// Every mutating operation runs in a transaction opened with
// _txlock=immediate, which takes the write lock before any state is read. That
// makes the headroom check and the write a single atomic step across processes,
// not just goroutines, which is what the reservation contract requires. A
// deferred transaction would let two callers read the same headroom and collide
// at commit time -- exactly the race reservations exist to prevent.
//
// WAL mode lets readers proceed while a writer holds the lock, and busy_timeout
// absorbs contention rather than failing fast.
//
// # Hierarchical reservation
//
// A reservation against a leaf budget consumes headroom from that budget and
// every ancestor. All legs are checked and inserted in one transaction, so the
// whole chain reserves or none of it does. The ledger derives the chain from
// stored parent links rather than trusting the caller, so an ancestor cannot be
// omitted and thereby escape its cap.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"throttle/ledger"
	"throttle/money"
	"throttle/usage"

	_ "modernc.org/sqlite"
)

// Store is a durable, concurrency-safe ledger.
type Store struct {
	db *sql.DB
}

var _ ledger.Ledger = (*Store)(nil)

// querier is the read surface shared by *sql.DB and *sql.Tx, so helpers can run
// either inside a transaction or standalone.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Open opens or creates a ledger database at path. Use ":memory:" for a
// transient store; note that each ":memory:" connection is a separate database,
// so the pool is limited to one connection in that case.
func Open(ctx context.Context, path string) (*Store, error) {
	memory := path == ":memory:" || strings.Contains(path, "mode=memory")

	dsn := path
	if !strings.Contains(dsn, "?") {
		// _pragma keys are how modernc.org/sqlite accepts pragmas in a DSN, and
		// _txlock=immediate makes BeginTx take the write lock up front so a
		// headroom check and its insert cannot interleave.
		dsn += "?_txlock=immediate" +
			"&_pragma=busy_timeout(10000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=foreign_keys(ON)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}

	if memory {
		// A shared in-memory database exists only as long as its connection.
		db.SetMaxOpenConns(1)
	} else {
		// SQLite serializes writes anyway; a small pool avoids pointless lock
		// contention while still allowing concurrent WAL readers.
		db.SetMaxOpenConns(8)
	}
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := openRetry(ctx, func() error { return db.PingContext(ctx) }); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}
	if err := openRetry(ctx, func() error {
		_, err := db.ExecContext(ctx, schema)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// openRetry retries fn while SQLite reports the database as locked.
//
// busy_timeout covers ordinary contention, but not the two things that happen exactly once
// per database: switching a fresh file into WAL mode and creating the schema. A journal-mode
// change needs an exclusive lock and SQLite does not invoke the busy handler for it, so
// several processes opening a new ledger at the same moment -- which is what a few machines
// applying the same configuration on first run looks like -- would otherwise see one succeed
// and the rest fail with SQLITE_BUSY.
//
// Retrying is safe because both operations are idempotent: the pragma is a no-op once WAL is
// set, and the schema is CREATE TABLE IF NOT EXISTS throughout. Nothing about money is
// retried here; this is opening a file.
func openRetry(ctx context.Context, fn func() error) error {
	const (
		attempts = 40
		pause    = 50 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil || !isLocked(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
	return err
}

// isLocked reports whether err is SQLite's busy/locked condition.
//
// Matched on the message rather than on a driver error type: modernc.org/sqlite returns an
// unexported error type, so there is nothing to assert against. The driver names the result
// code in the message ("database is locked (5) (SQLITE_BUSY)"), which is what this reads.
// Deliberately narrow -- a bare "(5)" would match unrelated messages.
func isLocked(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "SQLITE_LOCKED") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked")
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the local API layer to run read-only reporting
// queries. Callers must not write through it.
func (s *Store) DB() *sql.DB { return s.db }

// tx runs fn inside a write transaction and commits only if fn succeeds.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// read runs fn against the pool without a transaction, for single-statement or
// read-only work where a write lock would be waste.
func (s *Store) read(ctx context.Context, fn func(querier) error) error {
	return fn(s.db)
}

// timestamps are stored as UTC Unix nanoseconds, with 0 meaning "unset".
func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Totals implements ledger.Ledger for one scope.
//
// Expired holds are separated from live ones so that a crashed process stops
// blocking new spend the moment its lease lapses, without waiting for recovery
// to run.
func (s *Store) Totals(ctx context.Context, scope ledger.Scope, now time.Time) (ledger.Totals, error) {
	var t ledger.Totals
	err := s.read(ctx, func(q querier) error {
		var err error
		t, err = totals(ctx, q, scope, now)
		return err
	})
	return t, err
}

func totals(ctx context.Context, q querier, scope ledger.Scope, now time.Time) (ledger.Totals, error) {
	var t ledger.Totals

	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM charge_legs WHERE budget_id = ? AND period_id = ?`,
		scope.BudgetID, scope.PeriodID).Scan(&t.Spent); err != nil {
		return ledger.Totals{}, fmt.Errorf("sqlite: sum charges: %w", err)
	}

	nowNanos := now.UTC().UnixNano()
	err := q.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN r.expires_at = 0 OR r.expires_at >= ? THEN l.amount ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.expires_at = 0 OR r.expires_at >= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.expires_at > 0 AND r.expires_at < ? THEN l.amount ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.expires_at > 0 AND r.expires_at < ? THEN 1 ELSE 0 END), 0)
		   FROM reservation_legs l
		   JOIN reservations r ON r.id = l.reservation_id
		  WHERE l.budget_id = ? AND l.period_id = ? AND r.state = 'pending'`,
		nowNanos, nowNanos, nowNanos, nowNanos, scope.BudgetID, scope.PeriodID,
	).Scan(&t.Reserved, &t.PendingCount, &t.ReservedExpired, &t.ExpiredCount)
	if err != nil {
		return ledger.Totals{}, fmt.Errorf("sqlite: sum reservations: %w", err)
	}
	return t, nil
}

// Reserve implements ledger.Ledger.
//
// The scope set, the headroom check for every scope, and the inserts all share one
// IMMEDIATE transaction. Either the entire ancestor chain reserves or nothing is
// written.
func (s *Store) Reserve(ctx context.Context, req ledger.ReserveRequest) (ledger.Reservation, error) {
	if req.ID == "" {
		return ledger.Reservation{}, fmt.Errorf("%w: reservation id is required", ledger.ErrInvalidArgument)
	}
	if req.BudgetID == "" {
		return ledger.Reservation{}, fmt.Errorf("%w: budget id is required", ledger.ErrInvalidArgument)
	}
	if req.Amount < 0 {
		return ledger.Reservation{}, fmt.Errorf("%w: reservation amount cannot be negative", ledger.ErrInvalidArgument)
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	r := req.Reservation
	r.State = ledger.StatePending
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.Legs = nil

	err := s.tx(ctx, func(tx *sql.Tx) error {
		r.Legs = nil

		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM reservations WHERE id = ?`, r.ID).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return fmt.Errorf("%w: %q", ledger.ErrDuplicateReservation, r.ID)
		}

		// Roll periods forward before deciding anything, so a request arriving
		// after a boundary is admitted against the correct period.
		if _, err := advance(ctx, tx, r.BudgetID, now); err != nil {
			return err
		}

		defs, err := chain(ctx, tx, r.BudgetID)
		if err != nil {
			return err
		}

		legs := make([]ledger.Leg, 0, len(defs))
		for depth, def := range defs {
			if _, err := advance(ctx, tx, def.ID, now); err != nil {
				return err
			}
			period, err := ensurePeriod(ctx, tx, def.ID, now)
			if err != nil {
				return err
			}
			if period.State != ledger.StateOpen {
				return &ledger.ScopeError{
					BudgetID: def.ID, PeriodID: period.ID,
					Err: fmt.Errorf("%w: period is %s", ledger.ErrPeriodClosed, period.State),
				}
			}
			legs = append(legs, ledger.Leg{
				Scope:  ledger.Scope{BudgetID: def.ID, PeriodID: period.ID},
				Depth:  depth,
				Amount: r.Amount,
			})
		}

		// Check every scope before writing anything. A ceiling must be supplied
		// for each: defaulting a missing one to unlimited would let a caller skip
		// an ancestor's cap by forgetting it.
		for _, leg := range legs {
			ceiling, ok := req.Ceilings[leg.Scope.BudgetID]
			if !ok {
				return &ledger.ScopeError{
					BudgetID: leg.Scope.BudgetID, PeriodID: leg.Scope.PeriodID,
					Err: ledger.ErrMissingCeiling,
				}
			}
			t, err := totals(ctx, tx, leg.Scope, now)
			if err != nil {
				return err
			}
			committed, sumOK := money.Sum(t.Spent, t.Reserved, leg.Amount)
			if !sumOK || committed > ceiling {
				return &ledger.ScopeError{
					BudgetID: leg.Scope.BudgetID, PeriodID: leg.Scope.PeriodID,
					Err: ledger.ErrInsufficientHeadroom,
				}
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reservations
			 (id, budget_id, request_id, amount, estimated_cost, created_at, expires_at,
			  lease_ns, renewed_at, renew_count, state, identity, metadata)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)`,
			r.ID, r.BudgetID, r.RequestID, int64(r.Amount), int64(r.EstimatedCost),
			toUnix(r.CreatedAt), toUnix(r.ExpiresAt), int64(r.Lease),
			string(r.State), marshalJSON(r.Identity), marshalJSON(r.Metadata),
		); err != nil {
			return err
		}

		for _, leg := range legs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO reservation_legs (reservation_id, budget_id, period_id, depth, amount)
				 VALUES (?, ?, ?, ?, ?)`,
				r.ID, leg.Scope.BudgetID, leg.Scope.PeriodID, leg.Depth, int64(leg.Amount)); err != nil {
				return err
			}
		}
		r.Legs = legs
		return nil
	})
	if err != nil {
		return ledger.Reservation{}, err
	}
	return r, nil
}

// Renew implements ledger.Ledger.
//
// Renewal moves the lease deadline without re-checking headroom: the amount held
// is unchanged, so there is nothing new to authorize. A renewal that arrives after
// the lease has already lapsed fails, because that headroom was already freed and
// may have been granted to someone else -- silently reclaiming it would let the
// budget be oversubscribed. The request may still settle and will be recorded as
// spend regardless.
func (s *Store) Renew(ctx context.Context, req ledger.RenewRequest) (ledger.Reservation, error) {
	if req.ReservationID == "" {
		return ledger.Reservation{}, fmt.Errorf("%w: reservation id is required", ledger.ErrInvalidArgument)
	}
	if req.Extend < 0 {
		return ledger.Reservation{}, fmt.Errorf("%w: renewal extension cannot be negative", ledger.ErrInvalidArgument)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var out ledger.Reservation
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadReservation(ctx, tx, req.ReservationID)
		if err != nil {
			return err
		}
		switch r.State {
		case ledger.StateSettled, ledger.StateReleased:
			return fmt.Errorf("%w: %q is %s", ledger.ErrAlreadyResolved, r.ID, r.State)
		case ledger.StateExpired:
			return fmt.Errorf("%w: %q was already reclaimed", ledger.ErrLeaseExpired, r.ID)
		}
		if r.Expired(now) {
			return fmt.Errorf("%w: %q lapsed at %s",
				ledger.ErrLeaseExpired, r.ID, r.ExpiresAt.Format(time.RFC3339Nano))
		}

		extend := req.Extend
		if extend == 0 {
			extend = r.Lease
		}
		if extend == 0 {
			return fmt.Errorf("%w: reservation %q has no lease quantum, so renewal needs an explicit extension",
				ledger.ErrInvalidArgument, r.ID)
		}

		// The new deadline is measured from now, not from the old deadline, so a
		// renewal cannot accumulate an unboundedly distant expiry by being called
		// repeatedly. It never moves the deadline backward.
		deadline := now.Add(extend)
		if !r.ExpiresAt.IsZero() && deadline.Before(r.ExpiresAt) {
			deadline = r.ExpiresAt
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE reservations
			    SET expires_at = ?, renewed_at = ?, renew_count = renew_count + 1
			  WHERE id = ? AND state = 'pending'`,
			toUnix(deadline), toUnix(now), r.ID); err != nil {
			return err
		}

		r.ExpiresAt = deadline
		r.RenewedAt = now
		r.RenewCount++
		out = r
		return nil
	})
	return out, err
}

// Settle implements ledger.Ledger.
//
// The charge is applied to exactly the scopes the hold reserved, which is what
// makes a cost land in the period that authorized it even when settlement arrives
// after that period's window closed. An expired reservation may still settle: the
// request happened, so the cost is real. Only an already-resolved reservation is
// refused, because settling twice would double-count money.
func (s *Store) Settle(ctx context.Context, st ledger.Settlement) (ledger.Charge, error) {
	if st.ReservationID == "" {
		return ledger.Charge{}, fmt.Errorf("%w: reservation id is required", ledger.ErrInvalidArgument)
	}

	when := st.CompletedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}

	var c ledger.Charge
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadReservation(ctx, tx, st.ReservationID)
		if err != nil {
			return err
		}
		switch r.State {
		case ledger.StateSettled, ledger.StateReleased:
			return fmt.Errorf("%w: %q is %s", ledger.ErrAlreadyResolved, r.ID, r.State)
		}

		identity := st.Usage.Identity
		if identity == (usage.ModelIdentity{}) {
			identity = r.Identity
		}

		var latency time.Duration
		if !r.CreatedAt.IsZero() && when.After(r.CreatedAt) {
			latency = when.Sub(r.CreatedAt)
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO charges
			 (reservation_id, budget_id, request_id, estimated_cost, reserved_cost, actual_cost,
			  occurred_at, latency_ns, usage_json, identity, policy_actions, metadata)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', ?)`,
			r.ID, r.BudgetID, r.RequestID, int64(r.EstimatedCost), int64(r.Amount),
			int64(st.ActualCost), toUnix(when), int64(latency),
			marshalJSON(st.Usage), marshalJSON(identity), marshalJSON(st.Metadata))
		if err != nil {
			return err
		}
		chargeID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		// One charge leg per reservation leg: the actual cost lands in the same
		// scopes that authorized it, exactly once each.
		legs := make([]ledger.Leg, 0, len(r.Legs))
		for _, leg := range r.Legs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO charge_legs (charge_id, budget_id, period_id, depth, amount)
				 VALUES (?, ?, ?, ?, ?)`,
				chargeID, leg.Scope.BudgetID, leg.Scope.PeriodID, leg.Depth, int64(st.ActualCost)); err != nil {
				return err
			}
			legs = append(legs, ledger.Leg{Scope: leg.Scope, Depth: leg.Depth, Amount: st.ActualCost})
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE reservations SET state = 'settled' WHERE id = ?`, r.ID); err != nil {
			return err
		}

		// Settling may have drained the last hold in a period that was waiting to
		// close.
		for _, leg := range legs {
			if _, err := advance(ctx, tx, leg.Scope.BudgetID, when); err != nil {
				return err
			}
		}

		c = ledger.Charge{
			ID:            fmt.Sprintf("chg-%d", chargeID),
			ReservationID: r.ID,
			BudgetID:      r.BudgetID,
			RequestID:     r.RequestID,
			EstimatedCost: r.EstimatedCost,
			ReservedCost:  r.Amount,
			ActualCost:    st.ActualCost,
			Legs:          legs,
			Usage:         st.Usage,
			Identity:      identity,
			OccurredAt:    when,
			Latency:       latency,
			Metadata:      st.Metadata,
		}
		return nil
	})
	if err != nil {
		return ledger.Charge{}, err
	}
	return c, nil
}

// Release implements ledger.Ledger. The whole leg set is freed at once, since a
// single state change on the reservation row is what makes its legs stop counting.
//
// Unlike Settle, Release takes no timestamp: nothing about it is a dated event,
// so there is no caller-supplied clock to advance periods against. Releasing the
// last outstanding hold of a draining period therefore makes that period
// closeable but does not close it; the next Reserve, EnsurePeriod, or Advance
// does. Reading the wall clock here instead would make period transitions depend
// on a clock the caller never supplied, which is exactly what the injectable-time
// contract exists to prevent.
func (s *Store) Release(ctx context.Context, reservationID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadReservation(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		switch r.State {
		case ledger.StateSettled, ledger.StateReleased:
			return fmt.Errorf("%w: %q is %s", ledger.ErrAlreadyResolved, r.ID, r.State)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE reservations SET state = 'released' WHERE id = ?`, r.ID)
		return err
	})
}

const reservationColumns = `
	SELECT id, budget_id, request_id, amount, estimated_cost, created_at, expires_at,
	       lease_ns, renewed_at, renew_count, state, identity, metadata
	  FROM reservations`

func scanReservation(row rowScanner) (ledger.Reservation, error) {
	var (
		r                          ledger.Reservation
		amount, estimated          int64
		createdAt, expiresAt       int64
		lease, renewedAt           int64
		state                      string
		identityJSON, metadataJSON string
	)
	err := row.Scan(&r.ID, &r.BudgetID, &r.RequestID, &amount, &estimated,
		&createdAt, &expiresAt, &lease, &renewedAt, &r.RenewCount,
		&state, &identityJSON, &metadataJSON)
	if err != nil {
		return ledger.Reservation{}, err
	}

	r.Amount = money.Money(amount)
	r.EstimatedCost = money.Money(estimated)
	r.CreatedAt = fromUnix(createdAt)
	r.ExpiresAt = fromUnix(expiresAt)
	r.Lease = time.Duration(lease)
	r.RenewedAt = fromUnix(renewedAt)
	r.State = ledger.ReservationState(state)
	_ = json.Unmarshal([]byte(identityJSON), &r.Identity)
	_ = json.Unmarshal([]byte(metadataJSON), &r.Metadata)
	return r, nil
}

func loadReservation(ctx context.Context, q querier, id string) (ledger.Reservation, error) {
	r, err := scanReservation(q.QueryRowContext(ctx, reservationColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Reservation{}, fmt.Errorf("%w: %q", ledger.ErrReservationNotFound, id)
	}
	if err != nil {
		return ledger.Reservation{}, err
	}
	r.Legs, err = loadLegs(ctx, q, id)
	return r, err
}

func loadLegs(ctx context.Context, q querier, reservationID string) ([]ledger.Leg, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT budget_id, period_id, depth, amount FROM reservation_legs
		  WHERE reservation_id = ? ORDER BY depth`, reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ledger.Leg
	for rows.Next() {
		var (
			leg    ledger.Leg
			amount int64
		)
		if err := rows.Scan(&leg.Scope.BudgetID, &leg.Scope.PeriodID, &leg.Depth, &amount); err != nil {
			return nil, err
		}
		leg.Amount = money.Money(amount)
		out = append(out, leg)
	}
	return out, rows.Err()
}

// Get implements ledger.Ledger.
func (s *Store) Get(ctx context.Context, reservationID string) (ledger.Reservation, error) {
	var out ledger.Reservation
	err := s.read(ctx, func(q querier) error {
		var err error
		out, err = loadReservation(ctx, q, reservationID)
		return err
	})
	return out, err
}

// RecoverExpired implements ledger.Ledger. This is the crash-recovery path: a
// hold whose owner died stops blocking new spend as soon as its lease lapses, and
// recovery records that fact.
//
// Holds are matched by leg, so recovering a parent budget also reclaims holds
// created against its children.
func (s *Store) RecoverExpired(ctx context.Context, budgetID string, now time.Time) ([]ledger.Reservation, error) {
	var out []ledger.Reservation
	err := s.tx(ctx, func(tx *sql.Tx) error {
		out = nil
		nowNanos := now.UTC().UnixNano()

		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT r.id FROM reservations r
			   JOIN reservation_legs l ON l.reservation_id = r.id
			  WHERE l.budget_id = ? AND r.state = 'pending'
			    AND r.expires_at > 0 AND r.expires_at < ?
			  ORDER BY r.id`,
			budgetID, nowNanos)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE reservations SET state = 'expired' WHERE id = ? AND state = 'pending'`,
				id); err != nil {
				return err
			}
			r, err := loadReservation(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Charges implements ledger.Ledger, newest first over the half-open window.
//
// Charges are selected by leg, so querying a parent budget returns the spend its
// children generated, attributed to the parent's periods.
func (s *Store) Charges(ctx context.Context, scope ledger.Scope, from, to time.Time, limit int) ([]ledger.Charge, error) {
	var q strings.Builder
	q.WriteString(
		`SELECT c.id, c.reservation_id, c.budget_id, c.request_id, c.estimated_cost,
		        c.reserved_cost, cl.amount, c.occurred_at, c.latency_ns,
		        c.usage_json, c.identity, c.metadata
		   FROM charges c
		   JOIN charge_legs cl ON cl.charge_id = c.id
		  WHERE cl.budget_id = ?`)
	args := []any{scope.BudgetID}

	if scope.PeriodID != "" {
		q.WriteString(" AND cl.period_id = ?")
		args = append(args, scope.PeriodID)
	}
	if !from.IsZero() {
		q.WriteString(" AND c.occurred_at >= ?")
		args = append(args, toUnix(from))
	}
	if !to.IsZero() {
		q.WriteString(" AND c.occurred_at < ?")
		args = append(args, toUnix(to))
	}
	q.WriteString(" ORDER BY c.occurred_at DESC, c.id DESC")
	if limit > 0 {
		q.WriteString(" LIMIT ?")
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query charges: %w", err)
	}
	defer rows.Close()

	var (
		out []ledger.Charge
		ids []int64
	)
	for rows.Next() {
		var (
			c                                     ledger.Charge
			id, estimated, reserved, actual       int64
			occurredAt, latency                   int64
			usageJSON, identityJSON, metadataJSON string
		)
		if err := rows.Scan(&id, &c.ReservationID, &c.BudgetID, &c.RequestID,
			&estimated, &reserved, &actual, &occurredAt, &latency,
			&usageJSON, &identityJSON, &metadataJSON); err != nil {
			return nil, err
		}
		c.ID = fmt.Sprintf("chg-%d", id)
		c.EstimatedCost = money.Money(estimated)
		c.ReservedCost = money.Money(reserved)
		c.ActualCost = money.Money(actual)
		c.OccurredAt = fromUnix(occurredAt)
		c.Latency = time.Duration(latency)
		_ = json.Unmarshal([]byte(usageJSON), &c.Usage)
		_ = json.Unmarshal([]byte(identityJSON), &c.Identity)
		_ = json.Unmarshal([]byte(metadataJSON), &c.Metadata)
		out = append(out, c)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach the full leg set so a caller can see every scope a charge touched.
	for i, id := range ids {
		legs, err := chargeLegs(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out[i].Legs = legs
	}
	return out, nil
}

// ChargeFor implements ledger.Ledger.
//
// It reads the charge itself rather than deriving settlement from the
// reservation's state, because a reconciler repairing telemetry needs the
// authoritative amount, timing, and usage -- not merely the knowledge that
// something settled.
func (s *Store) ChargeFor(ctx context.Context, reservationID string) (ledger.Charge, error) {
	if reservationID == "" {
		return ledger.Charge{}, fmt.Errorf("%w: reservation id is required", ledger.ErrInvalidArgument)
	}

	var (
		c                                     ledger.Charge
		id, estimated, reserved, actual       int64
		occurredAt, latency                   int64
		usageJSON, identityJSON, metadataJSON string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, reservation_id, budget_id, request_id, estimated_cost,
		        reserved_cost, actual_cost, occurred_at, latency_ns,
		        usage_json, identity, metadata
		   FROM charges WHERE reservation_id = ?`, reservationID).
		Scan(&id, &c.ReservationID, &c.BudgetID, &c.RequestID, &estimated,
			&reserved, &actual, &occurredAt, &latency,
			&usageJSON, &identityJSON, &metadataJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Charge{}, fmt.Errorf("%w: %q", ledger.ErrChargeNotFound, reservationID)
	}
	if err != nil {
		return ledger.Charge{}, fmt.Errorf("sqlite: charge for %q: %w", reservationID, err)
	}

	c.ID = fmt.Sprintf("chg-%d", id)
	c.EstimatedCost = money.Money(estimated)
	c.ReservedCost = money.Money(reserved)
	c.ActualCost = money.Money(actual)
	c.OccurredAt = fromUnix(occurredAt)
	c.Latency = time.Duration(latency)
	_ = json.Unmarshal([]byte(usageJSON), &c.Usage)
	_ = json.Unmarshal([]byte(identityJSON), &c.Identity)
	_ = json.Unmarshal([]byte(metadataJSON), &c.Metadata)

	legs, err := chargeLegs(ctx, s.db, id)
	if err != nil {
		return ledger.Charge{}, err
	}
	c.Legs = legs
	return c, nil
}

// Reservations implements ledger.Ledger: a read-only, bounded survey.
//
// Oldest first, because the reservations most likely to be stranded are the ones
// that have been outstanding longest, and a bounded pass should reach them before
// it reaches this morning's traffic.
func (s *Store) Reservations(ctx context.Context, states []ledger.ReservationState, limit int) ([]ledger.Reservation, error) {
	var q strings.Builder
	q.WriteString(reservationColumns)
	args := make([]any, 0, len(states)+1)

	if len(states) > 0 {
		q.WriteString(" WHERE state IN (")
		for i, st := range states {
			if i > 0 {
				q.WriteString(", ")
			}
			q.WriteString("?")
			args = append(args, string(st))
		}
		q.WriteString(")")
	}
	q.WriteString(" ORDER BY created_at, id")
	if limit > 0 {
		q.WriteString(" LIMIT ?")
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query reservations: %w", err)
	}
	defer rows.Close()

	var out []ledger.Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Legs are a second read per reservation rather than a join, so a reservation
	// with no legs is still returned: an orphan is exactly the kind of damage this
	// enumeration exists to surface.
	for i := range out {
		legs, err := loadLegs(ctx, s.db, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Legs = legs
	}
	return out, nil
}

func chargeLegs(ctx context.Context, q querier, chargeID int64) ([]ledger.Leg, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT budget_id, period_id, depth, amount FROM charge_legs
		  WHERE charge_id = ? ORDER BY depth`, chargeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ledger.Leg
	for rows.Next() {
		var (
			leg    ledger.Leg
			amount int64
		)
		if err := rows.Scan(&leg.Scope.BudgetID, &leg.Scope.PeriodID, &leg.Depth, &amount); err != nil {
			return nil, err
		}
		leg.Amount = money.Money(amount)
		out = append(out, leg)
	}
	return out, rows.Err()
}

// SortLegs orders legs by depth, so callers can rely on nearest-first order
// regardless of how a store returned them.
func SortLegs(legs []ledger.Leg) {
	sort.Slice(legs, func(i, j int) bool { return legs[i].Depth < legs[j].Depth })
}

// FileDSN builds a DSN for a database file, escaping the path.
func FileDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
}
