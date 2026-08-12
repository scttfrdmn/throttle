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

// Period transition semantics
//
// A period moves open -> draining -> closed.
//
//   - open: the period's time window includes now, and it accepts reservations.
//   - draining: the window has passed, but holds it authorized may still be in
//     flight. No new reservations; existing holds may still settle against it.
//   - closed: every hold has resolved. The closing balance is final and never
//     changes again.
//
// A charge always lands in the period that authorized its reservation, even when
// settlement arrives after that period's window ended. This keeps attribution
// honest: work done in August is August's cost, and it was admitted against
// August's pacing curve, so it must not silently consume September's money.
//
// The consequence is that a successor period can begin while its predecessor is
// still draining, before the predecessor's final balance is known. Rather than
// delay the successor or restate its carry later, the successor starts on a
// provisional carry computed as if every outstanding hold settles at its full
// reserved amount. That can only understate the carry, never overstate it, so a
// successor never spends money its predecessor turns out to have needed. When the
// predecessor finishes draining, the carry is finalized -- which can only revise
// it upward.

// EnsurePeriod implements ledger.Ledger.
func (s *Store) EnsurePeriod(ctx context.Context, budgetID string, at time.Time) (ledger.Period, error) {
	var out ledger.Period
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = ensurePeriod(ctx, tx, budgetID, at)
		return err
	})
	return out, err
}

// ensurePeriod materializes the period containing at, plus any earlier periods
// that were never materialized, so that carry chains through unused periods
// rather than skipping them.
//
// Gaps are real: a budget with balance rollover that goes unused for two months
// must still accumulate (or repay) across them. Materializing lazily but
// completely keeps the chain correct without needing a daemon.
func ensurePeriod(ctx context.Context, tx *sql.Tx, budgetID string, at time.Time) (ledger.Period, error) {
	def, _, err := scanDefinition(tx.QueryRowContext(ctx, definitionColumns+` WHERE id = ?`, budgetID))
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Period{}, fmt.Errorf("%w: %q", ledger.ErrBudgetNotFound, budgetID)
	}
	if err != nil {
		return ledger.Period{}, err
	}

	seq, err := def.PeriodFor(at)
	if err != nil {
		return ledger.Period{}, err
	}

	// Settle existing periods' states first. A predecessor with nothing
	// outstanding should close here, so its successor inherits a final carry
	// rather than a provisional one that would only ever be revised to the same
	// value.
	if _, err := advance(ctx, tx, budgetID, at); err != nil {
		return ledger.Period{}, err
	}

	// Highest sequence already materialized, if any.
	var highest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM periods WHERE budget_id = ?`, budgetID).Scan(&highest); err != nil {
		return ledger.Period{}, err
	}

	from := 0
	if highest.Valid {
		if int(highest.Int64) >= seq {
			// Already materialized; just load it.
			return loadPeriod(ctx, tx, def.PeriodID(seq))
		}
		from = int(highest.Int64) + 1
	}

	// Materialize forward from the first missing period so carry chains.
	for n := from; n <= seq; n++ {
		if err := materialize(ctx, tx, def, n); err != nil {
			return ledger.Period{}, err
		}
	}

	// Each period is created open, so a gap fill leaves a run of elapsed periods
	// whose carry is flagged provisional even though nothing was ever outstanding
	// in them. Advancing again settles that run in sequence order, finalizing each
	// carry as its predecessor closes.
	if _, err := advance(ctx, tx, budgetID, at); err != nil {
		return ledger.Period{}, err
	}
	return loadPeriod(ctx, tx, def.PeriodID(seq))
}

// materialize inserts one period, computing its carry from its predecessor.
func materialize(ctx context.Context, tx *sql.Tx, def budget.Definition, seq int) error {
	carry, carryFinal, err := carryFor(ctx, tx, def, seq)
	if err != nil {
		return err
	}

	env, err := def.Envelope(seq, carry)
	if err != nil {
		return err
	}

	finalFlag := 0
	if carryFinal {
		finalFlag = 1
	}

	// INSERT OR IGNORE plus the UNIQUE (budget_id, seq) constraint makes this
	// idempotent: two processes racing to create the same period both succeed and
	// exactly one row exists.
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO periods
		 (id, budget_id, seq, start_at, end_at, allocation, carry, borrow_ns,
		  rollover_mode, rollover_cap, rollover_cap_bp, state, carry_final)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)`,
		env.ID, def.ID, seq, env.Start.UnixNano(), env.End.UnixNano(),
		int64(env.Allocation), int64(carry), int64(env.Borrow),
		rolloverMode(env.Rollover.Mode), int64(env.Rollover.Cap), env.Rollover.CapBasisPoints,
		finalFlag)
	return err
}

