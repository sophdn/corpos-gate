package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sophdn/corpos-gate/internal/gate"
)

// recordingGit is a fake gitFn that answers a fixed map of git queries
// and records every call so config-mutating calls can be asserted. Any
// unmatched query returns "" (the benign answer for `git config …`).
type recordingGit struct {
	answers map[string]string
	calls   []string
}

func (g *recordingGit) fn(dir string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	g.calls = append(g.calls, key)
	if v, ok := g.answers[key]; ok {
		return v, nil
	}
	return "", nil
}

func (g *recordingGit) called(substr string) bool {
	for _, c := range g.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// mainCheckoutGit builds a fake git for a MAIN checkout rooted at root.
func mainCheckoutGit(root string) *recordingGit {
	return &recordingGit{answers: map[string]string{
		"rev-parse --show-toplevel":                         root,
		"rev-parse --path-format=absolute --git-common-dir": filepath.Join(root, ".git"),
		"rev-parse --path-format=absolute --git-path hooks": filepath.Join(root, ".git", "hooks"),
	}}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestInitGoMainCheckout proves init on a Go main checkout writes a
// go-stack starter gate.yml + both hooks into .git/hooks, and is
// idempotent (second run keeps gate.yml, rewrites hooks).
func TestInitGoMainCheckout(t *testing.T) {
	root := t.TempDir()
	// A go.mod at the root → stack go, unit ".".
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	g := mainCheckoutGit(root)
	var out bytes.Buffer
	if err := initRepo(root, g.fn, &out); err != nil {
		t.Fatalf("initRepo: %v", err)
	}

	cfg := readFile(t, filepath.Join(root, "gate.yml"))
	if !strings.Contains(cfg, "stack: go") || !strings.Contains(cfg, "dir: .") {
		t.Fatalf("go starter gate.yml wrong:\n%s", cfg)
	}
	if !strings.Contains(cfg, "tier: pre-commit") || !strings.Contains(cfg, "tier: pre-push") {
		t.Fatalf("starter should tier checks:\n%s", cfg)
	}

	hooksDir := filepath.Join(root, ".git", "hooks")
	pc := readFile(t, filepath.Join(hooksDir, "pre-commit"))
	pp := readFile(t, filepath.Join(hooksDir, "pre-push"))
	if !strings.Contains(pc, "run --tier=pre-commit") || !strings.Contains(pp, "run --tier=pre-push") {
		t.Fatalf("hooks call wrong tier:\npre-commit=%s\npre-push=%s", pc, pp)
	}
	if !strings.Contains(pc, managedMarker) {
		t.Fatalf("hook missing managed marker:\n%s", pc)
	}
	// git-native bypass must be documented and NOT defeated.
	if !strings.Contains(pc, "--no-verify") {
		t.Fatalf("hook should document --no-verify escape:\n%s", pc)
	}
	if !strings.Contains(pc, "command -v corpos-gate") {
		t.Fatalf("hook should guard on corpos-gate being on PATH:\n%s", pc)
	}
	// A main checkout must NOT set a shared core.hooksPath.
	if g.called("core.hooksPath") {
		t.Fatalf("main checkout must not set core.hooksPath: %v", g.calls)
	}

	// Idempotent second run: gate.yml kept, hooks rewritten, no error.
	out.Reset()
	if err := initRepo(root, mainCheckoutGit(root).fn, &out); err != nil {
		t.Fatalf("second initRepo: %v", err)
	}
	if !strings.Contains(out.String(), "gate.yml exists, keeping it") {
		t.Fatalf("second run should keep gate.yml:\n%s", out.String())
	}
	if readFile(t, filepath.Join(hooksDir, "pre-commit")) != pc {
		t.Fatalf("second run should reproduce identical managed hook")
	}
}

// TestInitLinkedWorktree proves init on a linked worktree installs
// gate-only hooks into the worktree's private git dir and sets a
// PER-WORKTREE core.hooksPath (never a shared config), mirroring
// scripts/worktree-setup.sh.
func TestInitLinkedWorktree(t *testing.T) {
	mainRoot := t.TempDir()
	wt := t.TempDir() // the linked worktree root (a different dir)
	gitDir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	g := &recordingGit{answers: map[string]string{
		"rev-parse --show-toplevel":                         wt,
		"rev-parse --path-format=absolute --git-common-dir": filepath.Join(mainRoot, ".git"),
		"rev-parse --absolute-git-dir":                      gitDir,
	}}
	var out bytes.Buffer
	if err := initRepo(wt, g.fn, &out); err != nil {
		t.Fatalf("initRepo worktree: %v", err)
	}
	hooksDir := filepath.Join(gitDir, "gate-only-hooks")
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-commit")); err != nil {
		t.Fatalf("worktree hook not written to private git dir: %v", err)
	}
	if !g.called("extensions.worktreeConfig true") {
		t.Fatalf("worktree init should enable worktreeConfig: %v", g.calls)
	}
	if !g.called("config --worktree core.hooksPath") {
		t.Fatalf("worktree init should set per-worktree core.hooksPath: %v", g.calls)
	}
	if !strings.Contains(out.String(), "linked worktree") {
		t.Fatalf("summary should note the linked-worktree path:\n%s", out.String())
	}
}

// TestStarterConfigDetectsStack proves stack auto-detection: go (nested
// module), ts (package.json), and shell (fallback).
func TestStarterConfigDetectsStack(t *testing.T) {
	// Nested go.mod (like this repo's go/ layout) → unit "go".
	goRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goRoot, "go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goRoot, "go", "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stack, body := starterConfig(goRoot); stack != "go" || !strings.Contains(body, "dir: go") {
		t.Fatalf("nested go detection: stack=%s body=%s", stack, body)
	}

	tsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(tsRoot, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stack, body := starterConfig(tsRoot); stack != "ts" || !strings.Contains(body, "stack: ts") {
		t.Fatalf("ts detection: stack=%s", stack)
	}

	shRoot := t.TempDir()
	if stack, body := starterConfig(shRoot); stack != "shell" || !strings.Contains(body, "stack: shell") {
		t.Fatalf("shell fallback: stack=%s", stack)
	}
}

// TestInitNotGitRepo proves init fails cleanly when the target is not a
// git repo.
func TestInitNotGitRepo(t *testing.T) {
	badGit := func(string, ...string) (string, error) {
		return "", os.ErrNotExist
	}
	var out bytes.Buffer
	if err := initRepo(t.TempDir(), badGit, &out); err == nil {
		t.Fatalf("expected error for non-git dir")
	}
}

// TestCmdInitCLI drives the CLI dispatch against a REAL git repo so
// gitExec + real worktree detection are exercised end-to-end.
func TestCmdInitCLI(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err2 := os.Stat("/bin/git"); err2 != nil {
			t.Skip("git not available")
		}
	}
	root := t.TempDir()
	mustGit(t, root, "init")
	mustGit(t, root, "config", "user.email", "t@t")
	mustGit(t, root, "config", "user.name", "t")

	var out, errb bytes.Buffer
	if code := runCLI([]string{"init", root}, &out, &errb); code != 0 {
		t.Fatalf("init CLI exit=%d err=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "gate.yml")); err != nil {
		t.Fatalf("gate.yml not written: %v", err)
	}
	// The real hooks dir (.git/hooks) should hold the managed pre-commit.
	pc := filepath.Join(root, ".git", "hooks", "pre-commit")
	if body := readFile(t, pc); !strings.Contains(body, "run --tier=pre-commit") {
		t.Fatalf("real hook wrong:\n%s", body)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitExec(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

// writeMarker writes name at root with the given content (empty ok).
func writeMarker(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// assertLoads proves a generated starter body is a VALID gate.yml — parses
// under KnownFields(true) and passes gate.validate (via gate.Load). A
// starter that init writes must be one corpos-gate can then run.
func assertLoads(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gate.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write gate.yml: %v", err)
	}
	if _, err := gate.Load(p); err != nil {
		t.Fatalf("generated starter is not a loadable gate.yml: %v\n%s", err, body)
	}
}

// TestDetectPythonEachMarker proves each of the four Python project-root
// markers, alone, selects the py stack — and that loose *.py files do NOT.
func TestDetectPythonEachMarker(t *testing.T) {
	for _, marker := range []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"} {
		root := t.TempDir()
		writeMarker(t, root, marker, "")
		p := planStarter(root)
		if p.stack != "py" {
			t.Fatalf("marker %s should select py, got %s", marker, p.stack)
		}
		if p.unit != "." {
			t.Fatalf("py unit dir should be '.', got %q", p.unit)
		}
		assertLoads(t, p.body)
	}

	// A loose *.py file is deliberately NOT a Python signal (too many false
	// positives) — it falls through to the shell stack.
	looseRoot := t.TempDir()
	writeMarker(t, looseRoot, "helper.py", "print('hi')\n")
	if p := planStarter(looseRoot); p.stack != "shell" {
		t.Fatalf("loose *.py must not trigger py detection, got %s", p.stack)
	}
}

// TestDetectionOrder locks the detection precedence: go beats ts beats py
// beats shell. Python sits AFTER TypeScript so a mixed repo with both a
// package.json and a pyproject.toml resolves to ts, not py.
func TestDetectionOrder(t *testing.T) {
	// go.mod present alongside a py marker → go wins.
	goRoot := t.TempDir()
	writeMarker(t, goRoot, "go.mod", "module x\n")
	writeMarker(t, goRoot, "pyproject.toml", "")
	if p := planStarter(goRoot); p.stack != "go" {
		t.Fatalf("go.mod must win over py marker, got %s", p.stack)
	}

	// package.json + pyproject.toml → ts wins (py is after ts).
	tsRoot := t.TempDir()
	writeMarker(t, tsRoot, "package.json", "{}")
	writeMarker(t, tsRoot, "pyproject.toml", "")
	if p := planStarter(tsRoot); p.stack != "ts" {
		t.Fatalf("package.json must win over py marker, got %s", p.stack)
	}
}

// TestPyFloorFromPyproject proves coverage.py fail_under is read from
// pyproject.toml [tool.coverage.report] and seeded as the coverage floor.
func TestPyFloorFromPyproject(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "pyproject.toml", `[build-system]
requires = ["setuptools"]

[tool.coverage.report]
show_missing = true
fail_under = 85
`)
	p := planStarter(root)
	if p.stack != "py" {
		t.Fatalf("stack: got %s", p.stack)
	}
	if !strings.Contains(p.body, "floor: 85") {
		t.Fatalf("body should seed floor 85:\n%s", p.body)
	}
	if !strings.Contains(p.coverageLine, "inferred floor=85") || !strings.Contains(p.coverageLine, "pyproject.toml") {
		t.Fatalf("coverage summary should cite pyproject: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestPyFloorFromSetupCfg proves the setup.cfg [coverage:report] fallback
// when there is no pyproject.toml.
func TestPyFloorFromSetupCfg(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "setup.cfg", `[metadata]
name = thing

[coverage:report]
fail_under = 72
`)
	p := planStarter(root)
	if !strings.Contains(p.body, "floor: 72") {
		t.Fatalf("body should seed floor 72 from setup.cfg:\n%s", p.body)
	}
	if !strings.Contains(p.coverageLine, "inferred floor=72") || !strings.Contains(p.coverageLine, "setup.cfg") {
		t.Fatalf("coverage summary should cite setup.cfg: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestPyFloorPyprojectWinsOverSetupCfg proves pyproject.toml is preferred
// when both carry a fail_under.
func TestPyFloorPyprojectWinsOverSetupCfg(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "pyproject.toml", "[tool.coverage.report]\nfail_under = 90\n")
	writeMarker(t, root, "setup.cfg", "[coverage:report]\nfail_under = 50\n")
	if floor, src, ok := inferPyFloor(root); !ok || floor != 90 || !strings.Contains(src, "pyproject.toml") {
		t.Fatalf("pyproject should win: floor=%d src=%q ok=%v", floor, src, ok)
	}
}

// TestPyFloorFractionalRounds documents the scope call: coverage.py accepts
// a float fail_under but the gate floor is an integer percent, so a
// fractional value rounds to the nearest whole percent.
func TestPyFloorFractionalRounds(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "pyproject.toml", "[tool.coverage.report]\nfail_under = 84.6\n")
	if floor, _, ok := inferPyFloor(root); !ok || floor != 85 {
		t.Fatalf("84.6 should round to 85: floor=%d ok=%v", floor, ok)
	}
}

// TestPyNoFloorWhenAbsent proves the conservative default: a Python repo
// with no coverage.py fail_under gets NO floor (init does not invent one).
func TestPyNoFloorWhenAbsent(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "requirements.txt", "pytest\n")
	p := planStarter(root)
	if p.stack != "py" {
		t.Fatalf("stack: got %s", p.stack)
	}
	if strings.Contains(p.body, "floor:") {
		t.Fatalf("no fail_under → no floor should be emitted:\n%s", p.body)
	}
	if !strings.Contains(p.coverageLine, "omitted") {
		t.Fatalf("summary should say the floor was omitted: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestPyFailUnderIgnoresWrongSection proves the section-aware scan does not
// pick up a fail_under that lives outside the coverage report section.
func TestPyFailUnderIgnoresWrongSection(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "pyproject.toml", `[tool.other]
fail_under = 99

[tool.coverage.run]
branch = true
`)
	if _, _, ok := inferPyFloor(root); ok {
		t.Fatalf("fail_under outside [tool.coverage.report] must be ignored")
	}
}

// TestTsJestThresholdInference proves jest coverageThreshold.global is read
// from package.json and seeded into the ts coverage check with runner=jest.
func TestTsJestThresholdInference(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "package.json", `{
  "name": "app",
  "jest": {
    "coverageThreshold": {
      "global": { "statements": 80, "branches": 70, "functions": 85, "lines": 82 }
    }
  }
}`)
	p := planStarter(root)
	if p.stack != "ts" {
		t.Fatalf("stack: got %s", p.stack)
	}
	if !strings.Contains(p.body, "runner: jest") {
		t.Fatalf("body should set runner jest:\n%s", p.body)
	}
	for _, want := range []string{"statements: 80", "branches: 70", "functions: 85", "lines: 82"} {
		if !strings.Contains(p.body, want) {
			t.Fatalf("body missing %q:\n%s", want, p.body)
		}
	}
	if !strings.Contains(p.coverageLine, "inferred jest thresholds") {
		t.Fatalf("summary should note jest inference: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestTsJestPartialThresholds proves only the present metrics are seeded
// (a metric with no jest entry is not gated).
func TestTsJestPartialThresholds(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "package.json", `{"jest":{"coverageThreshold":{"global":{"lines": 90}}}}`)
	p := planStarter(root)
	if !strings.Contains(p.body, "lines: 90") {
		t.Fatalf("body should seed lines: 90:\n%s", p.body)
	}
	if strings.Contains(p.body, "statements:") || strings.Contains(p.body, "branches:") {
		t.Fatalf("absent metrics must not be seeded:\n%s", p.body)
	}
	assertLoads(t, p.body)
}

// TestTsNoJestThresholds proves the not-found path: no jest coverageThreshold
// (or a bare package.json) leaves the ts coverage check un-thresholded and
// says a vitest config is not parseable.
func TestTsNoJestThresholds(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "package.json", `{"name":"app","scripts":{"test":"vitest"}}`)
	p := planStarter(root)
	if p.stack != "ts" {
		t.Fatalf("stack: got %s", p.stack)
	}
	if strings.Contains(p.body, "runner: jest") || strings.Contains(p.body, "thresholds: {") {
		t.Fatalf("no jest config → no inferred thresholds:\n%s", p.body)
	}
	if !strings.Contains(p.coverageLine, "not parseable") {
		t.Fatalf("summary should explain vitest is not parseable: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestTsMalformedPackageJSON proves a syntactically broken package.json is
// treated as "no thresholds inferred" rather than crashing init.
func TestTsMalformedPackageJSON(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "package.json", `{"jest": { this is not json `)
	p := planStarter(root)
	if p.stack != "ts" {
		t.Fatalf("still ts by package.json presence, got %s", p.stack)
	}
	if strings.Contains(p.body, "thresholds: {") {
		t.Fatalf("malformed json must not seed thresholds:\n%s", p.body)
	}
	assertLoads(t, p.body)
}

// TestGoStarterHasNoCoverageFloor proves init does not invent a Go coverage
// number (Go has no standard on-disk floor location).
func TestGoStarterHasNoCoverageFloor(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "go.mod", "module x\n")
	p := planStarter(root)
	if strings.Contains(p.body, "floor:") || strings.Contains(p.body, "coverage:") {
		t.Fatalf("go starter must not carry a coverage floor:\n%s", p.body)
	}
	if !strings.Contains(p.coverageLine, "no standard Go coverage-floor") {
		t.Fatalf("go summary should explain the omission: %q", p.coverageLine)
	}
	assertLoads(t, p.body)
}

// TestDetectionSummaryPrinted proves init prints the detection summary
// (stack, wired checks, coverage inference) when it writes a starter.
func TestDetectionSummaryPrinted(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root, "pyproject.toml", "[tool.coverage.report]\nfail_under = 66\n")
	g := mainCheckoutGit(root)
	var out bytes.Buffer
	if err := initRepo(root, g.fn, &out); err != nil {
		t.Fatalf("initRepo: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"detected stack=py",
		"wired checks: format, lint, coverage",
		"coverage: inferred floor=66 from pyproject.toml",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q:\n%s", want, s)
		}
	}
	// The summary is NOT printed on the idempotent keep path.
	out.Reset()
	if err := initRepo(root, mainCheckoutGit(root).fn, &out); err != nil {
		t.Fatalf("second initRepo: %v", err)
	}
	if strings.Contains(out.String(), "detected stack=") {
		t.Fatalf("summary must not print when gate.yml is kept:\n%s", out.String())
	}
}

// TestAllStartersLoad proves every stack's generated starter is a valid,
// loadable gate.yml — go, ts (jest + none), py (floor + none), shell.
func TestAllStartersLoad(t *testing.T) {
	cases := map[string]func(root string){
		"go": func(r string) { writeMarker(t, r, "go.mod", "module x\n") },
		"ts-jest": func(r string) {
			writeMarker(t, r, "package.json", `{"jest":{"coverageThreshold":{"global":{"statements":80}}}}`)
		},
		"ts-none":  func(r string) { writeMarker(t, r, "package.json", "{}") },
		"py-floor": func(r string) { writeMarker(t, r, "pyproject.toml", "[tool.coverage.report]\nfail_under = 70\n") },
		"py-none":  func(r string) { writeMarker(t, r, "requirements.txt", "pytest\n") },
		"shell":    func(r string) {},
	}
	for name, setup := range cases {
		root := t.TempDir()
		setup(root)
		assertLoads(t, planStarter(root).body)
		_ = name
	}
}
