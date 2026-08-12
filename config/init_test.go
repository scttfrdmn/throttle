package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// (21) An existing config file is never overwritten without an explicit force.
//
// A config file is hand-edited and the edits are not recoverable. "throttle init" run a
// second time in the wrong directory would otherwise silently discard somebody's budget
// definitions, and nothing about the output would suggest anything was lost.
func TestWriteDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	original := "# hand written, months of edits\nversion: 1\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := Write(path, "2026-09-01", false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	// The message has to say what to do, or the reader's next move is to delete the file.
	if !strings.Contains(err.Error(), "-force") {
		t.Errorf("error does not mention -force: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != original {
		t.Error("the existing file was modified")
	}
}

// With -force it is replaced, because that is what the flag is for.
func TestWriteForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Write(path, "2026-09-01", true); err != nil {
		t.Fatalf("Write with force: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), "throttle configuration") {
		t.Error("the template was not written")
	}
}

// The starter file parses under the real loader, produces a usable budget, and is not
// world-readable.
//
// A first experience that fails at "throttle config check" is worse than no starter file, and
// the template is a string constant that no other test would catch drifting.
func TestWriteProducesLoadableConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", ConfigFileName)

	if err := Write(path, TemplateAnchor(2026, 9), false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 0600 rather than 0644: there are no credentials in it and never will be, but a budget
	// file names what an organization spends and on what.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}

	cfg, err := LoadFile(path, testEnv(t, "linux", dir))
	if err != nil {
		t.Fatalf("the starter file does not load: %v", err)
	}
	d := def(t, cfg, "default")
	if err := d.Validate(); err != nil {
		t.Errorf("the starter budget does not validate: %v", err)
	}
	if cfg.DefaultBudget != "default" {
		t.Errorf("default budget = %q, want default", cfg.DefaultBudget)
	}
	// The anchor is the one passed in, not one from the clock: a caller that owns the clock
	// is a test that does not fail on the first of the month.
	if got := d.AnchorAt.UTC().Format("2006-01-02"); got != "2026-09-01" {
		t.Errorf("anchor = %s, want 2026-09-01", got)
	}
}

// init creates one text file and nothing else: no database, no credential, no cloud
// resource. Creating a store is the business of the command that needs one.
func TestWriteCreatesOnlyTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := Write(path, "2026-09-01", false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ConfigFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("init created more than the config file: %v", names)
	}
}

// The starter file mentions the security posture it ships with, because a default nobody
// knows about is a default that gets changed without thinking.
func TestTemplateExplainsLoopback(t *testing.T) {
	if !strings.Contains(Template, "no authentication") {
		t.Error("the template does not say why the dashboard binds loopback")
	}
	if !strings.Contains(Template, DefaultListen) {
		t.Error("the template does not use the loopback default")
	}
}

func TestTemplateAnchor(t *testing.T) {
	// The first of the month rather than today: a budget anchored mid-month has periods
	// that run 14th-to-14th, which is not what anyone writing "monthly" means.
	if got := TemplateAnchor(2026, 9); got != "2026-09-01" {
		t.Errorf("TemplateAnchor = %q, want 2026-09-01", got)
	}
	if got := TemplateAnchor(2026, 12); got != "2026-12-01" {
		t.Errorf("TemplateAnchor = %q, want 2026-12-01", got)
	}
}
