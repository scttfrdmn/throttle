package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"throttle/budget"
)

// The file schema. Small, explicit, and every field maps onto something the engine already
// has: nothing here is a new budgeting concept, and the loader's whole job is to compile a
// readable file down to budget.Definition values the engine already knows how to govern.

// file is the YAML document.
//
// Pointers on the optional scalars so that "present and empty" is distinguishable from
// "absent" -- listen: "" is a mistake worth reporting, while an omitted listen is a request
// for the default.
type file struct {
	Version *int `yaml:"version"`

	Store struct {
		Ledger   string `yaml:"ledger"`
		Activity string `yaml:"activity"`
	} `yaml:"store"`

	Defaults struct {
		Budget      string `yaml:"budget"`
		Enforcement string `yaml:"enforcement"`
		Lease       string `yaml:"lease"`
	} `yaml:"defaults"`

	Dashboard struct {
		Listen        *string `yaml:"listen"`
		ActivityLimit *int    `yaml:"activity_limit"`
	} `yaml:"dashboard"`

	// Budgets is keyed by budget id.
	//
	// A map rather than a list because the ledger already keys budgets by a string id
	// rather than a UUID, so the key a person writes *is* the durable identifier and
	// nobody has to look one up to declare a child. It also makes a duplicate id a YAML
	// parse error rather than something the loader has to detect and explain.
	Budgets map[string]fileBudget `yaml:"budgets"`
}

type fileBudget struct {
	Name   string `yaml:"name"`
	Parent string `yaml:"parent"`
	Amount string `yaml:"amount"`
	Borrow string `yaml:"borrow"`

	Period *filePeriod `yaml:"period"`

	Rollover *fileRollover `yaml:"rollover"`
}

type filePeriod struct {
	Recur    string `yaml:"recur"`
	Every    string `yaml:"every"`
	Timezone string `yaml:"timezone"`
	Anchor   string `yaml:"anchor"`
	End      string `yaml:"end"`
}

type fileRollover struct {
	Mode string `yaml:"mode"`
	Cap  *struct {
		Amount  string `yaml:"amount"`
		Percent string `yaml:"percent"`
	} `yaml:"cap"`
}

// ErrNotExist reports that a named config file is not there.
var ErrNotExist = errors.New("config: file does not exist")

// LoadFile reads and validates a configuration file.
//
// It returns as many problems as it can find in one pass. Configuration is edited by hand,
// and a loader that stops at the first mistake turns five typos into five edit-run cycles.
//
// Nothing is opened but the file: no database, no period materialized, no provider call.
func LoadFile(path string, env Env) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%w: %s", ErrNotExist, path)
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	cfg, err := decode(f, path, env)
	if err != nil {
		return Config{}, err
	}
	cfg.Path = path
	cfg.note("config", FromFlag)
	return cfg, nil
}

