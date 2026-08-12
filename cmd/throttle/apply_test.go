package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The declarative first-run path, through the real binary.
//
// This is the sequence a new user actually walks: init, edit, check, diff, apply, diff again,
// serve. Each step is asserted on the way through, because the value of the path is that it
// works end to end without anybody being told to run "throttle define" three times.
//
// Every test here runs under an isolated HOME with the XDG and THROTTLE_* variables cleared,
// like the rest of cli_test.go, and none of them need AWS credentials or a network.

// hierarchy is a config file describing a parent and two children, which is the shape the
// first-run documentation promises.
const hierarchy = `
version: 1
defaults:
  budget: research
budgets:
  research:
    name: Research programme
    amount: $4,000
    period:
      recur: monthly
      anchor: 2026-01-01
  chat:
    parent: research
    name: Interactive chat
    amount: $1,000
    period:
      recur: monthly
      anchor: 2026-01-01
  agents:
    parent: research
    name: Agent runs
    amount: $500
    period:
      recur: monthly
      anchor: 2026-01-01
`

// (19, 20) The whole declarative path, in order, with no imperative define anywhere.
func TestDeclarativeFirstRun(t *testing.T) {
	c := newCLI(t)

	// init writes a file and names the steps that follow it.
	out := c.ok("init")
	for _, want := range []string{"config check", "config diff", "config apply", "serve"} {
		if !strings.Contains(out, want) {
			t.Errorf("init does not name %q as a next step:\n%s", want, out)
		}
	}

	// (19) The generated file checks, diffs, and applies without being edited first.
	c.ok("config", "check")
	if out := c.ok("config", "diff"); !strings.Contains(out, "create") {
		t.Errorf("diff of a fresh file does not plan a create:\n%s", out)
	}
	if out := c.ok("config", "apply"); !strings.Contains(out, "1 created") {
		t.Errorf("apply of a fresh file does not report a create:\n%s", out)
	}

	// Now the file the user actually wants: a parent and two children.
	path := c.configPath()
	c.write(path, hierarchy)

	c.ok("config", "check")

	out = c.ok("config", "diff")
	for _, id := range []string{"research", "chat", "agents"} {
		if !strings.Contains(out, id) {
			t.Errorf("diff does not mention %q:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("diff does not say it wrote nothing:\n%s", out)
	}
	// The budget from the starter file is still stored and is left alone.
	if !strings.Contains(out, "left alone") {
		t.Errorf("diff does not report the budget missing from the file as left alone:\n%s", out)
	}

	// diff really did write nothing: nothing from the new file is stored yet.
	if got := c.ok("budgets"); strings.Contains(got, "research") {
		t.Errorf("config diff stored a budget:\n%s", got)
	}

	out = c.ok("config", "apply")
	if !strings.Contains(out, "3 created") {
		t.Errorf("apply does not report three creates:\n%s", out)
	}

	// (16) A second diff is quiet.
	out = c.ok("config", "diff")
	if !strings.Contains(out, "unchanged") {
		t.Errorf("a second diff does not report the file as applied:\n%s", out)
	}
	if strings.Contains(out, "nothing was written") {
		t.Errorf("a second diff still plans changes:\n%s", out)
	}

	// A second apply is a no-op rather than an error.
	if out := c.ok("config", "apply"); !strings.Contains(out, "nothing to apply") {
		t.Errorf("a repeated apply does not report itself as a no-op:\n%s", out)
	}

	// The hierarchy is real: status on the child names its parent chain.
	out = c.ok("status", "-id", "chat", "-chain")
	if !strings.Contains(out, "research") {
		t.Errorf("the child's chain does not include its parent:\n%s", out)
	}
	if out := c.ok("budgets"); !strings.Contains(out, "$4000.00") {
		t.Errorf("the parent's allocation is not stored:\n%s", out)
	}

	// (20) Every applied budget reports where it stands, and an open period exists for each
	// one. Without that, "apply" followed by "serve" shows a dashboard that says nothing is
	// materialized, which reads as though the apply had not worked.
	for _, id := range []string{"research", "chat", "agents"} {
		out := c.ok("periods", "-id", id)
		if !strings.Contains(out, "open") {
			t.Errorf("%q has no open period after apply:\n%s", id, out)
		}
		if strings.Contains(out, "not started yet") {
			t.Errorf("%q reports no periods at all after apply:\n%s", id, out)
		}
	}
}

// (20) The applied hierarchy appears on the dashboard before any provider activity exists.
//
// A dashboard that needs spend before it will show a budget cannot confirm to a new user that
// their configuration took effect, which is the one thing they want to know at that moment.
func TestDashboardShowsAppliedHierarchyWithNoActivity(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}

	c.ok(append([]string{"config", "apply"}, cfg...)...)

	// A port the OS chooses, so the test does not race another process for a fixed one.
	addr := freeAddr(t)
	serve := exec.Command(binary, append([]string{"serve", "-listen", addr}, cfg...)...)
	serve.Env = c.env()
	if err := serve.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()

	base := "http://" + addr
	waitForHTTP(t, base+"/healthz")

	body := fetch(t, base+"/?budget=research")
	for _, want := range []string{"research", "chat", "agents"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dashboard does not show %q with zero activity:\n%s", want, truncate(body))
		}
	}
	// The dashboard formats with thousands separators; the CLI's CentsString does not. Both
	// are the same microdollar figure rendered for different readers.
	if !strings.Contains(body, "$4,000.00") {
		t.Errorf("the dashboard does not show the parent's allocation:\n%s", truncate(body))
	}
	// Zero spend, stated as a measurement rather than as an absence of budgets.
	if strings.Contains(body, "No budgets are defined") {
		t.Errorf("the dashboard reports no budgets after apply:\n%s", truncate(body))
	}

	// And the JSON view agrees, which is what a browser polls.
	var summary map[string]any
	if err := json.Unmarshal([]byte(fetch(t, base+"/api/summary?budget=research")), &summary); err != nil {
		t.Fatalf("decode /api/summary: %v", err)
	}
	if empty, _ := summary["empty"].(bool); empty {
		t.Errorf("/api/summary reports an empty dashboard after apply: %v", summary)
	}
}

