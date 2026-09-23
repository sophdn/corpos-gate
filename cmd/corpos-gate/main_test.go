package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// customOnlyConfig disables every go check and wires two custom checks so
// `run` executes hermetically (real `sh -c true` / `false`) without
// invoking the real go toolchain.
const customOnlyConfig = `stack: go
units:
  - dir: go
    tags: sqlite_fts5
checks:
  format: { enabled: false, tier: pre-commit }
custom:
  - { name: yes-check, cmd: "true",  tier: pre-commit }
  - { name: push-only, cmd: "true",  tier: pre-push }
`

func writeGateYML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gate.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write gate.yml: %v", err)
	}
	return p
}

func TestRunCLINoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCLI(nil, &out, &errb); code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage") {
		t.Fatalf("expected usage on stderr")
	}
}

func TestRunCLIHelpAndUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCLI([]string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	out.Reset()
	errb.Reset()
	if code := runCLI([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
}

func TestPlanSubcommand(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	code := runCLI([]string{"plan", "--tier=pre-commit", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("plan exit = %d (%s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "yes-check") || strings.Contains(out.String(), "push-only") {
		t.Fatalf("pre-commit plan wrong:\n%s", out.String())
	}
	// pre-push is the superset — includes both.
	out.Reset()
	runCLI([]string{"plan", "--tier=pre-push", "--config", cfg}, &out, &errb)
	if !strings.Contains(out.String(), "yes-check") || !strings.Contains(out.String(), "push-only") {
		t.Fatalf("pre-push plan should include both:\n%s", out.String())
	}
}

func TestPlanSkipFlag(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	// pre-push includes both custom checks; --skip=push-only drops it.
	code := runCLI([]string{"plan", "--tier=pre-push", "--skip=push-only", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("plan --skip exit = %d (%s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "yes-check") || strings.Contains(out.String(), "push-only") {
		t.Fatalf("--skip=push-only should omit push-only, keep yes-check:\n%s", out.String())
	}
}

func TestParseSkip(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"vuln", []string{"vuln"}},
		{"vuln,build", []string{"vuln", "build"}},
		{" vuln , build ,", []string{"vuln", "build"}},
	}
	for _, c := range cases {
		got := parseSkip(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("parseSkip(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRunSubcommandPass(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	code := runCLI([]string{"run", "--tier=pre-commit", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0 (%s)\n%s", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Fatalf("expected PASS lines:\n%s", out.String())
	}
}

func TestRunSubcommandFail(t *testing.T) {
	body := `stack: go
units:
  - dir: go
custom:
  - { name: boom, cmd: "false", tier: pre-commit }
`
	cfg := writeGateYML(t, body)
	var out, errb bytes.Buffer
	code := runCLI([]string{"run", "--tier=pre-commit", "--config", cfg}, &out, &errb)
	if code != 1 {
		t.Fatalf("run with failing check exit = %d, want 1\n%s", code, out.String())
	}
}

func TestRunListFlag(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	code := runCLI([]string{"run", "--tier=pre-commit", "--list", "--config", cfg}, &out, &errb)
	if code != 0 {
		t.Fatalf("run --list exit = %d", code)
	}
	if !strings.Contains(out.String(), "plan") {
		t.Fatalf("run --list should print plan:\n%s", out.String())
	}
}

func TestBadTier(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	if code := runCLI([]string{"plan", "--tier=whenever", "--config", cfg}, &out, &errb); code != 2 {
		t.Fatalf("bad tier exit = %d, want 2", code)
	}
}

func TestMissingConfig(t *testing.T) {
	var out, errb bytes.Buffer
	code := runCLI([]string{"plan", "--tier=pre-commit", "--config", filepath.Join(t.TempDir(), "nope.yml")}, &out, &errb)
	if code != 1 {
		t.Fatalf("missing config exit = %d, want 1", code)
	}
}

func TestFindConfigWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "gate.yml"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	got, err := findConfig("")
	if err != nil {
		t.Fatalf("findConfig: %v", err)
	}
	// Resolve symlinks (macOS /tmp) before comparing.
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(filepath.Join(root, "gate.yml"))
	if gotResolved != wantResolved {
		t.Fatalf("findConfig = %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindConfigExplicit(t *testing.T) {
	if got, err := findConfig("/some/explicit/path.yml"); err != nil || got != "/some/explicit/path.yml" {
		t.Fatalf("explicit config = %q %v", got, err)
	}
}

// ── version subcommand ──────────────────────────────────────────────────────

// The version-line format is pinned by this test so a format change fails in
// the suite rather than downstream in anything that parses the version line.
func TestVersionSubcommandShape(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runCLI([]string{arg}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
			}
			got := stdout.String()
			if !strings.HasPrefix(got, "corpos-gate ") {
				t.Errorf("version line = %q, want a 'corpos-gate ' prefix", got)
			}
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("version line = %q, want a trailing newline", got)
			}
			fields := strings.Fields(got)
			if len(fields) != 4 || fields[2] != "built" {
				t.Errorf("version line = %q, want 'corpos-gate <sha> built <unix>'", got)
			}
			if fields[1] != gitSHA || fields[3] != builtAtUnix {
				t.Errorf("version line does not report the stamped vars: %q", got)
			}
			if stderr.Len() != 0 {
				t.Errorf("version wrote to stderr: %q", stderr.String())
			}
		})
	}
}

// An unstamped build (plain `go build`) must still produce a parseable line
// rather than an empty field, so a consumer's SHA check fails loudly instead of
// matching an empty string.
func TestVersionDefaultsAreNonEmpty(t *testing.T) {
	if gitSHA == "" || builtAtUnix == "" {
		t.Fatal("gitSHA/builtAtUnix defaults are empty; an unstamped build would emit a malformed line")
	}
	if got := versionLine(); len(strings.Fields(got)) != 4 {
		t.Errorf("versionLine() = %q, want four fields", got)
	}
}