// decode parses a document and produces a Config, aggregating problems.
func decode(r io.Reader, source string, env Env) (Config, error) {
	cfg, err := Defaults(env)
	if err != nil {
		return Config{}, err
	}

	var doc file
	dec := yaml.NewDecoder(r)

	// An unknown field is an error, not a shrug. A silently ignored "alloction:" is a
	// budget quietly running on the default allocation, and the config check that exists
	// to catch exactly that would report the file as clean.
	dec.KnownFields(true)

	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", source, yamlProblem(err))
	}

	problems := &Errors{Source: source}

	if doc.Version != nil && *doc.Version != SchemaVersion {
		problems.add(fieldErr("version", fmt.Sprintf(
			"unknown schema version %d: this build understands version %d",
			*doc.Version, SchemaVersion)))
	}

	// Paths first: they are independent of everything else, so a bad ledger path does
	// not suppress a budget's problems or the reverse.
	if doc.Store.Ledger != "" {
		if p, err := expandPath(doc.Store.Ledger, env); err != nil {
			problems.add(fieldErr("store.ledger", err.Error()))
		} else {
			cfg.Ledger = p
			cfg.note("store.ledger", FromFile)
		}
	}
	if doc.Store.Activity != "" {
		if p, err := expandPath(doc.Store.Activity, env); err != nil {
			problems.add(fieldErr("store.activity", err.Error()))
		} else {
			cfg.Activity = p
			cfg.note("store.activity", FromFile)
		}
	}

	if doc.Defaults.Budget != "" {
		cfg.DefaultBudget = doc.Defaults.Budget
		cfg.note("defaults.budget", FromFile)
	}
	if doc.Defaults.Enforcement != "" {
		mode, err := parseMode("defaults.enforcement", doc.Defaults.Enforcement)
		if err != nil {
			problems.add(err)
		} else {
			cfg.Enforcement = mode
			cfg.note("defaults.enforcement", FromFile)
		}
	}
	if doc.Defaults.Lease != "" {
		d, err := parseDuration("defaults.lease", doc.Defaults.Lease)
		switch {
		case err != nil:
			problems.add(err)
		case d == 0:
			problems.add(fieldErr("defaults.lease",
				"a lease of zero would let recovery reclaim a hold the instant it was taken"))
		default:
			cfg.Lease = d
			cfg.note("defaults.lease", FromFile)
		}
	}

	if doc.Dashboard.Listen != nil {
		if *doc.Dashboard.Listen == "" {
			problems.add(fieldErr("dashboard.listen",
				"an empty listen address means every interface; omit the field for the loopback default"))
		} else {
			cfg.Listen = *doc.Dashboard.Listen
			cfg.note("dashboard.listen", FromFile)
		}
	}
	if doc.Dashboard.ActivityLimit != nil {
		if *doc.Dashboard.ActivityLimit < 0 {
			problems.add(fieldErr("dashboard.activity_limit", "cannot be negative"))
		} else {
			cfg.ActivityLimit = *doc.Dashboard.ActivityLimit
			cfg.note("dashboard.activity_limit", FromFile)
		}
	}

	defs, budgetErrs := compileBudgets(doc.Budgets)
	for _, err := range budgetErrs {
		problems.add(err)
	}
	cfg.Budgets = defs
	for id := range doc.Budgets {
		cfg.note("budgets."+id, FromFile)
	}

	if cfg.DefaultBudget != "" && len(budgetErrs) == 0 && len(doc.Budgets) > 0 {
		if _, ok := cfg.Definition(cfg.DefaultBudget); !ok {
			problems.add(fieldErr("defaults.budget", fmt.Sprintf(
				"names %q, but no budget with that name is defined in this file. Defined here: %s",
				cfg.DefaultBudget, strings.Join(sortedIDs(doc.Budgets), ", "))))
		}
	}

	if problems.len() > 0 {
		return Config{}, problems
	}
	return cfg, nil
}

// compiled is one budget mid-compilation.
//
// The set flags exist because inheritance needs to know the difference between a field a
// child omitted and a field it set to the zero value: an omitted timezone should come from
// the parent, whereas timezone: UTC is a choice.
type compiled struct {
	def budget.Definition

	setRecur  bool
	setEvery  bool
	setTZ     bool
	setAnchor bool
	setEnd    bool
}

