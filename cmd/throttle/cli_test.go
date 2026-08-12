package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end tests over the built binary.
//
// The commands are wired to flag sets, os.Stdout, and os.Exit, so testing them as functions
// would mean either restructuring every command around injected writers or asserting on
// nothing much. Running the real binary tests what a user actually invokes, including the
// exit status, which is what a CI job checking its configuration depends on.
//
// (24) Every test runs in its own temp directory with HOME pointed at it and the XDG and
// THROTTLE_* variables cleared. Nothing here can read or write the real user's config or
// data directories: a test that touches $HOME behaves differently on the author's machine
// than in CI, and one that writes there damages the machine it runs on.

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "throttle-cli-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	binary = filepath.Join(dir, "throttle")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build throttle: " + err.Error() + "\n" + string(out))
	}

	os.Exit(m.Run())
}

// cli is one isolated throttle installation.
type cli struct {
	t    *testing.T
	home string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	return &cli{t: t, home: t.TempDir()}
}

// run invokes throttle and returns its combined output and exit status.
func (c *cli) run(args ...string) (string, int) {
	c.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{
		"HOME=" + c.home,
		"PATH=" + os.Getenv("PATH"),
		// Explicitly emptied rather than merely absent from this list: an inherited
		// XDG_DATA_HOME would send a ledger outside the temp directory, and an inherited
		// THROTTLE_LEDGER would silently override the config file under test.
		"XDG_CONFIG_HOME=",
		"XDG_DATA_HOME=",
		"THROTTLE_CONFIG=",
		"THROTTLE_LEDGER=",
		"THROTTLE_ACTIVITY=",
		// TZ fixed so a machine in another zone reads the same bare dates.
		"TZ=UTC",
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		c.t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

// ok runs a command that must succeed.
func (c *cli) ok(args ...string) string {
	c.t.Helper()
	out, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("throttle %s exited %d:\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// fails runs a command that must not succeed.
func (c *cli) fails(args ...string) string {
	c.t.Helper()
	out, code := c.run(args...)
	if code == 0 {
		c.t.Fatalf("throttle %s succeeded, want a nonzero exit:\n%s", strings.Join(args, " "), out)
	}
	return out
}

// configPath is where init writes, on this platform, under the isolated home.
func (c *cli) configPath() string {
	c.t.Helper()
	// Resolved by asking the binary rather than by reimplementing the platform rules, so
	// this test does not have to know whether it is on darwin or linux.
	out := c.ok("config", "show")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "config file") {
			return strings.TrimSpace(strings.TrimPrefix(line, "config file"))
		}
	}
	c.t.Fatalf("no config file line in:\n%s", out)
	return ""
}

func (c *cli) write(path, body string) {
	c.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		c.t.Fatalf("write %s: %v", path, err)
	}
}

// The first-run path, in the order the usage text presents it.
func TestFirstRun(t *testing.T) {
	c := newCLI(t)

	// Nothing configured: not an error, and the message says what to do rather than showing
	// a failure to somebody who has not written a file yet.
	out := c.ok("config", "check")
	if !strings.Contains(out, "built-in defaults") {
		t.Errorf("check with no file does not explain itself:\n%s", out)
	}
	if !strings.Contains(out, "throttle init") {
		t.Errorf("check with no file does not name the next command:\n%s", out)
	}

	out = c.ok("init")
	if !strings.Contains(out, "wrote") {
		t.Errorf("init does not say what it wrote:\n%s", out)
	}

	out = c.ok("config", "check")
	if !strings.Contains(out, ": ok") {
		t.Errorf("check does not confirm the starter file:\n%s", out)
	}
	// No ledger yet, and that is not a configuration problem.
	if !strings.Contains(out, "nothing to compare against") {
		t.Errorf("check does not explain the absent ledger:\n%s", out)
	}

	out = c.ok("define", "-id", "default")
	if !strings.Contains(out, "defined") {
		t.Errorf("define does not confirm:\n%s", out)
	}

	// (17) Immediately after define, the file and the ledger agree. A false drift report
	// here is the same defect as a silent rewrite, from the other direction.
	out = c.ok("config", "check")
	if !strings.Contains(out, "already stored and identical") {
		t.Errorf("check after define does not report the budget as identical:\n%s", out)
	}

	// The configured default budget is what makes "status" work with no arguments, which is
	// the difference between a tool checked daily and one checked when the flag is
	// remembered.
	out = c.ok("status")
	if !strings.Contains(out, "default") {
		t.Errorf("status with no -id does not use the configured default:\n%s", out)
	}
}

// (21) init refuses to replace an existing file, and says how to insist.
func TestInitDoesNotOverwrite(t *testing.T) {
	c := newCLI(t)
	c.ok("init")
	path := c.configPath()

	// A local edit, standing in for months of them.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	edited := string(body) + "\n# a local edit\n"
	c.write(path, edited)

	out := c.fails("init")
	if !strings.Contains(out, "-force") {
		t.Errorf("init does not mention -force:\n%s", out)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != edited {
		t.Error("init overwrote an existing file without -force")
	}

	c.ok("init", "-force")
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) == edited {
		t.Error("init -force did not replace the file")
	}
}

// init writes one file and creates no stores. A database created as a side effect of writing
// configuration would make "parsing has no side effects" untrue at the CLI level.
func TestInitCreatesNoStores(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	var found []string
	err := filepath.WalkDir(c.home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".db") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("init created databases: %v", found)
	}
}

