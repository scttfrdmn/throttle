// Package sqlite is a durable activity.Store backed by SQLite.
//
// It shares the ledger's storage conventions: integer microdollars, Unix
// nanosecond timestamps with 0 meaning unset, and STRICT tables so a wrong column
// type is a write error rather than a silent conversion.
//
// It is a separate database handle from the ledger by design. Activity is
// observability: a failure to record it must never be able to fail or slow the
// accounting transaction that governs real money. The two can point at the same
// file, but they do not share a transaction.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"

	_ "modernc.org/sqlite"
)

// schema is the activity layout.
//
// One row per request, keyed on request_id so that a retry after an ambiguous
// failure updates the same record rather than creating a second story about the
// same call. Deliberately absent: any column that could hold a prompt, a message,
// or a generated response.
const schema = `
CREATE TABLE IF NOT EXISTS activity (
	request_id        TEXT PRIMARY KEY,
	reservation_id    TEXT    NOT NULL DEFAULT '',
	budget_id         TEXT    NOT NULL,
	scopes            TEXT    NOT NULL DEFAULT '[]',

	-- Identity dimensions, kept as columns rather than buried in JSON because
	-- "spend by publisher" and "spend by model" are the questions this table
	-- exists to answer.
	access_provider   TEXT    NOT NULL DEFAULT '',
	publisher         TEXT    NOT NULL DEFAULT '',
	canonical_model   TEXT    NOT NULL DEFAULT '',
	provider_model_id TEXT    NOT NULL DEFAULT '',
	operation         TEXT    NOT NULL DEFAULT '',
	region            TEXT    NOT NULL DEFAULT '',
	service_tier      TEXT    NOT NULL DEFAULT '',
	identity          TEXT    NOT NULL DEFAULT '{}',

	estimate_quality  TEXT    NOT NULL DEFAULT '',
	estimate_note     TEXT    NOT NULL DEFAULT '',
	estimated_usage   TEXT    NOT NULL DEFAULT '{}',
	estimated_cost    TEXT    NOT NULL DEFAULT '{}',

	-- The captured rate set, so this request stays re-priceable after the catalog
	-- moves on. Provenance travels inside it.
	quote             TEXT    NOT NULL DEFAULT '{}',
	-- The same, for a compound request whose models were discovered rather than
	-- named: the frozen quotes covering the invocations that actually happened.
	quotes            TEXT    NOT NULL DEFAULT '{}',

	reserved_cost     INTEGER NOT NULL DEFAULT 0,

	actual_usage      TEXT    NOT NULL DEFAULT '{}',
	-- Cost is an object, not a number: completeness and unpriced dimensions must
	-- survive the round trip, or a floor reads back as a total.
	actual_cost       TEXT    NOT NULL DEFAULT '{}',
	-- Denormalized out of actual_cost so "what is unresolved?" is an indexed query
	-- rather than a JSON scan of every row.
	cost_completeness TEXT    NOT NULL DEFAULT 'unknown',
	cost_amount       INTEGER NOT NULL DEFAULT 0,

	enforcement_mode  TEXT    NOT NULL DEFAULT '',
	status            TEXT    NOT NULL,
	outcome           TEXT    NOT NULL DEFAULT '',
	error             TEXT    NOT NULL DEFAULT '',

	started_at        INTEGER NOT NULL DEFAULT 0,
	completed_at      INTEGER NOT NULL DEFAULT 0,
	latency_ns        INTEGER NOT NULL DEFAULT 0,
	provider_latency_ns INTEGER NOT NULL DEFAULT 0,

	-- Streaming timings, zero for a non-streaming request. Durations, not content:
	-- how long until a stream existed, and how long until it said anything.
	stream_established_ns INTEGER NOT NULL DEFAULT 0,
	stream_first_event_ns INTEGER NOT NULL DEFAULT 0,

	-- Managed-agent identifiers, columned because "spend by agent" is the question
	-- a compound invocation exists to answer. Empty for an ordinary request.
	agent_id          TEXT    NOT NULL DEFAULT '',
	agent_alias_id    TEXT    NOT NULL DEFAULT '',
	agent_session_id  TEXT    NOT NULL DEFAULT '',
	-- The normalized per-step accounting detail: identity, usage, cost, timing.
	-- Never a serialized provider trace object, and no prompt, response, rationale,
	-- action payload, retrieved passage, or collaborator message reaches it.
	agent             TEXT    NOT NULL DEFAULT '{}',

	-- Hosted-runtime linkage, columned because these are the keys a later
	-- reconciliation queries by, not fields a dashboard reads. Empty otherwise.
	--
	-- runtime_session_id is indexed on its own: on a hosted runtime it is the only
	-- identifier that appears both on the API call and in the resource-usage
	-- telemetry, so "find every request belonging to this session" is the query
	-- delayed reconciliation is made of.
	runtime_id          TEXT    NOT NULL DEFAULT '',
	runtime_qualifier   TEXT    NOT NULL DEFAULT '',
	runtime_session_id  TEXT    NOT NULL DEFAULT '',
	runtime_reconciled  INTEGER NOT NULL DEFAULT 0,
	-- Identifiers, sizes, status, and any reconciled resource usage. No payload:
	-- a hosted runtime's request and response bodies are opaque bytes in a
	-- caller-declared format, and nothing here can hold them.
	runtime             TEXT    NOT NULL DEFAULT '{}',

	-- The append-only audit trail of automatic reconciliation. JSON rather than a
	-- second table because it is read only with its own record, never queried
	-- across records, and a repair is meaningless without the row it repaired.
	repairs           TEXT    NOT NULL DEFAULT '[]',

	metadata          TEXT    NOT NULL DEFAULT '{}',

	CHECK (status IN ('settled','denied','released','unresolved','outstanding','pending')),
	CHECK (cost_completeness IN ('known','partial','unknown'))
) STRICT;

CREATE INDEX IF NOT EXISTS idx_activity_budget_time ON activity (budget_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_activity_status      ON activity (status);
CREATE INDEX IF NOT EXISTS idx_activity_model       ON activity (provider_model_id);
-- The "3 unpriced requests" query.
CREATE INDEX IF NOT EXISTS idx_activity_incomplete  ON activity (cost_completeness) WHERE cost_completeness <> 'known';

-- One row per scope a request consumed, so a parent budget can total its
-- descendants' activity without re-deriving the hierarchy.
CREATE TABLE IF NOT EXISTS activity_scopes (
	request_id  TEXT    NOT NULL,
	budget_id   TEXT    NOT NULL,
	period_id   TEXT    NOT NULL,
	depth       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (request_id, budget_id),
	FOREIGN KEY (request_id) REFERENCES activity (request_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX IF NOT EXISTS idx_activity_scopes_scope ON activity_scopes (budget_id, period_id);
`