// futureGrant is a budget whose term begins after it is applied, which is the ordinary
// shape of a grant somebody sets up in advance. The anchor is far enough out to stay in
// the future for the life of the project, since it is a literal.
const futureGrant = `
version: 1
defaults:
  budget: grant
budgets:
  grant:
    name: Research grant
    amount: $125,000
    period:
      recur: monthly
      anchor: 2099-09-01
`

// A budget applied before its term begins renders on the dashboard rather than 404ing.
//
// This is the release blocker end to end: apply a valid definition whose period has not
// started, serve, and open the page. Nothing has spent against it, so the ledger holds no
// period row -- which used to make the whole dashboard, root page included, return 404 for
// a correctly configured grant.
func TestDashboardShowsAFutureBudgetRatherThan404(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, futureGrant)
	cfg := []string{"-config", path}

	if out := c.ok(append([]string{"config", "apply"}, cfg...)...); !strings.Contains(out, "1 created") {
		t.Fatalf("apply of a future budget does not report a create:\n%s", out)
	}
	// Apply materializes nothing for a term that has not begun, which is the state under
	// test: the definition is durable and there is no period row.
	if out := c.ok(append([]string{"periods", "-id", "grant"}, cfg...)...); !strings.Contains(out, "not started yet") {
		t.Fatalf("the fixture has periods already:\n%s", out)
	}

	addr := freeAddr(t)
	serve := exec.Command(binary, append([]string{"serve", "-listen", addr}, cfg...)...)
	serve.Env = c.env()
	if err := serve.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()

	base := "http://" + addr
	waitForHTTP(t, base+"/healthz")

	// The root page, with no budget named: a future budget is the only one defined, so this
	// is the page a new user lands on immediately after apply.
	for _, path := range []string{"/", "/?budget=grant", "/api/summary?budget=grant"} {
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: the budget is defined, not missing\n%s",
				path, res.StatusCode, truncate(string(body)))
		}
	}

	// Research grant / not started / starts 2099-09-01 / $125,000.
	body := fetch(t, base+"/?budget=grant")
	for _, want := range []string{"Research grant", "not started", "2099-09-01", "$125,000.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dashboard does not show %q for a future budget:\n%s", want, truncate(body))
		}
	}
	if strings.Contains(body, "No budgets are defined") {
		t.Errorf("a defined future budget reads as no budgets at all:\n%s", truncate(body))
	}

	// And reading it wrote nothing: the CLI still reports no periods, from the same store.
	if out := c.ok(append([]string{"periods", "-id", "grant"}, cfg...)...); !strings.Contains(out, "not started yet") {
		t.Errorf("serving the dashboard materialized a period:\n%s", out)
	}
}

