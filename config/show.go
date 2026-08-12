package config

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Rendering the effective configuration.
//
// # What this never prints
//
// Credentials, of any kind, from anywhere. Not AWS access keys, not session tokens, not
// provider API keys, not the contents of any environment variable that might hold one.
//
// This is structural rather than a filter: the Config type has no field for a credential,
// so there is nothing here to redact. AWS configuration is resolved by the AWS SDK from its
// own environment, profile, and role mechanisms, and throttle neither reads nor stores it.
// Where that external resolution is worth mentioning, it is described generically -- which
// mechanism will be consulted -- without resolving it. Naming a profile is a configuration
// fact; printing what the profile contains is a credential leak, and a "throttle config
// show" pasted into an issue is exactly how that happens.

// Show writes the effective configuration, with each value's origin.
//
// The origins are the point. "Why is it using that ledger?" is otherwise a question that
// gets answered by reading source, and the answer is usually an environment variable
// somebody set months ago.
func (c Config) Show(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	path := c.Path
	if path == "" {
		path = "(none: running on built-in defaults)"
	}
	fmt.Fprintf(tw, "config file\t%s\n", path)
	fmt.Fprintf(tw, "schema version\t%d\n", SchemaVersion)
	fmt.Fprintln(tw, "\t")

	c.row(tw, "ledger", c.Ledger, "store.ledger")
	c.row(tw, "activity", c.Activity, "store.activity")
	fmt.Fprintln(tw, "\t")

	budgetName := c.DefaultBudget
	if budgetName == "" {
		budgetName = "(none: commands that need a budget must be told which)"
	}
	c.row(tw, "default budget", budgetName, "defaults.budget")
	c.row(tw, "enforcement", string(c.Enforcement), "defaults.enforcement")
	c.row(tw, "reservation lease", c.Lease.String(), "defaults.lease")
	fmt.Fprintln(tw, "\t")

	listen := c.Listen
	if !IsLoopback(c.Listen) {
		// The same fact the serve warning states, visible before anyone runs serve.
		listen += "  (NOT loopback: the dashboard has no authentication)"
	}
	c.row(tw, "dashboard listen", listen, "dashboard.listen")
	c.row(tw, "dashboard rows", fmt.Sprint(c.ActivityLimit), "dashboard.activity_limit")

	if err := tw.Flush(); err != nil {
		return err
	}

	if len(c.Budgets) > 0 {
		fmt.Fprintf(w, "\nbudgets (%d, as the file defines them; the ledger is authoritative "+
			"once they are stored)\n\n", len(c.Budgets))
		if err := showBudgets(w, c.Budgets); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(w, "\nno budgets defined in configuration")
	}

	fmt.Fprint(w, "\n"+providerNote())
	return nil
}

// row prints one value and where it came from.
func (c Config) row(w io.Writer, label, value, path string) {
	src := c.source(path)
	if src == FromDefault {
		// An unadorned line means "nobody chose this", which is the common case and does
		// not need decorating.
		fmt.Fprintf(w, "%s\t%s\n", label, value)
		return
	}
	fmt.Fprintf(w, "%s\t%s\t(%s)\n", label, value, src)
}

// showBudgets prints the normalized definitions.
//
// Normalized, not as written: "monthly" appears as the recurrence it compiled to and the
// anchor as the instant it resolved to, because the question this output answers is what
// throttle will actually do, not what the file says.
func showBudgets(w io.Writer, defs []budget.Definition) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPARENT\tALLOCATION\tPERIOD\tSTARTS\tENDS\tROLLOVER\tBORROW")
	for _, def := range defs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			def.ID, dash(def.ParentID), def.Allocation.CentsString(),
			DescribePeriod(def), def.AnchorAt.In(def.Location).Format("2006-01-02 15:04 MST"),
			describeEnd(def), DescribeRollover(def.Rollover), describeBorrow(def.Borrow))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// DescribePeriod names the period rule in the vocabulary the rest of the CLI uses.
//
// Exported for the same reason as DescribeRollover: "budgets" and "config show" describe the
// same definitions, and a monthly budget that reads as "month" in one listing and "monthly" in
// the other is two vocabularies for one concept.
func DescribePeriod(def budget.Definition) string {
	switch def.Recurrence {
	case budget.RecurDuration:
		return "every " + def.Every.String()
	case budget.RecurNone:
		// Deliberately not called a "period": it does not repeat, and calling a fixed
		// grant monthly-shaped is how a two-year award gets read as a monthly one.
		return "fixed term"
	default:
		// Calendar recurrences carry a timezone because their boundaries fall in it, and
		// a month that ends at the wrong hour is a month that bills the wrong day.
		return string(def.Recurrence) + " (" + locName(def.Location) + ")"
	}
}

func describeEnd(def budget.Definition) string {
	if def.EndAt.IsZero() {
		return "-"
	}
	return def.EndAt.In(def.Location).Format("2006-01-02 15:04 MST")
}

// DescribeRollover renders a carry policy for a table.
//
// Exported because the CLI's "budgets" listing shows the same policy: one renderer means a
// cap cannot read as "credit up to 25%" in one command and "credit ≤25.000000001%" in
// another, which is what a float-formatted percentage eventually does.
func DescribeRollover(p budget.RolloverPolicy) string {
	mode := string(p.Mode.Normalized())
	switch {
	case p.Cap > 0:
		return mode + " up to " + p.Cap.CentsString()
	case p.CapBasisPoints > 0:
		return fmt.Sprintf("%s up to %s%% of allocation", mode, percentText(p.CapBasisPoints))
	default:
		return mode
	}
}

// percentText renders basis points as a decimal percentage without going through a float,
// so a cap of 12.34% prints as "12.34" and not as "12.339999999999999".
func percentText(bp int64) string {
	whole, frac := bp/100, bp%100
	if frac == 0 {
		return fmt.Sprint(whole)
	}
	s := fmt.Sprintf("%d.%02d", whole, frac)
	return strings.TrimSuffix(s, "0")
}

func describeBorrow(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.String()
}

// IsLoopback reports whether a listen address binds loopback only.
//
// A copy of the dashboard's own test rather than a call to it, because config must not
// depend on dashboard: serve reads configuration, not the reverse. The duplication is
// two dozen lines and a test asserts the two agree on every case that matters, which is
// cheaper than the import cycle the other arrangement would need.
//
// An unparseable address and a hostname that is not "localhost" both count as exposed. The
// cost of guessing wrong is publishing spend data, so the guess goes the safe way.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "":
		// In a listen address, no host means every interface.
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// providerNote describes where provider configuration comes from, without resolving it.
//
// It names the mechanisms rather than their contents. Whether AWS_PROFILE is set is a
// configuration fact worth knowing when a call fails; what credentials it resolves to is
// not throttle's to print, and a "config show" is pasted into issues.
func providerNote() string {
	var b strings.Builder
	b.WriteString("providers\n")
	b.WriteString("  AWS (Bedrock) configuration is resolved by the AWS SDK from its own\n")
	b.WriteString("  environment, shared config, and role mechanisms. throttle stores no\n")
	b.WriteString("  credentials and prints none.\n")

	// Presence only, never a value: a region is harmless but a session token is not, and
	// a rule that distinguishes them by name is a rule that eventually gets it wrong.
	var set []string
	for _, key := range []string{"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONFIG_FILE"} {
		if os.Getenv(key) != "" {
			set = append(set, key)
		}
	}
	if len(set) > 0 {
		fmt.Fprintf(&b, "  Set in this environment: %s (values not shown)\n", strings.Join(set, ", "))
	}
	return b.String()
}