// Store is a durable activity store.
type Store struct {
	db *sql.DB
}

var _ activity.Store = (*Store)(nil)

// Open opens or creates an activity database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	memory := path == ":memory:" || strings.Contains(path, "mode=memory")

	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?_txlock=immediate" +
			"&_pragma=busy_timeout(10000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=foreign_keys(ON)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("activity/sqlite: open %q: %w", path, err)
	}
	if memory {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	// Retried while the file reports itself locked. Switching a fresh database into WAL mode
	// takes an exclusive lock that SQLite does not run the busy handler for, so two
	// processes opening a new activity store in the same instant would otherwise see one
	// succeed and the other fail. Both steps are idempotent, so waiting is the whole fix.
	if err := openRetry(ctx, func() error { return db.PingContext(ctx) }); err != nil {
		db.Close()
		return nil, fmt.Errorf("activity/sqlite: ping %q: %w", path, err)
	}
	if err := openRetry(ctx, func() error {
		_, err := db.ExecContext(ctx, schema)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("activity/sqlite: migrate: %w", err)
	}
	if err := openRetry(ctx, func() error { return addColumns(ctx, db) }); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// openRetry retries fn while SQLite reports the database as locked. See the ledger store's
// copy for why the one-time schema and journal-mode steps need it.
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

// isLocked reports whether err is SQLite's busy/locked condition. The driver's error type is
// unexported, so this reads the result code it names in the message.
func isLocked(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "SQLITE_LOCKED") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked")
}

// addedColumns are columns introduced after the first release, added to an
// existing database rather than being present from the start.
//
// CREATE TABLE IF NOT EXISTS silently does nothing to a table that already
// exists, so a new column in `schema` would be present for a fresh database and
// absent for an existing one -- and the INSERT would then fail for every write.
// Rather than a version-stamped migration framework for two columns, these are
// added idempotently and a duplicate is not an error.
var addedColumns = []string{
	`ALTER TABLE activity ADD COLUMN stream_established_ns INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN stream_first_event_ns INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN agent_id         TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN agent_alias_id   TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN agent_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN agent            TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE activity ADD COLUMN quotes           TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE activity ADD COLUMN runtime_id         TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN runtime_qualifier  TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN runtime_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN runtime_reconciled INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN runtime            TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE activity ADD COLUMN repairs            TEXT NOT NULL DEFAULT '[]'`,
}

// addedIndexes are indexes over addedColumns. They are created after the ALTERs
// rather than in schema, because an index in schema referencing a column that
// arrives by ALTER would fail on an existing database -- the very case the ALTERs
// exist for.
var addedIndexes = []string{
	// "spend by agent", the compound-invocation counterpart of spend by model.
	`CREATE INDEX IF NOT EXISTS idx_activity_agent ON activity (agent_id) WHERE agent_id <> ''`,
	// The reconciliation query: every request in a runtime session whose resource
	// usage has not yet been attributed.
	`CREATE INDEX IF NOT EXISTS idx_activity_runtime_session ON activity (runtime_session_id) WHERE runtime_session_id <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_activity_runtime_pending ON activity (runtime_session_id) WHERE runtime_session_id <> '' AND runtime_reconciled = 0`,
}

func addColumns(ctx context.Context, db *sql.DB) error {
	for _, stmt := range addedColumns {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("activity/sqlite: migrate: %q: %w", stmt, err)
		}
	}
	for _, stmt := range addedIndexes {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("activity/sqlite: migrate: %q: %w", stmt, err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only reporting. Callers must not write through it.
func (s *Store) DB() *sql.DB { return s.db }

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

// boolToInt stores a flag as an integer, because STRICT tables have no boolean
// type and an implicit conversion is exactly what STRICT exists to refuse.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Begin implements activity.Store. It upserts, so a retried request updates its
// own record rather than creating a rival account of the same call.
func (s *Store) Begin(ctx context.Context, r activity.Record) error {
	if r.RequestID == "" {
		return errors.New("activity/sqlite: request id is required")
	}
	if r.Status == "" {
		r.Status = activity.StatusPending
	}
	return s.write(ctx, r)
}

// Complete implements activity.Store. It is the same upsert: a record that
// resolves overwrites its own pre-call row, and a Complete with no prior Begin
// still lands, so a caller that skips the pre-call write is not silently dropped.
func (s *Store) Complete(ctx context.Context, r activity.Record) error {
	if r.RequestID == "" {
		return errors.New("activity/sqlite: request id is required")
	}
	if r.Status == "" {
		return errors.New("activity/sqlite: a completed record needs a status")
	}
	return s.write(ctx, r)
}

func (s *Store) write(ctx context.Context, r activity.Record) error {
	// State rather than the raw field: the zero value is the empty string, which the
	// column's CHECK constraint would reject.
	completeness := r.ActualCost.State()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("activity/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity (
			request_id, reservation_id, budget_id, scopes,
			access_provider, publisher, canonical_model, provider_model_id, operation,
			region, service_tier, identity,
			estimate_quality, estimate_note, estimated_usage, estimated_cost,
			quote, quotes, reserved_cost,
			actual_usage, actual_cost, cost_completeness, cost_amount,
			enforcement_mode, status, outcome, error,
			started_at, completed_at, latency_ns, provider_latency_ns,
			stream_established_ns, stream_first_event_ns,
			agent_id, agent_alias_id, agent_session_id, agent,
			runtime_id, runtime_qualifier, runtime_session_id, runtime_reconciled, runtime,
			repairs, metadata
		 ) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?, ?,?,?,?, ?,?,?,?,?, ?,?)
		 ON CONFLICT (request_id) DO UPDATE SET
			reservation_id = excluded.reservation_id,
			budget_id = excluded.budget_id,
			scopes = excluded.scopes,
			access_provider = excluded.access_provider,
			publisher = excluded.publisher,
			canonical_model = excluded.canonical_model,
			provider_model_id = excluded.provider_model_id,
			operation = excluded.operation,
			region = excluded.region,
			service_tier = excluded.service_tier,
			identity = excluded.identity,
			estimate_quality = excluded.estimate_quality,
			estimate_note = excluded.estimate_note,
			estimated_usage = excluded.estimated_usage,
			estimated_cost = excluded.estimated_cost,
			quote = excluded.quote,
			quotes = excluded.quotes,
			reserved_cost = excluded.reserved_cost,
			actual_usage = excluded.actual_usage,
			actual_cost = excluded.actual_cost,
			cost_completeness = excluded.cost_completeness,
			cost_amount = excluded.cost_amount,
			enforcement_mode = excluded.enforcement_mode,
			status = excluded.status,
			outcome = excluded.outcome,
			error = excluded.error,
			completed_at = excluded.completed_at,
			latency_ns = excluded.latency_ns,
			provider_latency_ns = excluded.provider_latency_ns,
			stream_established_ns = excluded.stream_established_ns,
			stream_first_event_ns = excluded.stream_first_event_ns,
			agent_id = excluded.agent_id,
			agent_alias_id = excluded.agent_alias_id,
			agent_session_id = excluded.agent_session_id,
			agent = excluded.agent,
			runtime_id = excluded.runtime_id,
			runtime_qualifier = excluded.runtime_qualifier,
			runtime_session_id = excluded.runtime_session_id,
			runtime_reconciled = excluded.runtime_reconciled,
			runtime = excluded.runtime,
			repairs = excluded.repairs,
			metadata = excluded.metadata`,
		r.RequestID, r.ReservationID, r.BudgetID, marshalJSON(r.Scopes),
		r.Identity.AccessProvider, r.Identity.Publisher, r.Identity.CanonicalModel,
		r.Identity.ProviderModelID, r.Identity.Operation,
		r.Identity.Region, r.Identity.ServiceTier, marshalJSON(r.Identity),
		string(r.Estimate.Quality), r.Estimate.Note,
		marshalJSON(r.Estimate.Usage), marshalJSON(r.Estimate.Cost),
		quoteJSON(r.Quote), quoteSetJSON(r.Quotes), int64(r.Reserved),
		marshalJSON(r.ActualUsage), marshalJSON(r.ActualCost),
		string(completeness), int64(r.ActualCost.Amount),
		string(r.EnforcementMode), string(r.Status), string(r.Outcome), r.Error,
		toUnix(r.StartedAt), toUnix(r.CompletedAt), int64(r.Latency), int64(r.ProviderLatency),
		int64(r.StreamEstablished), int64(r.StreamFirstEvent),
		r.Agent.AgentID, r.Agent.AliasID, r.Agent.SessionID, marshalJSON(r.Agent),
		r.Runtime.RuntimeID, r.Runtime.Qualifier, r.Runtime.SessionID,
		boolToInt(r.Runtime.Reconciled), marshalJSON(r.Runtime),
		repairsJSON(r.Repairs), marshalJSON(r.Metadata),
	); err != nil {
		return fmt.Errorf("activity/sqlite: write %q: %w", r.RequestID, err)
	}

	// Scope rows are rewritten wholesale: the chain a request consumed cannot
	// change between Begin and Complete, but a Complete without a prior Begin must
	// still end up with them.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM activity_scopes WHERE request_id = ?`, r.RequestID); err != nil {
		return fmt.Errorf("activity/sqlite: clear scopes: %w", err)
	}
	for _, sc := range r.Scopes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO activity_scopes (request_id, budget_id, period_id, depth)
			 VALUES (?,?,?,?)`,
			r.RequestID, sc.BudgetID, sc.PeriodID, sc.Depth); err != nil {
			return fmt.Errorf("activity/sqlite: write scope: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("activity/sqlite: commit: %w", err)
	}
	return nil
}

// repairsJSON serializes the reconciliation trail, defaulting to an empty array
// rather than to "{}" so the column's own type never changes shape.
func repairsJSON(rs []activity.Reconciliation) string {
	if len(rs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func quoteJSON(q pricing.CapturedQuote) string {
	s, err := pricing.MarshalQuote(q)
	if err != nil {
		return "{}"
	}
	return s
}

func quoteSetJSON(s pricing.QuoteSet) string {
	if !s.Valid() {
		return "{}"
	}
	out, err := pricing.MarshalQuoteSet(s)
	if err != nil {
		return "{}"
	}
	return out
}

const columns = `
	SELECT request_id, reservation_id, budget_id,
	       identity, estimate_quality, estimate_note, estimated_usage, estimated_cost,
	       quote, quotes, reserved_cost, actual_usage, actual_cost,
	       enforcement_mode, status, outcome, error,
	       started_at, completed_at, latency_ns, provider_latency_ns,
	       stream_established_ns, stream_first_event_ns, agent, runtime, repairs, metadata
	  FROM activity`

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (activity.Record, error) {
	var (
		r                                              activity.Record
		identity, estUsage, estCost, quote, quotes     string
		actUsage, actCost, agent, runtime, metadata    string
		repairs                                        string
		quality, note, mode, status, outcome, errText  string
		reserved                                       int64
		startedAt, completedAt, latency, providerLatcy int64
		established, firstEvent                        int64
	)
	if err := row.Scan(
		&r.RequestID, &r.ReservationID, &r.BudgetID,
		&identity, &quality, &note, &estUsage, &estCost,
		&quote, &quotes, &reserved, &actUsage, &actCost,
		&mode, &status, &outcome, &errText,
		&startedAt, &completedAt, &latency, &providerLatcy,
		&established, &firstEvent, &agent, &runtime, &repairs, &metadata,
	); err != nil {
		return activity.Record{}, err
	}

	if err := json.Unmarshal([]byte(identity), &r.Identity); err != nil {
		return activity.Record{}, fmt.Errorf("activity/sqlite: identity: %w", err)
	}
	r.Estimate.Quality = usage.Quality(quality)
	r.Estimate.Note = note
	if err := json.Unmarshal([]byte(estUsage), &r.Estimate.Usage); err != nil {
		return activity.Record{}, fmt.Errorf("activity/sqlite: estimated usage: %w", err)
	}
	if err := json.Unmarshal([]byte(estCost), &r.Estimate.Cost); err != nil {
		return activity.Record{}, fmt.Errorf("activity/sqlite: estimated cost: %w", err)
	}
	q, err := pricing.UnmarshalQuote(quote)
	if err != nil {
		return activity.Record{}, err
	}
	r.Quote = q
	qs, err := pricing.UnmarshalQuoteSet(quotes)
	if err != nil {
		return activity.Record{}, err
	}
	r.Quotes = qs
	r.Reserved = money.Money(reserved)
	if err := json.Unmarshal([]byte(actUsage), &r.ActualUsage); err != nil {
		return activity.Record{}, fmt.Errorf("activity/sqlite: actual usage: %w", err)
	}
	if err := json.Unmarshal([]byte(actCost), &r.ActualCost); err != nil {
		return activity.Record{}, fmt.Errorf("activity/sqlite: actual cost: %w", err)
	}
	r.EnforcementMode = engine.Mode(mode)
	r.Status = activity.Status(status)
	r.Outcome = activity.Outcome(outcome)
	r.Error = errText
	r.StartedAt = fromUnix(startedAt)
	r.CompletedAt = fromUnix(completedAt)
	r.Latency = time.Duration(latency)
	r.ProviderLatency = time.Duration(providerLatcy)
	r.StreamEstablished = time.Duration(established)
	r.StreamFirstEvent = time.Duration(firstEvent)
	if agent != "" && agent != "null" && agent != "{}" {
		if err := json.Unmarshal([]byte(agent), &r.Agent); err != nil {
			return activity.Record{}, fmt.Errorf("activity/sqlite: agent: %w", err)
		}
	}
	if runtime != "" && runtime != "null" && runtime != "{}" {
		if err := json.Unmarshal([]byte(runtime), &r.Runtime); err != nil {
			return activity.Record{}, fmt.Errorf("activity/sqlite: runtime: %w", err)
		}
	}
	if repairs != "" && repairs != "null" && repairs != "[]" {
		if err := json.Unmarshal([]byte(repairs), &r.Repairs); err != nil {
			return activity.Record{}, fmt.Errorf("activity/sqlite: repairs: %w", err)
		}
	}
	if metadata != "" && metadata != "null" {
		if err := json.Unmarshal([]byte(metadata), &r.Metadata); err != nil {
			return activity.Record{}, fmt.Errorf("activity/sqlite: metadata: %w", err)
		}
	}
	return r, nil
}

// Get implements activity.Store.
func (s *Store) Get(ctx context.Context, requestID string) (activity.Record, error) {
	r, err := scan(s.db.QueryRowContext(ctx, columns+` WHERE request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return activity.Record{}, fmt.Errorf("%w: %q", activity.ErrNotFound, requestID)
	}
	if err != nil {
		return activity.Record{}, err
	}
	scopes, err := s.scopes(ctx, requestID)
	if err != nil {
		return activity.Record{}, err
	}
	r.Scopes = scopes
	return r, nil
}