// carryFor computes the carry entering period seq, and whether that carry is
// final.
//
// Carry is provisional exactly when the predecessor is still draining, because a
// hold outstanding there may yet settle for less than it reserved.
func carryFor(ctx context.Context, tx *sql.Tx, def budget.Definition, seq int) (money.Money, bool, error) {
	if seq == 0 {
		return 0, true, nil // Nothing precedes the first period.
	}

	prev, err := loadPeriod(ctx, tx, def.PeriodID(seq-1))
	if errors.Is(err, ledger.ErrNoSuchPeriodRow) {
		// No predecessor row: nothing to carry. This happens when a definition's
		// anchor is such that period 0 is the first ever materialized.
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}

	if prev.State == ledger.StateClosed {
		return prev.Envelope.Rollover.CarryInto(prev.ClosingBalance, prev.Envelope.Allocation), true, nil
	}

	// The predecessor is open or draining. Use the conservative provisional
	// balance: assume every outstanding hold settles in full.
	spent, reserved, err := periodCommitted(ctx, tx, prev.ID)
	if err != nil {
		return 0, false, err
	}
	balance := prev.Envelope.ProvisionalClose(spent, reserved)
	return prev.Envelope.Rollover.CarryInto(balance, prev.Envelope.Allocation), false, nil
}

// periodCommitted returns settled spend and live-plus-expired holds for a period.
//
// Expired holds are included here deliberately: for the provisional carry the
// question is "what might still be charged to this period", and an expired hold
// can still settle. This is the opposite of the Totals question, where an expired
// hold must stop blocking new spend.
func periodCommitted(ctx context.Context, q querier, periodID string) (spent, reserved money.Money, err error) {
	if err = q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM charge_legs WHERE period_id = ?`,
		periodID).Scan(&spent); err != nil {
		return 0, 0, err
	}
	err = q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(l.amount), 0)
		   FROM reservation_legs l
		   JOIN reservations r ON r.id = l.reservation_id
		  WHERE l.period_id = ? AND r.state IN ('pending', 'expired')`,
		periodID).Scan(&reserved)
	return spent, reserved, err
}

const periodColumns = `
	SELECT id, budget_id, seq, start_at, end_at, allocation, carry, borrow_ns,
	       rollover_mode, rollover_cap, rollover_cap_bp, state, carry_final,
	       closing_balance, closed_at
	  FROM periods`

func scanPeriod(row rowScanner) (ledger.Period, error) {
	var (
		p                        ledger.Period
		start, end               int64
		allocation, carry, borro int64
		cap_, capBP              int64
		mode, state              string
		carryFinal               int64
		closing, closedAt        int64
	)
	err := row.Scan(&p.ID, &p.BudgetID, &p.Seq, &start, &end, &allocation, &carry, &borro,
		&mode, &cap_, &capBP, &state, &carryFinal, &closing, &closedAt)
	if err != nil {
		return ledger.Period{}, err
	}

	p.Envelope = budget.Envelope{
		ID:         p.ID,
		Allocation: money.Money(allocation),
		Carry:      money.Money(carry),
		Start:      time.Unix(0, start).UTC(),
		End:        time.Unix(0, end).UTC(),
		Borrow:     time.Duration(borro),
		Rollover: budget.RolloverPolicy{
			Mode:           budget.RolloverMode(mode),
			Cap:            money.Money(cap_),
			CapBasisPoints: capBP,
		},
	}
	p.State = ledger.PeriodState(state)
	p.CarryFinal = carryFinal != 0
	p.ClosingBalance = money.Money(closing)
	p.ClosedAt = fromUnix(closedAt)
	return p, nil
}

func loadPeriod(ctx context.Context, q querier, periodID string) (ledger.Period, error) {
	p, err := scanPeriod(q.QueryRowContext(ctx, periodColumns+` WHERE id = ?`, periodID))
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Period{}, fmt.Errorf("%w: %q", ledger.ErrNoSuchPeriodRow, periodID)
	}
	return p, err
}