// (15) config check performs no durable mutation.
//
// The assertion is on the ledger's observable content rather than on the database file's
// bytes. The store runs in WAL mode, where even a read-only open may legitimately checkpoint
// and rewrite the main file, so a byte comparison would fail on an implementation detail while
// telling us nothing about whether any accounting changed. What matters -- and what a user
// would notice -- is that no definition and no period is different afterwards.
func TestConfigCheckDoesNotMutate(t *testing.T) {
	c := newCLI(t)
	c.ok("init")
	c.ok("define", "-id", "default")

	budgetsBefore := c.ok("budgets")
	periodsBefore := c.ok("periods", "-id", "default")

	c.ok("config", "check")
	c.ok("config", "show")
	c.ok("config", "check", "-q")

	if got := c.ok("budgets"); got != budgetsBefore {
		t.Errorf("the stored definitions changed:\nbefore:\n%s\nafter:\n%s", budgetsBefore, got)
	}
	if got := c.ok("periods", "-id", "default"); got != periodsBefore {
		t.Errorf("the stored periods changed:\nbefore:\n%s\nafter:\n%s", periodsBefore, got)
	}

	// Nor does check materialize a period for a budget the file describes and the ledger has
	// never stored: reading a file must not create the thing it describes.
	path := c.configPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	c.write(path, string(body)+`
  planned:
    amount: $50
    period:
      anchor: 2026-09-01
`)
	out := c.ok("config", "check")
	if !strings.Contains(out, "not stored yet") {
		t.Errorf("check does not report the unstored budget:\n%s", out)
	}
	if got := c.ok("budgets"); got != budgetsBefore {
		t.Errorf("check stored a budget that was only in the file:\n%s", got)
	}
}