func (s *Store) scopes(ctx context.Context, requestID string) ([]activity.Scope, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT budget_id, period_id, depth FROM activity_scopes
		  WHERE request_id = ? ORDER BY depth`, requestID)
	if err != nil {
		return nil, fmt.Errorf("activity/sqlite: scopes: %w", err)
	}
	defer rows.Close()
	var out []activity.Scope
	for rows.Next() {
		var sc activity.Scope
		if err := rows.Scan(&sc.BudgetID, &sc.PeriodID, &sc.Depth); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// List implements activity.Store.
//
// Budget matching goes through activity_scopes, so a parent sees spend that its
// descendants incurred: a request against a child really did consume the parent's
// headroom, and a report that showed otherwise would be wrong.
func (s *Store) List(ctx context.Context, f activity.Filter) ([]activity.Record, error) {
	var (
		where []string
		args  []any
	)
	if f.BudgetID != "" {
		where = append(where, `(budget_id = ? OR request_id IN
			(SELECT request_id FROM activity_scopes WHERE budget_id = ?))`)
		args = append(args, f.BudgetID, f.BudgetID)
	}
	if f.PeriodID != "" {
		where = append(where, `request_id IN
			(SELECT request_id FROM activity_scopes WHERE period_id = ?)`)
		args = append(args, f.PeriodID)
	}
	if !f.From.IsZero() {
		where = append(where, `started_at >= ?`)
		args = append(args, toUnix(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, `started_at < ?`)
		args = append(args, toUnix(f.To))
	}
	if f.UnresolvedOnly {
		where = append(where, `cost_completeness <> 'known'`)
	}
	if len(f.Statuses) > 0 {
		marks := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			marks[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, `status IN (`+strings.Join(marks, ",")+`)`)
	}

	q := columns
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY started_at DESC, request_id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("activity/sqlite: list: %w", err)
	}
	defer rows.Close()

	var out []activity.Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		scopes, err := s.scopes(ctx, out[i].RequestID)
		if err != nil {
			return nil, err
		}
		out[i].Scopes = scopes
	}
	return out, nil
}