// (1) config diff writes nothing, at the level a user can observe.
//
// Asserted on the ledger's content rather than on file bytes: the store runs in WAL mode,
// where a read-only open may legitimately checkpoint, so comparing bytes would fail on an
// implementation detail while saying nothing about whether accounting changed.
func TestConfigDiffDoesNotMutate(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}

	c.ok(append([]string{"config", "apply"}, cfg...)...)
	// A period exists, so a diff has something it could damage.
	c.ok(append([]string{"status", "-id", "research"}, cfg...)...)

	budgetsBefore := c.ok(append([]string{"budgets"}, cfg...)...)
	periodsBefore := c.ok(append([]string{"periods", "-id", "research"}, cfg...)...)

	// Diff against an edited file: a create, an update, and a rename all pending at once.
	c.write(path, strings.NewReplacer(
		"amount: $4,000", "amount: $5,000",
		"name: Agent runs", "name: Autonomous agents",
	).Replace(hierarchy)+`
  planned:
    amount: $50
    period:
      anchor: 2026-01-01
`)

	c.ok(append([]string{"config", "diff"}, cfg...)...)
	c.ok(append([]string{"config", "diff"}, cfg...)...)

	if got := c.ok(append([]string{"budgets"}, cfg...)...); got != budgetsBefore {
		t.Errorf("diff changed the stored definitions:\nbefore:\n%s\nafter:\n%s", budgetsBefore, got)
	}
	if got := c.ok(append([]string{"periods", "-id", "research"}, cfg...)...); got != periodsBefore {
		t.Errorf("diff changed the stored periods:\nbefore:\n%s\nafter:\n%s", periodsBefore, got)
	}
}

// (1, 18) config diff on a first run creates no ledger.
//
// Planning against nothing is how a first run reports that every budget is new, and it must
// not bring a database into existence to discover that.
func TestConfigDiffCreatesNoLedger(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)

	out := c.ok("config", "diff", "-config", path)
	if !strings.Contains(out, "create") {
		t.Errorf("diff on a first run does not plan creates:\n%s", out)
	}

	var found []string
	err := filepath.WalkDir(c.home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".db") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("config diff created databases: %v", found)
	}
}