// (18, 20) A changed definition is reported and refused, and the materialized period is not
// rewritten.
//
// A stored definition governs money that has already been spent against it. Rewriting one
// because a file changed would change what a live budget may spend on the strength of a YAML
// edit, so the CLI reports the difference and exits nonzero -- which is what makes "config
// check" usable in CI.
func TestConfigCheckReportsChangedDefinition(t *testing.T) {
	c := newCLI(t)
	c.ok("init")
	c.ok("define", "-id", "default")

	before := c.ok("periods", "-id", "default")

	path := c.configPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	c.write(path, strings.Replace(string(body), "amount: $100", "amount: $500", 1))

	out := c.fails("config", "check")
	if !strings.Contains(out, "allocation") {
		t.Errorf("check does not name the changed field:\n%s", out)
	}
	if !strings.Contains(out, "$100.00") || !strings.Contains(out, "$500.00") {
		t.Errorf("check does not show both values:\n%s", out)
	}
	if !strings.Contains(out, "does not rewrite one") {
		t.Errorf("check does not say that nothing was changed:\n%s", out)
	}
	// And it names the command that would: reporting a difference without a way to act on it
	// leaves the reader stuck.
	if !strings.Contains(out, "throttle config apply") {
		t.Errorf("check does not name the command that reconciles them:\n%s", out)
	}

	// The stored budget and its period are untouched: reading a file did not move money.
	if after := c.ok("periods", "-id", "default"); after != before {
		t.Errorf("the materialized period changed after a config edit:\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
	if out := c.ok("budgets"); !strings.Contains(out, "$100.00") {
		t.Errorf("the stored allocation changed after a config edit:\n%s", out)
	}

	// define is equally refusing, and says what to do rather than failing obscurely.
	out = c.fails("define", "-id", "default")
	if !strings.Contains(out, "differs") {
		t.Errorf("define does not explain the conflict:\n%s", out)
	}
}

// (22) The unauthenticated-dashboard warning fires for an address set in the config file,
// not only for the -listen flag.
//
// A warning that only fired for the flag would stop firing the moment somebody made the
// setting permanent, which is exactly when it matters most. The address is in TEST-NET-1, so
// the bind fails and the process exits -- after the warning, which is the point of warning
// before opening the socket.
func TestServeWarnsAboutConfiguredNonLoopback(t *testing.T) {
	c := newCLI(t)
	c.ok("init")
	path := c.configPath()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	c.write(path, strings.Replace(string(body),
		"listen: 127.0.0.1:7654", "listen: 192.0.2.1:7654", 1))

	// config show says so before anybody runs serve.
	if out := c.ok("config", "show"); !strings.Contains(out, "NOT loopback") {
		t.Errorf("config show does not flag the configured address:\n%s", out)
	}

	out := c.fails("serve")
	if !strings.Contains(out, "not loopback") {
		t.Errorf("serve does not warn about the configured address:\n%s", out)
	}
	// Case-folded: the two renderings differ in emphasis ("NO authentication" in the warning,
	// "no authentication" in the show line) and the assertion is about the fact, not the shout.
	if !strings.Contains(strings.ToLower(out), "no authentication") {
		t.Errorf("serve does not say why that matters:\n%s", out)
	}
	// And it names where the setting came from, because a surprising bind is usually a file
	// somebody forgot about.
	if !strings.Contains(out, "dashboard.listen") {
		t.Errorf("serve does not name the setting:\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("serve does not name the file:\n%s", out)
	}
}

// The loopback default draws no warning, or the warning stops meaning anything.
func TestServeQuietOnLoopback(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	// Port 1 is privileged, so the bind fails immediately and serve does not linger. The
	// address is still loopback, which is what is being asserted.
	out, _ := c.run("serve", "-listen", "127.0.0.1:1")
	if strings.Contains(out, "not loopback") {
		t.Errorf("serve warned about a loopback address:\n%s", out)
	}
}

// (23) reconcile uses the configured stores, and does not become automatic merely because
// configuration now knows where they are.
func TestReconcileUsesConfiguredStores(t *testing.T) {
	c := newCLI(t)

	dir := filepath.Join(c.home, "stores")
	c.write(filepath.Join(c.home, "throttle.yaml"), `
version: 1
store:
  ledger: `+filepath.Join(dir, "ledger.db")+`
  activity: `+filepath.Join(dir, "activity.db")+`
defaults:
  budget: research
budgets:
  research:
    amount: $100
    period:
      # A fixed date in the past, not a computed one: a budget whose anchor is in the
      # future has no current period, and a test that computes "the first of this month"
      # is a test that reimplements the thing it is checking.
      anchor: 2026-01-01
`)
	cfgFlag := []string{"-config", filepath.Join(c.home, "throttle.yaml")}

	c.ok(append([]string{"define", "-id", "research"}, cfgFlag...)...)
	if _, err := os.Stat(filepath.Join(dir, "ledger.db")); err != nil {
		t.Fatalf("the configured ledger was not used: %v", err)
	}

	// Reconciliation compares the two stores, so it cannot proceed without the activity
	// side: with nothing to compare against, every reservation would look stranded. The
	// error names the configured path, which is the likeliest thing to be wrong.
	out := c.fails(append([]string{"reconcile"}, cfgFlag...)...)
	if !strings.Contains(out, filepath.Join(dir, "activity.db")) {
		t.Errorf("reconcile does not name the configured activity path:\n%s", out)
	}
	if !strings.Contains(out, "store.activity") {
		t.Errorf("reconcile does not name the setting to check:\n%s", out)
	}

	// And the flag beats the file: the path reported is the one on the command line, not the
	// configured one. Asserted through the error because an activity store is created by the
	// spending path rather than by the CLI, so there is no CLI-only way to produce a real one.
	elsewhere := filepath.Join(c.home, "nowhere", "activity.db")
	out = c.fails(append(append([]string{"reconcile"}, cfgFlag...), "-activity", elsewhere)...)
	if !strings.Contains(out, elsewhere) {
		t.Errorf("the -activity flag did not override the configured path:\n%s", out)
	}
	if strings.Contains(out, filepath.Join(dir, "activity.db")) {
		t.Errorf("reconcile still reports the configured path with -activity passed:\n%s", out)
	}
}

// A budget defined before its term starts is stored, reported as not started, and explained
// in dates rather than in the engine's vocabulary.
//
// Defining a grant in advance of its start date is ordinary. It used to make "define" print
// "no such period: 2027-01-01T00:00:00Z precedes the anchor 2027-09-01T00:00:00Z" and exit 1 --
// after storing the definition, so the command reported failure about something that had
// succeeded, and the message named two timestamps and no course of action.
func TestFutureBudgetIsNotAFailure(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "throttle.yaml")
	// Far enough out to stay in the future for years, since the anchor is a literal.
	c.write(path, `
version: 1
defaults:
  budget: later
budgets:
  later:
    amount: $100
    period:
      anchor: 2099-01-01
`)
	cfg := []string{"-config", path}

	out := c.ok(append([]string{"define", "-id", "later"}, cfg...)...)
	if !strings.Contains(out, "not started yet") {
		t.Errorf("define does not say the budget has not started:\n%s", out)
	}
	if !strings.Contains(out, "2099-01-01") {
		t.Errorf("define does not say when it begins:\n%s", out)
	}
	if strings.Contains(out, "no such period") {
		t.Errorf("define leaks the engine's message:\n%s", out)
	}

	// Stored regardless: the command succeeded, so the definition is there.
	if out := c.ok(append([]string{"budgets"}, cfg...)...); !strings.Contains(out, "later") {
		t.Errorf("the definition was not stored:\n%s", out)
	}

	// status still cannot report a position -- there is none -- but it says why.
	out = c.fails(append([]string{"status"}, cfg...)...)
	if !strings.Contains(out, "not started yet") {
		t.Errorf("status does not explain why there is no position:\n%s", out)
	}
	if strings.Contains(out, "precedes the anchor") {
		t.Errorf("status leaks the engine's message:\n%s", out)
	}

	// periods is empty, and says which of the two reasons applies.
	out = c.ok(append([]string{"periods", "-id", "later"}, cfg...)...)
	if !strings.Contains(out, "not started yet") {
		t.Errorf("periods does not explain the empty listing:\n%s", out)
	}
}

// The shipped example works end to end through the real CLI, not merely through the loader.
//
// TestExampleConfigIsCurrent asserts the dates are live; this asserts the file a reader
// actually copies produces budgets they can then inspect.
func TestExampleConfigRunsEndToEnd(t *testing.T) {
	c := newCLI(t)

	body, err := os.ReadFile(filepath.Join("..", "..", "examples", "throttle.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	path := filepath.Join(c.home, "throttle.yaml")
	// The example pins ~/throttle-demo paths, which resolve under the isolated home.
	c.write(path, string(body))
	cfg := []string{"-config", path}

	c.ok(append([]string{"config", "check"}, cfg...)...)

	// Parents before children, which is the order the loader guarantees and PutDefinition
	// requires.
	for _, id := range []string{"research", "chat", "agents", "award"} {
		if out := c.ok(append([]string{"define", "-id", id}, cfg...)...); !strings.Contains(out, "defined") {
			t.Errorf("define %s:\n%s", id, out)
		}
	}

	// Every budget has a live position, and the child's chain names its parent.
	out := c.ok(append([]string{"status", "-id", "chat", "-chain"}, cfg...)...)
	if !strings.Contains(out, "research") {
		t.Errorf("the chain does not include the parent:\n%s", out)
	}
	if !strings.Contains(out, "sustainable rate") {
		t.Errorf("status reports no pacing:\n%s", out)
	}

	// And the file now agrees with the ledger, which is the state a user should end in.
	out = c.ok(append([]string{"config", "check"}, cfg...)...)
	if !strings.Contains(out, "4 budget(s) already stored and identical") {
		t.Errorf("check does not report all four as identical:\n%s", out)
	}
}

// A flag overrides the file for one command, and the file is unchanged by it: precedence at
// the CLI level, which is where a user meets it.
func TestFlagOverridesFile(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	elsewhere := filepath.Join(c.home, "elsewhere", "ledger.db")
	c.ok("define", "-id", "default", "-db", elsewhere)

	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("-db did not redirect the ledger: %v", err)
	}
	// The default location was not used, so the flag replaced the value rather than adding
	// to it.
	out := c.ok("budgets", "-db", elsewhere)
	if !strings.Contains(out, "default") {
		t.Errorf("the redirected ledger does not hold the budget:\n%s", out)
	}
	out = c.ok("budgets")
	if strings.Contains(out, "$100.00") {
		t.Errorf("the configured ledger was written to as well:\n%s", out)
	}
}

// THROTTLE_LEDGER redirects the store, and a flag still beats it.
func TestEnvironmentAndFlagPrecedence(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	fromEnv := filepath.Join(c.home, "env", "ledger.db")
	cmd := exec.Command(binary, "define", "-id", "default")
	cmd.Env = []string{"HOME=" + c.home, "PATH=" + os.Getenv("PATH"), "TZ=UTC",
		"XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "THROTTLE_LEDGER=" + fromEnv}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("define with THROTTLE_LEDGER: %v\n%s", err, out)
	}
	if _, err := os.Stat(fromEnv); err != nil {
		t.Fatalf("THROTTLE_LEDGER did not redirect the ledger: %v", err)
	}

	// config show attributes it, which is what makes an unexpected path explainable.
	cmd = exec.Command(binary, "config", "show")
	cmd.Env = []string{"HOME=" + c.home, "PATH=" + os.Getenv("PATH"), "TZ=UTC",
		"XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "THROTTLE_LEDGER=" + fromEnv}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "environment") {
		t.Errorf("config show does not attribute the ledger to the environment:\n%s", out)
	}
}

// (25) A config error names the file, the field, and what to do about it.
func TestConfigErrorsAreActionable(t *testing.T) {
	c := newCLI(t)
	path := filepath.Join(c.home, "bad.yaml")
	c.write(path, `
version: 1
budgets:
  research:
    amount: $4O00
    period:
      anchor: 2026-09-01
  chat:
    parent: researchx
    amount: $1,000
`)

	out := c.fails("config", "check", "-config", path)

	if !strings.Contains(out, path) {
		t.Errorf("error does not name the file:\n%s", out)
	}
	if !strings.Contains(out, "budgets.research.amount") {
		t.Errorf("error does not name the field:\n%s", out)
	}
	if !strings.Contains(out, `invalid money "$4O00"`) {
		t.Errorf("error does not quote the bad value:\n%s", out)
	}
	// No stack trace in normal CLI operation: it buries the useful line.
	if strings.Contains(out, "goroutine ") || strings.Contains(out, ".go:") {
		t.Errorf("error output looks like a panic:\n%s", out)
	}
}

// A -config naming a file that is not there is an error rather than a silent fall back to
// defaults, which would run the wrong budget quietly.
func TestMissingExplicitConfigFails(t *testing.T) {
	c := newCLI(t)
	out := c.fails("config", "check", "-config", filepath.Join(c.home, "absent.yaml"))
	if !strings.Contains(out, "no such file") {
		t.Errorf("error does not say the file is missing:\n%s", out)
	}
}

// -q says nothing on success and still reports through the exit status, which is what a CI
// step wants.
func TestConfigCheckQuiet(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	if out := c.ok("config", "check", "-q"); strings.TrimSpace(out) != "" {
		t.Errorf("check -q printed output:\n%s", out)
	}

	path := c.configPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	c.write(path, strings.Replace(string(body), "amount: $100", "amount: nonsense", 1))
	if out := c.fails("config", "check", "-q"); !strings.Contains(out, "amount") {
		t.Errorf("check -q suppressed a failure:\n%s", out)
	}
}

// The usage text is grouped by task and does not use internal vocabulary.
func TestUsageVocabulary(t *testing.T) {
	c := newCLI(t)
	out, _ := c.run("help")

	for _, want := range []string{"getting started", "watching the money", "init", "config", "define"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage does not mention %q:\n%s", want, out)
		}
	}
	// Implementation vocabulary that means nothing to a user reading a help screen.
	for _, unwanted := range []string{"leg", "scope row", "carry-final", "fingerprint"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("usage uses the internal term %q:\n%s", unwanted, out)
		}
	}
}

// Every command answers -h, and says so successfully.
//
// Asking a tool what it does is not a failure, and the flag package's default behaviour --
// print the usage, then return ErrHelp so the caller reports an error and exits nonzero -- is
// the difference between a command a script can call and one it cannot. The synopsis is
// checked too, because "Usage of status:" names a flag set rather than anything anyone types.
func TestEveryCommandHasHelp(t *testing.T) {
	c := newCLI(t)

	commands := [][]string{
		{"init"}, {"config"}, {"config", "check"}, {"config", "show"}, {"config", "diff"},
		{"config", "apply"}, {"define"}, {"budgets"}, {"rename"}, {"status"}, {"periods"},
		{"advance"}, {"recover"}, {"reconcile"}, {"serve"}, {"version"},
	}
	for _, cmd := range commands {
		name := strings.Join(cmd, " ")
		out, code := c.run(append(cmd, "-h")...)
		if code != 0 {
			t.Errorf("throttle %s -h exited %d, want 0:\n%s", name, code, out)
		}
		if !strings.Contains(out, "usage: throttle "+name) {
			t.Errorf("throttle %s -h does not open with its own synopsis:\n%s", name, out)
		}
		// The flag package's heading, which names an internal flag set and tells the reader
		// nothing about the command.
		if strings.Contains(out, "Usage of ") {
			t.Errorf("throttle %s -h uses the flag package's default heading:\n%s", name, out)
		}
		if strings.Contains(out, "help requested") {
			t.Errorf("throttle %s -h reports asking for help as an error:\n%s", name, out)
		}
	}
}

// A command that writes says so in its own help.
//
// Six commands mutate durable state and nine do not, and the ones that do are exactly the ones
// somebody wants to be sure about before running them against a real ledger. Nothing infers
// this from the code, so it is asserted: a new mutating command that arrives without the word
// fails here.
func TestMutatingCommandsSoundMutating(t *testing.T) {
	c := newCLI(t)

	writes := [][]string{
		{"config", "apply"}, {"define"}, {"rename"}, {"advance"}, {"recover"}, {"reconcile"},
	}
	for _, cmd := range writes {
		out, _ := c.run(append(cmd, "-h")...)
		if !strings.Contains(out, "WRITES") {
			t.Errorf("throttle %s -h does not say it writes:\n%s", strings.Join(cmd, " "), out)
		}
	}

	// And the read-only ones do not claim to.
	reads := [][]string{{"config", "check"}, {"config", "show"}, {"config", "diff"}, {"serve"}}
	for _, cmd := range reads {
		out, _ := c.run(append(cmd, "-h")...)
		if strings.Contains(out, "WRITES") {
			t.Errorf("throttle %s -h says it writes:\n%s", strings.Join(cmd, " "), out)
		}
	}
}

// An unknown budget is named as such, with the next step, on every command that takes one.
//
// The failure this replaces was worse than a bad message. "throttle periods -id typo" printed
// "no periods materialized yet" and exited 0, and "throttle recover -id typo" printed "no
// expired reservations to recover" -- both true of a budget that does not exist, and both
// read as reassurance about one that does.
func TestUnknownBudgetIsNamed(t *testing.T) {
	c := newCLI(t)
	c.ok("init")
	c.ok("config", "apply")

	for _, args := range [][]string{
		{"status", "-id", "typo"},
		{"periods", "-id", "typo"},
		{"recover", "-id", "typo"},
		{"advance", "-id", "typo"},
		{"rename", "typo", "New name"},
	} {
		out := c.fails(args...)
		if !strings.Contains(out, `"typo"`) {
			t.Errorf("throttle %s does not name the budget:\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(out, "throttle budgets") {
			t.Errorf("throttle %s does not say how to see the stored budgets:\n%s",
				strings.Join(args, " "), out)
		}
		// Which package noticed is not the reader's problem.
		for _, leak := range []string{"engine:", "ledger:", "sqlite:"} {
			if strings.Contains(out, leak) {
				t.Errorf("throttle %s leaks the internal layer %q:\n%s",
					strings.Join(args, " "), leak, out)
			}
		}
	}
}

// A budget the file defines but nothing has stored is a first run, not a typo.
//
// The two situations produce the same error from the ledger and want opposite things said
// about them: one needs "throttle config apply", the other needs to know the name is wrong.
func TestBudgetDefinedButNotAppliedSaysToApply(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	// Deliberately not applied.
	out := c.fails("status")
	if !strings.Contains(out, "throttle config apply") {
		t.Errorf("status before apply does not say to apply:\n%s", out)
	}
	if !strings.Contains(out, "not stored yet") {
		t.Errorf("status before apply does not distinguish unstored from unknown:\n%s", out)
	}
}

// Errors stay sentences, on every path a new user is likely to hit first.
//
// Not an assertion about wording, which will change. A panic trace, a bare Go error value, or
// a wrapped chain of package prefixes are each a sign that a failure reached the terminal
// without anybody having decided what to say about it.
func TestErrorPathsStayReadable(t *testing.T) {
	c := newCLI(t)
	c.ok("init")

	malformed := filepath.Join(c.home, "malformed.yaml")
	c.write(malformed, "version: 1\nbudgets: [not, a, map]\n")

	badMoney := filepath.Join(c.home, "money.yaml")
	c.write(badMoney, "version: 1\nbudgets:\n  a:\n    amount: twelve dollars\n")

	future := filepath.Join(c.home, "version.yaml")
	c.write(future, "version: 99\nbudgets: {}\n")

	// A ledger path throttle cannot open. Not a missing directory -- one of those is created,
	// because a configured path names where a database is meant to live -- but a path whose
	// parent is a regular file, which no amount of MkdirAll will fix.
	blocked := filepath.Join(c.home, "a-file")
	c.write(blocked, "not a directory\n")

	for _, args := range [][]string{
		{"config", "check", "-config", filepath.Join(c.home, "absent.yaml")},
		{"config", "check", "-config", malformed},
		{"config", "check", "-config", badMoney},
		{"config", "check", "-config", future},
		{"status", "-id", "typo"},
		{"reconcile"},
		{"budgets", "-db", filepath.Join(blocked, "ledger.db")},
	} {
		out := c.fails(args...)
		name := strings.Join(args, " ")
		if strings.Contains(out, "goroutine ") || strings.Contains(out, "panic:") {
			t.Errorf("throttle %s panicked:\n%s", name, out)
		}
		if strings.Count(out, "\n") > 8 {
			t.Errorf("throttle %s printed a wall of text:\n%s", name, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("throttle %s failed silently", name)
		}
	}
}

// Asking for the version is one thing, and asking for it twice is the same thing.
//
// The dashboard footer and this command read one variable, so a screenshot and a bug report
// cannot disagree about which build they describe. What that value is depends on how the
// binary was built and is not pinned here; that it is a plausible version, and identical in
// both places, is.
func TestVersionIsOneValue(t *testing.T) {
	c := newCLI(t)
	got := strings.TrimSpace(c.ok("version"))
	if got == "" {
		t.Fatal("throttle version printed nothing")
	}
	if !strings.HasPrefix(got, "0.1.0") && !strings.HasPrefix(got, "v0.1.0") {
		t.Errorf("version %q does not look like this project's version", got)
	}
	// A development build says so rather than claiming to be a release.
	if got == "(devel)" || strings.HasPrefix(got, "v0.0.0-") {
		t.Errorf("version %q reports a placeholder as though it were a release", got)
	}
}