// compileBudgets turns the file's budget entries into definitions, in three phases.
//
// Each phase only aggregates errors its predecessors have made safe. Every entry is
// compiled independently first, so a typo in one budget does not hide a typo in another;
// references are resolved once each entry individually exists, because "parent research
// does not exist" is a misleading thing to say about a file where research failed to parse;
// and validation runs last, because a child's fields are not complete until it has
// inherited from its parent.
func compileBudgets(entries map[string]fileBudget) ([]budget.Definition, []error) {
	if len(entries) == 0 {
		return nil, nil
	}

	// Sorted so that a run over the same file produces the same order and the same error
	// sequence. Declaration order is not recoverable from a YAML map, and the order that
	// matters -- parents before children -- is imposed below.
	ids := sortedIDs(entries)

	var (
		items []*compiled
		errs  []error
	)
	for _, id := range ids {
		item, itemErrs := compileBudget(id, entries[id])
		if len(itemErrs) > 0 {
			errs = append(errs, itemErrs...)
			continue
		}
		items = append(items, item)
	}
	if len(errs) > 0 {
		return nil, errs
	}

	byID := make(map[string]*compiled, len(items))
	for _, item := range items {
		byID[item.def.ID] = item
	}

	if errs := resolveReferences(items, byID, ids); len(errs) > 0 {
		return nil, errs
	}

	// Parents first, so inheritance sees a completed parent and so a caller can persist
	// the slice in order: PutDefinition requires the parent row to exist already.
	sortParentsFirst(items, byID)

	defs := make([]budget.Definition, 0, len(items))
	for _, item := range items {
		if parent, ok := byID[item.def.ParentID]; ok {
			item.inherit(parent)
		}
		if err := validate(item); err != nil {
			errs = append(errs, err...)
			continue
		}
		defs = append(defs, item.def)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return defs, nil
}

// compileBudget turns one entry into a definition, reporting every field problem it finds.
func compileBudget(id string, b fileBudget) (*compiled, []error) {
	base := "budgets." + id
	var errs []error

	if strings.TrimSpace(id) == "" {
		return nil, []error{fieldErr("budgets", "a budget id cannot be empty")}
	}

	item := &compiled{def: budget.Definition{ID: id, ParentID: b.Parent, Name: b.Name}}

	amount, err := parseMoney(base+".amount", b.Amount)
	if err != nil {
		errs = append(errs, err)
	}
	item.def.Allocation = amount

	if b.Borrow != "" {
		d, err := parseDuration(base+".borrow", b.Borrow)
		if err != nil {
			errs = append(errs, err)
		}
		item.def.Borrow = d
	}

	errs = append(errs, compilePeriod(base, b.Period, item)...)
	errs = append(errs, compileRollover(base, b.Rollover, item)...)

	if len(errs) > 0 {
		return nil, errs
	}
	return item, nil
}

// compilePeriod fills in the recurrence, timezone, and bounds.
//
// "monthly" is convenient syntax, not the engine's abstraction: it compiles to the same
// Recurrence/Every/Location/AnchorAt/EndAt that a fixed grant or a six-hour experiment
// window compiles to, and there is no month-shaped special case below.
func compilePeriod(base string, p *filePeriod, item *compiled) []error {
	// The timezone is resolved before the dates, because a bare date means midnight in
	// the budget's own zone: parsing it in UTC would shift every period boundary by the
	// offset, and a monthly budget in America/New_York would roll over at 20:00 on the
	// last day of the month.
	item.def.Location = time.UTC

	if p == nil {
		return nil
	}

	var errs []error

	if p.Timezone != "" {
		loc, err := parseLocation(base+".period.timezone", p.Timezone)
		if err != nil {
			errs = append(errs, err)
		} else {
			item.def.Location = loc
			item.setTZ = true
		}
	}
	loc := item.def.Location

	if p.Recur != "" {
		recur := budget.Recurrence(p.Recur)
		switch recur {
		case budget.RecurNone, budget.RecurDaily, budget.RecurWeekly, budget.RecurMonthly, budget.RecurDuration:
			item.def.Recurrence = recur
			item.setRecur = true
		default:
			errs = append(errs, fieldErr(base+".period.recur", fmt.Sprintf(
				"unknown recurrence %q: use monthly, weekly, daily, duration, or none", p.Recur)))
		}
	}

	if p.Every != "" {
		d, err := parseDuration(base+".period.every", p.Every)
		switch {
		case err != nil:
			errs = append(errs, err)
		case d <= 0:
			errs = append(errs, fieldErr(base+".period.every", "must be positive"))
		default:
			item.def.Every = d
			item.setEvery = true
		}
	}

	if p.Anchor != "" {
		t, err := parseDate(base+".period.anchor", p.Anchor, loc, false)
		if err != nil {
			errs = append(errs, err)
		} else {
			item.def.AnchorAt = t
			item.setAnchor = true
		}
	}
	if p.End != "" {
		// endOfDay: a grant "through 2028-08-31" includes the 31st. Reading a bare date
		// as the start of that day would expire the budget a day early, and nothing
		// about the result would look wrong.
		t, err := parseDate(base+".period.end", p.End, loc, true)
		if err != nil {
			errs = append(errs, err)
		} else {
			item.def.EndAt = t
			item.setEnd = true
		}
	}
	return errs
}

// compileRollover fills in the carry policy.
func compileRollover(base string, r *fileRollover, item *compiled) []error {
	if r == nil {
		return nil
	}

	var errs []error

	mode, err := parseRolloverMode(base+".rollover.mode", r.Mode)
	if err != nil {
		errs = append(errs, err)
	}
	policy := budget.RolloverPolicy{Mode: mode}

	if cap := r.Cap; cap != nil {
		hasAmount := strings.TrimSpace(cap.Amount) != ""
		hasPercent := strings.TrimSpace(cap.Percent) != ""

		switch {
		case hasAmount && hasPercent:
			// Refused before anything durable is written. A cap is either a fixed sum or
			// a proportion of the allocation, and accepting both would leave which one
			// wins as an implicit product rule nobody chose.
			errs = append(errs, fieldErr(base+".rollover.cap",
				"amount and percent are mutually exclusive: a cap is either a fixed sum or a "+
					"proportion of the allocation, not both"))
		case hasAmount:
			m, err := parseMoney(base+".rollover.cap.amount", cap.Amount)
			if err != nil {
				errs = append(errs, err)
			}
			policy.Cap = m
		case hasPercent:
			bp, err := parsePercent(base+".rollover.cap.percent", cap.Percent)
			if err != nil {
				errs = append(errs, err)
			}
			policy.CapBasisPoints = bp
		default:
			errs = append(errs, fieldErr(base+".rollover.cap",
				"needs either amount or percent; omit the cap entirely for uncapped carry"))
		}

		if mode == budget.RolloverNone && (hasAmount || hasPercent) {
			// A cap under mode: none is inert, and inert configuration is a written
			// belief about behavior that is not happening.
			errs = append(errs, fieldErr(base+".rollover.cap",
				"has no effect with mode: none, which carries nothing forward to cap"))
		}
	}

	item.def.Rollover = policy
	return errs
}

// inherit fills a child's unset period fields from its parent.
//
// This is what lets a sub-budget be written as "parent: research, amount: $1000" -- which
// is how anyone would expect to write it, and how the ledger already treats it: a child's
// spend consumes the parent's headroom, so sharing the parent's calendar is the reading
// that makes the two sets of numbers comparable. A child that wants a different period
// says so and gets it.
//
// Only the period is inherited. Allocation is not: a child whose amount was silently the
// parent's would double the apparent commitment.
func (c *compiled) inherit(parent *compiled) {
	if !c.setTZ {
		c.def.Location = parent.def.Location
	}
	if !c.setRecur {
		c.def.Recurrence = parent.def.Recurrence
	}
	if !c.setEvery {
		c.def.Every = parent.def.Every
	}
	if !c.setAnchor {
		c.def.AnchorAt = parent.def.AnchorAt
	}
	if !c.setEnd {
		c.def.EndAt = parent.def.EndAt
	}
}

// validate finishes a definition and checks it, reporting problems against field paths.
func validate(item *compiled) []error {
	base := "budgets." + item.def.ID
	def := &item.def
	var errs []error

	if def.Recurrence == "" {
		// Monthly is the overwhelmingly common case and is what "a budget" means to most
		// people. Not a special case in the engine -- just the default value of one
		// field, which is why an arbitrary grant period costs no more to express.
		def.Recurrence = budget.RecurMonthly
	}

	switch def.Recurrence {
	case budget.RecurDuration:
		if def.Every <= 0 {
			errs = append(errs, fieldErr(base+".period.every",
				"required with recur: duration, e.g. every: 6h"))
		}
	case budget.RecurNone:
		if def.EndAt.IsZero() {
			errs = append(errs, fieldErr(base+".period.end",
				"required with recur: none, which is one fixed term rather than a repeating period"))
		}
	default:
		if def.Every != 0 {
			errs = append(errs, fieldErr(base+".period.every", fmt.Sprintf(
				"only applies with recur: duration; %s already fixes the period length",
				def.Recurrence)))
		}
	}

	if def.AnchorAt.IsZero() {
		// Deliberately required rather than defaulted to the current month.
		//
		// A definition's fingerprint covers its anchor, so a config file whose anchor
		// came from the clock would describe a different budget in September than in
		// October -- the same file, read twice, disagreeing with the ledger for no
		// reason the reader could see. A child inherits its parent's anchor, so only
		// root budgets have to say this.
		hint := "the first day the budget applies, e.g. anchor: 2026-09-01"
		if def.ParentID != "" {
			hint += " (a child normally inherits its parent's, so this means the parent has none either)"
		}
		errs = append(errs, fieldErr(base+".period.anchor", "required: "+hint))
	}

	if len(errs) > 0 {
		return errs
	}

	// The engine's own validation is the last word, so no config file can produce a
	// definition the engine would reject. Its message speaks in terms of a budget rather
	// than a file, so the path is prefixed.
	if err := def.Validate(); err != nil {
		return []error{fieldErr(base, err.Error())}
	}
	return nil
}

// resolveReferences checks the hierarchy across the whole set.
func resolveReferences(items []*compiled, byID map[string]*compiled, ids []string) []error {
	var errs []error
	for _, item := range items {
		if item.def.ParentID == "" {
			continue
		}
		if _, ok := byID[item.def.ParentID]; !ok {
			// The likely cause is a typo, so the message lists what is available rather
			// than only stating what is missing.
			errs = append(errs, fieldErr("budgets."+item.def.ID+".parent", fmt.Sprintf(
				"names %q, but no budget with that name exists in this file. Defined here: %s",
				item.def.ParentID, strings.Join(ids, ", "))))
		}
	}
	if len(errs) > 0 {
		return errs
	}

	// Cycles. The ledger detects these too, but a config file is where one actually gets
	// written, and catching it here is both faster and the only way "config check" can be
	// honestly read-only about it.
	for _, item := range items {
		seen := map[string]bool{item.def.ID: true}
		cur := item.def.ParentID
		for cur != "" {
			if seen[cur] {
				errs = append(errs, fieldErr("budgets."+item.def.ID+".parent", fmt.Sprintf(
					"is part of a parent cycle: %s", cycleText(byID, item.def.ID))))
				break
			}
			seen[cur] = true
			next, ok := byID[cur]
			if !ok {
				break
			}
			cur = next.def.ParentID
		}
	}
	return errs
}

// cycleText renders a parent cycle as a path, because "there is a cycle" does not tell the
// reader which line to delete.
func cycleText(byID map[string]*compiled, start string) string {
	var path []string
	seen := map[string]bool{}
	cur := start
	for cur != "" && !seen[cur] {
		seen[cur] = true
		path = append(path, cur)
		next, ok := byID[cur]
		if !ok {
			break
		}
		cur = next.def.ParentID
	}
	if cur != "" {
		path = append(path, cur)
	}
	return strings.Join(path, " -> ")
}

func sortedIDs(entries map[string]fileBudget) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortParentsFirst reorders items so every parent precedes its children.
//
// Called only after resolveReferences has ruled out missing parents and cycles, so the
// depth walk terminates.
func sortParentsFirst(items []*compiled, byID map[string]*compiled) {
	depth := make(map[string]int, len(items))
	var depthOf func(string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		item, ok := byID[id]
		if !ok || item.def.ParentID == "" {
			depth[id] = 0
			return 0
		}
		// Marked before recursing so a cycle that somehow reached here terminates at
		// depth 0 rather than overflowing the stack.
		depth[id] = 0
		d := depthOf(item.def.ParentID) + 1
		depth[id] = d
		return d
	}
	for _, item := range items {
		depthOf(item.def.ID)
	}
	sort.SliceStable(items, func(i, j int) bool {
		di, dj := depth[items[i].def.ID], depth[items[j].def.ID]
		if di != dj {
			return di < dj
		}
		return items[i].def.ID < items[j].def.ID
	})
}

// yamlProblem trims the noise off a yaml decode error.
//
// The library's "line 7: field alloction not found in type config.fileBudget" names a Go
// type the reader has never heard of. The line number is the useful half.
func yamlProblem(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: ")
	msg = strings.ReplaceAll(msg, "unmarshal errors:\n", "")
	msg = strings.TrimSpace(msg)

	var out []string
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.Index(line, " not found in type "); i >= 0 {
			// "line 7: field alloction not found in type X" becomes a sentence about the
			// file, and says why it is an error rather than a warning.
			out = append(out, strings.Replace(line[:i], "field ", "unknown field ", 1)+
				" (an unrecognized field would otherwise be silently ignored, leaving the "+
				"setting it was meant to change at its default)")
			continue
		}
		out = append(out, line)
	}
	return errors.New(strings.Join(out, "; "))
}