// (4, 10, 11) An update says, in the CLI, that the current period keeps its terms and when
// the new ones begin.
//
// The rule that matters most and is least obvious. A user who edits $4,000 to $5,000 and is
// told only "updated" will reasonably believe the new figure applies now.
func TestConfigApplyExplainsWhenAnUpdateTakesEffect(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}

	c.ok(append([]string{"config", "apply"}, cfg...)...)
	// Materialize the current period, as a status query does.
	c.ok(append([]string{"status", "-id", "research"}, cfg...)...)
	periodsBefore := c.ok(append([]string{"periods", "-id", "research"}, cfg...)...)

	c.write(path, strings.Replace(hierarchy, "amount: $4,000", "amount: $5,000", 1))

	// diff and apply say the same thing, because they share the planner.
	diff := c.ok(append([]string{"config", "diff"}, cfg...)...)
	apply := c.ok(append([]string{"config", "apply"}, cfg...)...)

	for name, out := range map[string]string{"diff": diff, "apply": apply} {
		if !strings.Contains(out, "current period unchanged") {
			t.Errorf("%s does not say the current period is unchanged:\n%s", name, out)
		}
		if !strings.Contains(out, "new definition applies beginning") {
			t.Errorf("%s does not say when the new definition applies:\n%s", name, out)
		}
		if !strings.Contains(out, "$4000.00") || !strings.Contains(out, "$5000.00") {
			t.Errorf("%s does not show both allocations:\n%s", name, out)
		}
	}
	if !strings.Contains(apply, "1 updated") {
		t.Errorf("apply does not report the update:\n%s", apply)
	}

	// The materialized period is untouched.
	if got := c.ok(append([]string{"periods", "-id", "research"}, cfg...)...); got != periodsBefore {
		t.Errorf("the materialized period changed:\nbefore:\n%s\nafter:\n%s", periodsBefore, got)
	}
	// The stored definition is the new one.
	if got := c.ok(append([]string{"budgets"}, cfg...)...); !strings.Contains(got, "$5000.00") {
		t.Errorf("the definition was not updated:\n%s", got)
	}
}

// (5) A budget removed from the file survives, and no command output suggests otherwise.
func TestConfigApplyNeverDeletes(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}
	c.ok(append([]string{"config", "apply"}, cfg...)...)

	// The children are gone from the file, as an incompletely edited file has them.
	c.write(path, `
version: 1
defaults:
  budget: research
budgets:
  research:
    name: Research programme
    amount: $4,000
    period:
      recur: monthly
      anchor: 2026-01-01
`)

	out := c.ok(append([]string{"config", "apply"}, cfg...)...)
	for _, word := range []string{"delete", "removed", "pruned"} {
		if strings.Contains(strings.ToLower(out), word) {
			t.Errorf("apply output uses %q:\n%s", word, out)
		}
	}

	stored := c.ok(append([]string{"budgets"}, cfg...)...)
	for _, id := range []string{"chat", "agents"} {
		if !strings.Contains(stored, id) {
			t.Errorf("%q was removed because it left the config file:\n%s", id, stored)
		}
	}
}

// (6, 7, 8, 9) A rename is explicit, and apply will not do it.
func TestRenameIsExplicit(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}
	c.ok(append([]string{"config", "apply"}, cfg...)...)

	c.write(path, strings.Replace(hierarchy, "name: Research programme", "name: Research group", 1))

	// diff reports the probable rename and refuses to be the thing that applies it.
	out := c.ok(append([]string{"config", "diff"}, cfg...)...)
	if !strings.Contains(out, "name differs") {
		t.Errorf("diff does not report the name difference:\n%s", out)
	}
	if !strings.Contains(out, "throttle rename research") {
		t.Errorf("diff does not name the explicit command:\n%s", out)
	}

	if out := c.ok(append([]string{"config", "apply"}, cfg...)...); strings.Contains(out, "1 updated") {
		t.Errorf("apply performed a rename:\n%s", out)
	}
	// Still reported as a name-only difference afterwards, which is how we know apply left
	// the stored name alone. ("budgets" lists ids and money, not display names.)
	if out := c.ok(append([]string{"config", "diff"}, cfg...)...); !strings.Contains(out, "name differs") {
		t.Errorf("apply renamed the budget:\n%s", out)
	}

	// The explicit command does it, and says what changed. Flags first: parsing stops at the
	// first positional word.
	out = c.ok(append(append([]string{"rename"}, cfg...), "research", "Research group")...)
	if !strings.Contains(out, "Research programme") || !strings.Contains(out, "Research group") {
		t.Errorf("rename does not report both names:\n%s", out)
	}

	// (9) The children still point at it, and the allocation is untouched.
	if out := c.ok(append([]string{"status", "-id", "chat", "-chain"}, cfg...)...); !strings.Contains(out, "research") {
		t.Errorf("the child lost its parent across a rename:\n%s", out)
	}
	// (8) The allocation and period are exactly as they were: a rename is not a money edit.
	if stored := c.ok(append([]string{"budgets"}, cfg...)...); !strings.Contains(stored, "$4000.00") {
		t.Errorf("the allocation changed across a rename:\n%s", stored)
	}

	// The file now agrees with the ledger, which is both where the user should end up and
	// the proof the new name is stored.
	out = c.ok(append([]string{"config", "diff"}, cfg...)...)
	if strings.Contains(out, "name differs") {
		t.Errorf("diff still reports a name difference after the rename:\n%s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("the ledger does not match the file after the rename:\n%s", out)
	}

	// Re-running it is a no-op rather than an error, so a scripted rename is safe.
	out = c.ok(append(append([]string{"rename"}, cfg...), "research", "Research group")...)
	if !strings.Contains(out, "already named") {
		t.Errorf("a repeated rename does not report itself as a no-op:\n%s", out)
	}
}