// Period implements ledger.Ledger.
func (s *Store) Period(ctx context.Context, periodID string) (ledger.Period, error) {
	var out ledger.Period
	err := s.read(ctx, func(q querier) error {
		var err error
		out, err = loadPeriod(ctx, q, periodID)
		return err
	})
	return out, err
}

// Periods implements ledger.Ledger.
func (s *Store) Periods(ctx context.Context, budgetID string) ([]ledger.Period, error) {
	rows, err := s.db.QueryContext(ctx, periodColumns+` WHERE budget_id = ? ORDER BY seq`, budgetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ledger.Period
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Advance implements ledger.Ledger, performing all due transitions for a budget.
func (s *Store) Advance(ctx context.Context, budgetID string, now time.Time) ([]ledger.Period, error) {
	var out []ledger.Period
	err := s.tx(ctx, func(tx *sql.Tx) error {
		out = nil
		var err error
		out, err = advance(ctx, tx, budgetID, now)
		return err
	})
	return out, err
}

// advance moves every period of a budget to its correct state for now.
//
// It is idempotent and cheap when there is nothing to do, which is what lets it
// run implicitly on the reservation path rather than needing a scheduler.
func advance(ctx context.Context, tx *sql.Tx, budgetID string, now time.Time) ([]ledger.Period, error) {
	rows, err := tx.QueryContext(ctx,
		periodColumns+` WHERE budget_id = ? AND state <> 'closed' ORDER BY seq`, budgetID)
	if err != nil {
		return nil, err
	}
	var candidates []ledger.Period
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nowNanos := now.UTC().UnixNano()
	var changed []ledger.Period

	for _, p := range candidates {
		// An open period whose window has passed stops accepting new work.
		if p.State == ledger.StateOpen && p.Envelope.End.UnixNano() <= nowNanos {
			if _, err := tx.ExecContext(ctx,
				`UPDATE periods SET state = 'draining' WHERE id = ? AND state = 'open'`, p.ID); err != nil {
				return nil, err
			}
			p.State = ledger.StateDraining
			changed = append(changed, p)
		}

		if p.State != ledger.StateDraining {
			continue
		}

		// A draining period closes once nothing can still be charged to it.
		// Expired holds count as outstanding: an expired lease can still settle,
		// so closing over it would risk restating a closed balance.
		var outstanding int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1)
			   FROM reservation_legs l
			   JOIN reservations r ON r.id = l.reservation_id
			  WHERE l.period_id = ? AND r.state IN ('pending', 'expired')`,
			p.ID).Scan(&outstanding); err != nil {
			return nil, err
		}
		if outstanding > 0 {
			continue
		}

		spent, _, err := periodCommitted(ctx, tx, p.ID)
		if err != nil {
			return nil, err
		}
		balance := p.Envelope.Close(spent)
		if _, err := tx.ExecContext(ctx,
			`UPDATE periods SET state = 'closed', closing_balance = ?, closed_at = ?, carry_final = 1
			 WHERE id = ? AND state = 'draining'`,
			int64(balance), nowNanos, p.ID); err != nil {
			return nil, err
		}
		p.State = ledger.StateClosed
		p.ClosingBalance = balance
		p.CarryFinal = true
		changed = append(changed, p)

		// Closing finalizes the successor's carry, which can only revise it
		// upward: the provisional value assumed every hold settled in full.
		if err := finalizeSuccessorCarry(ctx, tx, p, balance); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// finalizeSuccessorCarry updates the next period's carry once this period's
// balance is final.
//
// Only a successor that is still provisional is touched, and a closed successor is
// never rewritten: a closed period's books do not change.
func finalizeSuccessorCarry(ctx context.Context, tx *sql.Tx, closed ledger.Period, balance money.Money) error {
	def, _, err := scanDefinition(tx.QueryRowContext(ctx, definitionColumns+` WHERE id = ?`, closed.BudgetID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	carry := closed.Envelope.Rollover.CarryInto(balance, closed.Envelope.Allocation)
	_, err = tx.ExecContext(ctx,
		`UPDATE periods SET carry = ?, carry_final = 1
		 WHERE id = ? AND carry_final = 0 AND state <> 'closed'`,
		int64(carry), def.PeriodID(closed.Seq+1))
	return err
}
