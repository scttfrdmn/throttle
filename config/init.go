package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Writing a first configuration file.
//
// # What Write will not do
//
// Overwrite. An existing file is an error unless the caller passes force, because a config
// file is hand-edited and the edits are not recoverable: "throttle init" run a second time
// in the wrong directory would otherwise silently discard a month of somebody's budget
// definitions.
//
// It also creates no provider credentials, no AWS resources, and no databases. It writes one
// text file. Creating a store is the business of the command that needs one, and a
// credential is not throttle's to create at all.

// ErrExists reports that a config file is already there.
var ErrExists = errors.New("config: file already exists")

// Template is the commented starter configuration.
//
// One root monthly budget with one child, and nothing else. A starter file that showed every
// option would be a reference document that the reader has to prune before it means
// anything; examples/throttle.yaml is where the fuller shape lives, and the pointer to it is
// two lines from the top.
const Template = `# throttle configuration.
#
# Check it with "throttle config check", and see what is in effect with
# "throttle config show". A fuller annotated example, including sub-budgets,
# rollover caps, and fixed-term grants, is in examples/throttle.yaml.

version: 1

defaults:
  # Which budget commands use when none is named.
  budget: default

  # How a spending process reacts when a request does not fit: monitor records and
  # allows, enforce refuses, wait blocks until the pacing curve affords it.
  enforcement: enforce

# The dashboard has no authentication, and it shows budget names, spend, and
# per-request costs. Loopback is the default for that reason.
dashboard:
  listen: ` + DefaultListen + `

budgets:
  default:
    name: Default budget

    # Dollars. Written as a decimal amount, parsed in integer arithmetic.
    amount: $100

    period:
      # monthly, weekly, daily, duration (with every:), or none (a fixed term
      # with an end:).
      recur: monthly

      # Where calendar boundaries fall. Use an IANA zone name.
      timezone: UTC

      # The first day the budget applies. Required: a budget whose start came
      # from the clock would mean something different every month.
      anchor: %s

    # Uncomment to let a period spend up to three days ahead of its own pacing
    # curve, which an even split across a month otherwise refuses.
    # borrow: 72h

    # Uncomment to carry unspent money into the next period, capped at a quarter
    # of the allocation. The cap is a percentage or an amount, never both.
    # rollover:
    #   mode: credit
    #   cap:
    #     percent: 25
`

// Write creates a starter config file at path.
//
// anchor is the date written into the template, formatted 2006-01-02. It is a parameter
// rather than time.Now() here so the caller owns the clock: a test that cannot predict the
// file it is asserting on is a test that fails on the first of the month.
func Write(path, anchor string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%w: %s\n\nIt is hand-edited and would not be recoverable. "+
			"Pass -force to replace it, or edit it in place", ErrExists, path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
	}

	body := fmt.Sprintf(Template, anchor)

	// 0o600 rather than 0o644. There are no credentials in it and there never will be, but
	// a budget file names what an organization spends and on what, and a default that has
	// to be tightened later is a default that will not be.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}

	// Parsed back immediately. A starter file that does not load under the real loader is a
	// broken first experience, and the template is a string constant that no other test
	// would catch drifting.
	if _, err := LoadFile(path, OSEnv()); err != nil {
		return fmt.Errorf("the file just written does not parse, which is a bug in throttle: %w", err)
	}
	return nil
}

// TemplateAnchor is the anchor date a fresh config should carry: the first of the month
// containing at, in UTC.
//
// The first of the month rather than today, because a budget anchored mid-month has periods
// that run 14th-to-14th, which is not what anyone writing "monthly" means.
func TemplateAnchor(year int, month int) string {
	return fmt.Sprintf("%04d-%02d-01", year, month)
}