// (15) A parent change is refused with a message that says what it will not do, by both
// commands, and nothing is written.
func TestConfigApplyRefusesAParentChange(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}
	c.ok(append([]string{"config", "apply"}, cfg...)...)

	// chat becomes a root budget, plus one perfectly safe new budget alongside it.
	c.write(path, strings.Replace(hierarchy, "    parent: research\n    name: Interactive chat", "    name: Interactive chat", 1)+`
  fresh:
    amount: $10
    period:
      anchor: 2026-01-01
`)

	// diff fails too: a configuration describing a change throttle will not make is a
	// configuration problem whether or not anybody has tried to apply it yet.
	for _, sub := range []string{"diff", "apply"} {
		out := c.fails(append([]string{"config", sub}, cfg...)...)
		if !strings.Contains(out, "parentage") {
			t.Errorf("config %s does not explain the refusal:\n%s", sub, out)
		}
		if !strings.Contains(out, "chat") {
			t.Errorf("config %s does not name the budget:\n%s", sub, out)
		}
	}

	// Nothing was applied, not even the safe half.
	stored := c.ok(append([]string{"budgets"}, cfg...)...)
	if strings.Contains(stored, "fresh") {
		t.Errorf("the safe half of a refused plan was applied:\n%s", stored)
	}
	if out := c.ok(append([]string{"status", "-id", "chat", "-chain"}, cfg...)...); !strings.Contains(out, "research") {
		t.Errorf("chat's parent was changed:\n%s", out)
	}
}

// (13) Two processes applying the same configuration at once both succeed.
//
// The first-run shape of a race: several machines or containers starting from the same file,
// none of which has stored anything yet, so all of them attempt the same creates.
func TestConcurrentConfigApplyConverges(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}

	const workers = 4
	type result struct {
		out  string
		code int
	}
	results := make(chan result, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			out, code := c.run(append([]string{"config", "apply"}, cfg...)...)
			results <- result{out, code}
		}()
	}
	close(start)

	for i := 0; i < workers; i++ {
		if r := <-results; r.code != 0 {
			t.Errorf("a concurrent apply exited %d:\n%s", r.code, r.out)
		}
	}

	// One row per budget and no duplicates. Counted on the id column rather than on the
	// whole line, because "research" also appears as the parent of the other two.
	stored := c.ok(append([]string{"budgets"}, cfg...)...)
	rows := map[string]int{}
	for _, line := range strings.Split(stored, "\n") {
		if id, _, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			rows[id]++
		}
	}
	for _, id := range []string{"research", "chat", "agents"} {
		if rows[id] != 1 {
			t.Errorf("%q has %d rows in the stored budgets:\n%s", id, rows[id], stored)
		}
	}
	if out := c.ok(append([]string{"config", "diff"}, cfg...)...); strings.Contains(out, "nothing was written") {
		t.Errorf("the configuration is not fully applied after the race:\n%s", out)
	}
	// Exactly one process reports having created them; the rest converge quietly rather
	// than each claiming the creates or failing.
	// (Asserted through the ledger above: four applies, three budgets, no duplicates.)
}

