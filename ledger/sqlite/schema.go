package sqlite

// schema is the full database layout.
//
// Money is always INTEGER microdollars and timestamps are always Unix
// nanoseconds with 0 meaning unset. STRICT tables make a wrong column type a
// write error rather than a silent conversion, which matters because a REAL
// column would quietly round money.
//
// Two invariants are enforced by the schema itself rather than by application
// code, because a constraint the database checks cannot be forgotten under
// concurrency:
//
//   - charges.reservation_id UNIQUE makes "settle at most once" unrepresentable
//     to violate.
//   - the composite primary keys on reservation_legs and charge_legs make it
//     impossible to charge one scope twice for the same reservation.
const schema = `
CREATE TABLE IF NOT EXISTS budgets (
	id               TEXT PRIMARY KEY,
	parent_id        TEXT,
	name             TEXT    NOT NULL DEFAULT '',
	allocation       INTEGER NOT NULL,
	borrow_ns        INTEGER NOT NULL DEFAULT 0,
	rollover_mode    TEXT    NOT NULL DEFAULT 'none',
	rollover_cap     INTEGER NOT NULL DEFAULT 0,
	rollover_cap_bp  INTEGER NOT NULL DEFAULT 0,
	recurrence       TEXT    NOT NULL,
	recurrence_ns    INTEGER NOT NULL DEFAULT 0,
	timezone         TEXT    NOT NULL DEFAULT 'UTC',
	anchor_at        INTEGER NOT NULL,
	end_at           INTEGER NOT NULL DEFAULT 0,
	fingerprint      TEXT    NOT NULL,
	revision         INTEGER NOT NULL DEFAULT 1,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	FOREIGN KEY (parent_id) REFERENCES budgets (id),
	-- The absolute and percentage cap forms are mutually exclusive.
	CHECK (rollover_cap = 0 OR rollover_cap_bp = 0),
	CHECK (rollover_cap >= 0 AND rollover_cap_bp >= 0),
	CHECK (allocation >= 0),
	CHECK (id <> parent_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_budgets_parent ON budgets (parent_id);

-- Materialized periods. allocation, carry, and borrow_ns are snapshots taken at
-- creation, so editing a definition cannot retroactively rewrite history.
CREATE TABLE IF NOT EXISTS periods (
	id               TEXT PRIMARY KEY,
	budget_id        TEXT    NOT NULL,
	seq              INTEGER NOT NULL,
	start_at         INTEGER NOT NULL,
	end_at           INTEGER NOT NULL,
	allocation       INTEGER NOT NULL,
	carry            INTEGER NOT NULL DEFAULT 0,
	borrow_ns        INTEGER NOT NULL DEFAULT 0,
	rollover_mode    TEXT    NOT NULL DEFAULT 'none',
	rollover_cap     INTEGER NOT NULL DEFAULT 0,
	rollover_cap_bp  INTEGER NOT NULL DEFAULT 0,
	state            TEXT    NOT NULL DEFAULT 'open',
	carry_final      INTEGER NOT NULL DEFAULT 0,
	closing_balance  INTEGER NOT NULL DEFAULT 0,
	closed_at        INTEGER NOT NULL DEFAULT 0,
	-- Two processes racing to materialize the same period cannot both win.
	UNIQUE (budget_id, seq),
	UNIQUE (budget_id, start_at),
	FOREIGN KEY (budget_id) REFERENCES budgets (id),
	CHECK (end_at > start_at),
	CHECK (state IN ('open', 'draining', 'closed'))
) STRICT;

CREATE INDEX IF NOT EXISTS idx_periods_budget_state ON periods (budget_id, state);
CREATE INDEX IF NOT EXISTS idx_periods_budget_time  ON periods (budget_id, start_at);

CREATE TABLE IF NOT EXISTS reservations (
	id              TEXT PRIMARY KEY,
	budget_id       TEXT    NOT NULL,
	request_id      TEXT    NOT NULL DEFAULT '',
	amount          INTEGER NOT NULL,
	estimated_cost  INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL DEFAULT 0,
	lease_ns        INTEGER NOT NULL DEFAULT 0,
	renewed_at      INTEGER NOT NULL DEFAULT 0,
	renew_count     INTEGER NOT NULL DEFAULT 0,
	state           TEXT    NOT NULL,
	identity        TEXT    NOT NULL DEFAULT '{}',
	metadata        TEXT    NOT NULL DEFAULT '{}',
	FOREIGN KEY (budget_id) REFERENCES budgets (id),
	CHECK (amount >= 0),
	CHECK (state IN ('pending', 'settled', 'released', 'expired'))
) STRICT;

CREATE INDEX IF NOT EXISTS idx_reservations_state ON reservations (state, expires_at);

-- One leg per scope a hold consumes. depth 0 is the named budget, 1 its parent.
CREATE TABLE IF NOT EXISTS reservation_legs (
	reservation_id  TEXT    NOT NULL,
	budget_id       TEXT    NOT NULL,
	period_id       TEXT    NOT NULL,
	depth           INTEGER NOT NULL,
	amount          INTEGER NOT NULL,
	-- One leg per (reservation, budget): an ancestor cannot be double-held even
	-- if a cycle somehow reached this far.
	PRIMARY KEY (reservation_id, budget_id),
	FOREIGN KEY (reservation_id) REFERENCES reservations (id) ON DELETE CASCADE,
	FOREIGN KEY (period_id)      REFERENCES periods (id),
	CHECK (amount >= 0)
) STRICT;

-- The hot path: sum live holds for one scope.
CREATE INDEX IF NOT EXISTS idx_res_legs_scope  ON reservation_legs (budget_id, period_id);
CREATE INDEX IF NOT EXISTS idx_res_legs_period ON reservation_legs (period_id);

CREATE TABLE IF NOT EXISTS charges (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	-- Settling twice is impossible rather than merely checked.
	reservation_id  TEXT    NOT NULL UNIQUE,
	budget_id       TEXT    NOT NULL,
	request_id      TEXT    NOT NULL DEFAULT '',
	estimated_cost  INTEGER NOT NULL DEFAULT 0,
	reserved_cost   INTEGER NOT NULL DEFAULT 0,
	actual_cost     INTEGER NOT NULL,
	occurred_at     INTEGER NOT NULL,
	latency_ns      INTEGER NOT NULL DEFAULT 0,
	usage_json      TEXT    NOT NULL DEFAULT '{}',
	identity        TEXT    NOT NULL DEFAULT '{}',
	policy_actions  TEXT    NOT NULL DEFAULT '[]',
	metadata        TEXT    NOT NULL DEFAULT '{}',
	FOREIGN KEY (reservation_id) REFERENCES reservations (id)
) STRICT;

-- Mirrors reservation_legs: period_id is where the money lands, which is the
-- period that authorized the request even if settlement arrived later.
CREATE TABLE IF NOT EXISTS charge_legs (
	charge_id   INTEGER NOT NULL,
	budget_id   TEXT    NOT NULL,
	period_id   TEXT    NOT NULL,
	depth       INTEGER NOT NULL,
	amount      INTEGER NOT NULL,
	PRIMARY KEY (charge_id, budget_id),
	FOREIGN KEY (charge_id) REFERENCES charges (id) ON DELETE CASCADE,
	FOREIGN KEY (period_id) REFERENCES periods (id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_charge_legs_scope ON charge_legs (budget_id, period_id);
CREATE INDEX IF NOT EXISTS idx_charge_legs_period ON charge_legs (period_id);
`