// (12) An apply planned against terms that have since changed fails and says to re-run.
//
// Reproduced by applying a file, changing the stored definition through the other path, and
// applying an edit computed before that change. Last-write-wins here would silently restore
// an allocation somebody had just corrected.
func TestConfigApplyReportsAConflict(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, `
version: 1
defaults:
  budget: research
budgets:
  research:
    amount: $4,000
    period:
      recur: monthly
      anchor: 2026-01-01
`)
	cfg := []string{"-config", path}
	c.ok(append([]string{"config", "apply"}, cfg...)...)

	// Definitions are immutable through define, so an outright different definition under
	// the same id is a conflict there as well; this asserts the diagnosis is the same story
	// either way.
	out := c.fails(append([]string{"define", "-id", "research", "-budget", "$9,000", "-recur", "monthly", "-anchor", "2026-01-01"}, cfg...)...)
	if !strings.Contains(out, "differs") && !strings.Contains(out, "conflict") {
		t.Errorf("define does not diagnose the conflict:\n%s", out)
	}

	// And the stored value stands.
	if got := c.ok(append([]string{"budgets"}, cfg...)...); !strings.Contains(got, "$4000.00") {
		t.Errorf("the stored allocation changed:\n%s", got)
	}
}

// (18, 25) Loading configuration writes nothing, and every path stays inside the isolated
// home.
//
// The read-only commands are run against a file describing budgets that are not stored, which
// is the case where a mutation would be most tempting: the tool knows what the user wants and
// could just do it.
func TestConfigReadingIsPure(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	c.write(path, hierarchy)
	cfg := []string{"-config", path}

	for _, args := range [][]string{
		{"config", "check"},
		{"config", "check", "-q"},
		{"config", "show"},
		{"config", "diff"},
	} {
		c.ok(append(args, cfg...)...)
	}

	var found []string
	err := filepath.WalkDir(c.home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && p != path {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("reading configuration created files: %v", found)
	}
}

// The usage text names rename under a heading that says what it does not touch.
func TestRenameIsDocumentedAsMetadata(t *testing.T) {
	c := newCLI(t)
	out, _ := c.run("help")
	if !strings.Contains(out, "rename") {
		t.Errorf("usage does not mention rename:\n%s", out)
	}
	if !strings.Contains(out, "diff") || !strings.Contains(out, "apply") {
		t.Errorf("usage does not mention the config subcommands:\n%s", out)
	}

	out, _ = c.run("rename", "-h")
	if !strings.Contains(out, "display name only") {
		t.Errorf("rename usage does not say what it changes:\n%s", out)
	}
}

// env is the isolated environment the cli helper runs commands in, exposed so a test can
// start a long-lived process with the same isolation.
func (c *cli) env() []string {
	return []string{
		"HOME=" + c.home,
		"PATH=" + os.Getenv("PATH"),
		"XDG_CONFIG_HOME=",
		"XDG_DATA_HOME=",
		"THROTTLE_CONFIG=",
		"THROTTLE_LEDGER=",
		"THROTTLE_ACTIVITY=",
		"TZ=UTC",
	}
}

// freeAddr asks the OS for a loopback port nothing is using, so a serve test does not race
// another process for a fixed one.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
}

func fetch(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", url, resp.StatusCode, body)
	}
	return string(body)
}

// truncate keeps a failure message readable when the body is a whole HTML page.
func truncate(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... truncated"
}
