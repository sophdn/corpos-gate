package gate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pyCfg builds a single-unit py Config with the given checks.
func pyCfg(checks map[string]CheckConfig) *Config {
	return &Config{
		Stack:  "py",
		Units:  []Unit{{Dir: "training"}},
		Checks: checks,
	}
}

// found returns a LookupTool that resolves the named tools (and only
// those) to a fake path, so a test can assert the tool-present path
// without depending on what's installed on the machine running the suite.
func found(tools ...string) func(string) (string, bool) {
	set := make(map[string]bool, len(tools))
	for _, t := range tools {
		set[t] = true
	}
	return func(name string) (string, bool) {
		if set[name] {
			return "/opt/" + name, true
		}
		return "", false
	}
}

// jsonPathFromArgs extracts the coverage.py json report path the check
// asks the runner to write (`--cov-report=json:<path>`), so a fake runner
// can drop a report file there as a side effect.
func jsonPathFromArgs(args []string) string {
	const pfx = "--cov-report=json:"
	for _, a := range args {
		if strings.HasPrefix(a, pfx) {
			return strings.TrimPrefix(a, pfx)
		}
	}
	return ""
}

// writePyCoverageResponder returns a fakeRunner respond func that, on the
// pytest invocation, writes a coverage.py json report with the given
// percent_covered into the path the check requested, then returns exit 0.
func writePyCoverageResponder(t *testing.T, percentCovered float64) func(string, []string) (string, int, error) {
	t.Helper()
	return func(_ string, args []string) (string, int, error) {
		path := jsonPathFromArgs(args)
		if path == "" {
			return "", 0, nil
		}
		body := pyCoverageReport{Totals: pyCoverageTotals{PercentCovered: percentCovered}}
		data, err := json.Marshal(body)
		if err != nil {
			return "", 0, err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}
}

// pyEnvWith wraps a fakeRunner env, overriding LookupTool so the resolver
// is hermetic (independent of what's installed on the test host).
func pyEnvWith(f *fakeRunner, tools ...string) RunEnv {
	env := f.env("/repo")
	env.LookupTool = found(tools...)
	return env
}

// ── build order + command shapes ────────────────────────────────────────

func TestPyChecksOrderAndEnabled(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{
		"format":   {Enabled: true, Tier: "pre-commit"},
		"lint":     {Enabled: true, Tier: "pre-commit"},
		"static":   {Enabled: false, Tier: "pre-commit"}, // disabled → omitted
		"coverage": {Enabled: true, Tier: "pre-push", Floor: 80},
		"mutation": {Enabled: true, Tier: "ci"},
	})
	got := names(PyChecks(cfg))
	want := []string{"format", "lint", "coverage", "mutation"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("py order = %v, want %v", got, want)
	}
}

func TestPyLintConstruction(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, PyChecks(cfg), "lint").Run(context.Background(), pyEnvWith(f, "ruff"))
	if !r.OK {
		t.Fatalf("lint on clean run should pass: %+v", r)
	}
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "training") || call.name != "ruff" ||
		strings.Join(call.args, " ") != "check ." {
		t.Fatalf("ruff check command wrong: %+v", call)
	}
	// Non-zero exit → FAIL.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "lint error", 1, nil }}
	r2 := findCheck(t, PyChecks(cfg), "lint").Run(context.Background(), pyEnvWith(f2, "ruff"))
	if r2.OK {
		t.Fatalf("ruff check non-zero should fail: %+v", r2)
	}
}

func TestPyFormatConstruction(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"format": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, PyChecks(cfg), "format").Run(context.Background(), pyEnvWith(f, "ruff"))
	if !r.OK {
		t.Fatalf("format on clean run should pass: %+v", r)
	}
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "training") || call.name != "ruff" ||
		strings.Join(call.args, " ") != "format --check ." {
		t.Fatalf("ruff format command wrong: %+v", call)
	}
	// Formatting drift (non-zero) → FAIL, output names the drift.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "would reformat x.py", 1, nil }}
	r2 := findCheck(t, PyChecks(cfg), "format").Run(context.Background(), pyEnvWith(f2, "ruff"))
	if r2.OK || !strings.Contains(r2.Output, "drift") {
		t.Fatalf("ruff format non-zero should fail with a drift note: %+v", r2)
	}
}

func TestPyStaticConstruction(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"static": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, PyChecks(cfg), "static").Run(context.Background(), pyEnvWith(f, "mypy"))
	if !r.OK {
		t.Fatalf("static clean should pass: %+v", r)
	}
	call := f.calls[0]
	if call.name != "mypy" || strings.Join(call.args, " ") != "." {
		t.Fatalf("mypy command wrong: %+v", call)
	}
	// Non-zero → FAIL.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "type error", 1, nil }}
	if findCheck(t, PyChecks(cfg), "static").Run(context.Background(), pyEnvWith(f2, "mypy")).OK {
		t.Fatalf("mypy non-zero should fail")
	}
}

// ── coverage: scalar floor parse + compare ──────────────────────────────

func TestPyCoverageCommandAndPass(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 80, Scope: "training",
	}})
	f := &fakeRunner{respond: writePyCoverageResponder(t, 85.0)}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if !r.OK {
		t.Fatalf("coverage above floor should pass: %+v", r)
	}
	if !strings.Contains(r.Output, "85.0%") || !strings.Contains(r.Output, ">=") {
		t.Fatalf("coverage pass verdict should show measured vs floor: %+v", r.Output)
	}
	// Assert the pytest-cov command shape.
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "training") || call.name != "python" {
		t.Fatalf("coverage should run python in the unit dir: %+v", call)
	}
	joined := strings.Join(call.args, " ")
	for _, want := range []string{"-m pytest", "--cov=training", "--cov-report=json:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("pytest args missing %q: %q", want, joined)
		}
	}
}

func TestPyCoverageBelowFloorFails(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 80,
	}})
	f := &fakeRunner{respond: writePyCoverageResponder(t, 71.5)}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if r.OK {
		t.Fatalf("coverage below floor should fail: %+v", r)
	}
	if !strings.Contains(r.Output, "71.5%") || !strings.Contains(r.Output, "<") {
		t.Fatalf("coverage fail verdict should show measured vs floor: %+v", r.Output)
	}
}

func TestPyCoverageDefaultScope(t *testing.T) {
	// Empty scope → a single `--cov=.` target.
	cfg := pyCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 50,
	}})
	f := &fakeRunner{respond: writePyCoverageResponder(t, 60.0)}
	if !findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest")).OK {
		t.Fatalf("default-scope coverage above floor should pass")
	}
	if !containsArg(f.calls[0].args, "--cov=.") {
		t.Fatalf("empty scope should default to --cov=. : %+v", f.calls[0].args)
	}
}

func TestPyCoverageMultiTargetScope(t *testing.T) {
	// Space/comma-separated scope → one --cov=X per target.
	cfg := pyCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 50, Scope: "training, export eval",
	}})
	f := &fakeRunner{respond: writePyCoverageResponder(t, 60.0)}
	findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	for _, want := range []string{"--cov=training", "--cov=export", "--cov=eval"} {
		if !containsArg(f.calls[0].args, want) {
			t.Fatalf("multi-target scope missing %q: %+v", want, f.calls[0].args)
		}
	}
}

func TestPyCoverageMissingJSONFails(t *testing.T) {
	// Runner returns exit 0 but writes NO json report → check failure with
	// a clear message (not a silent zero-coverage pass/fail).
	cfg := pyCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 80}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 0, nil }}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if r.OK || !strings.Contains(r.Output, "coverage json unreadable") {
		t.Fatalf("missing json should fail with a clear message: %+v", r)
	}
}

func TestPyCoverageMalformedJSONFails(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 80}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if p := jsonPathFromArgs(args); p != "" {
			_ = os.WriteFile(p, []byte("{not json"), 0o644)
		}
		return "", 0, nil
	}}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if r.OK || !strings.Contains(r.Output, "coverage json unreadable") {
		t.Fatalf("malformed json should fail with a clear message: %+v", r)
	}
}

func TestPyCoverageRunFails(t *testing.T) {
	// The pytest run itself exits non-zero → check failure, json never read.
	cfg := pyCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 80}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "tests failed", 1, nil }}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if r.OK || !strings.Contains(r.Output, "coverage test run failed") {
		t.Fatalf("failing coverage run should fail the check: %+v", r)
	}
}

func TestParsePyCoverage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "coverage.json")
	if err := os.WriteFile(p, []byte(`{"totals":{"percent_covered":73.4,"num_statements":100}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	pct, err := parsePyCoverage(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pct != 73.4 {
		t.Fatalf("parsed percent_covered = %v, want 73.4", pct)
	}
	// Missing file → error.
	if _, err := parsePyCoverage(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatalf("missing json should error")
	}
	// Malformed JSON → error.
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := parsePyCoverage(bad); err == nil {
		t.Fatalf("malformed json should error")
	}
}

// ── mutation: report-only ───────────────────────────────────────────────

func TestPyMutationReportOnly(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"mutation": {Enabled: true, Tier: "ci"}})

	// mutmut not resolvable → OK + Skipped, no command run.
	fAbsent := &fakeRunner{}
	rAbsent := findCheck(t, PyChecks(cfg), "mutation").Run(context.Background(), pyEnvWith(fAbsent))
	if !rAbsent.OK || !rAbsent.Skipped {
		t.Fatalf("mutation with mutmut absent should be ok+skip: %+v", rAbsent)
	}
	if len(fAbsent.calls) != 0 {
		t.Fatalf("mutation with mutmut absent should run no command, ran %d", len(fAbsent.calls))
	}

	// Clean run → OK, command shape `mutmut run`.
	fOK := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "no surviving mutants", 0, nil
	}}
	rOK := findCheck(t, PyChecks(cfg), "mutation").Run(context.Background(), pyEnvWith(fOK, "mutmut"))
	if !rOK.OK || rOK.Skipped {
		t.Fatalf("mutation clean run should be plain OK: %+v", rOK)
	}
	if fOK.calls[0].name != "mutmut" || strings.Join(fOK.calls[0].args, " ") != "run" {
		t.Fatalf("mutmut command wrong: %+v", fOK.calls[0])
	}

	// Surviving mutants (non-zero) → STILL OK.
	fSurvive := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "2 mutants survived", 1, nil
	}}
	if !findCheck(t, PyChecks(cfg), "mutation").Run(context.Background(), pyEnvWith(fSurvive, "mutmut")).OK {
		t.Fatalf("mutation non-zero exit must stay OK (report-only)")
	}

	// Couldn't start (exec error) → STILL OK, no infra Err surfaced.
	fErr := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("permission denied")
	}}
	rErr := findCheck(t, PyChecks(cfg), "mutation").Run(context.Background(), pyEnvWith(fErr, "mutmut"))
	if !rErr.OK || rErr.Err != nil {
		t.Fatalf("mutation exec error must stay OK with no infra Err: %+v", rErr)
	}
}

// ── tool absent → SKIP with reason ──────────────────────────────────────

func TestPyToolsSkipWhenAbsent(t *testing.T) {
	// With NO tools resolvable, each tool-backed check SKIPs with a reason
	// and runs no command.
	cases := []struct {
		check  string
		cc     CheckConfig
		reason string
	}{
		{"format", CheckConfig{Enabled: true, Tier: "pre-commit"}, "ruff not installed"},
		{"lint", CheckConfig{Enabled: true, Tier: "pre-commit"}, "ruff not installed"},
		{"static", CheckConfig{Enabled: true, Tier: "pre-commit"}, "mypy not resolvable"},
		{"coverage", CheckConfig{Enabled: true, Tier: "pre-push", Floor: 80}, "pytest not resolvable"},
		{"mutation", CheckConfig{Enabled: true, Tier: "ci"}, "mutmut not resolvable"},
	}
	for _, c := range cases {
		cfg := pyCfg(map[string]CheckConfig{c.check: c.cc})
		f := &fakeRunner{}
		r := findCheck(t, PyChecks(cfg), c.check).Run(context.Background(), pyEnvWith(f)) // no tools resolvable
		if !r.OK || !r.Skipped {
			t.Fatalf("%s with tool absent should be ok+skip: %+v", c.check, r)
		}
		if !strings.Contains(r.Output, c.reason) {
			t.Fatalf("%s skip should carry reason %q: %+v", c.check, c.reason, r.Output)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%s skip should run no command, ran %d", c.check, len(f.calls))
		}
	}
}

// ── race/goleak on a py unit → SKIP with reason ─────────────────────────

func TestPyRaceGoleakSkipped(t *testing.T) {
	for _, name := range []string{"race", "goleak"} {
		cfg := pyCfg(map[string]CheckConfig{name: {Enabled: true, Tier: "pre-push"}})
		f := &fakeRunner{}
		r := findCheck(t, PyChecks(cfg), name).Run(context.Background(), f.env("/repo"))
		if !r.OK || !r.Skipped {
			t.Fatalf("%s on a py unit should SKIP+ok: %+v", name, r)
		}
		if !strings.Contains(r.Output, "not applicable to the Python stack") {
			t.Fatalf("%s skip should carry the reason: %+v", name, r.Output)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%s skip should run no command, ran %d", name, len(f.calls))
		}
	}
}

// ── stack dispatch ──────────────────────────────────────────────────────

func TestAdapterChecksPyStackDispatch(t *testing.T) {
	// stack: py → PyChecks (ruff/mypy/pytest-driven), never GoChecks/TSChecks.
	cfg := pyCfg(map[string]CheckConfig{
		"format": {Enabled: true, Tier: "pre-commit"},
		"lint":   {Enabled: true, Tier: "pre-commit"},
	})
	got := names(adapterChecks(cfg))
	if strings.Join(got, ",") != "format,lint" {
		t.Fatalf("py adapter checks = %v, want format,lint", got)
	}
	// Prove it dispatched to the Py adapter: run lint and confirm the
	// command is `ruff check`, not a go/npx tool.
	f := &fakeRunner{}
	findCheck(t, adapterChecks(cfg), "lint").Run(context.Background(), pyEnvWith(f, "ruff"))
	if f.calls[0].name != "ruff" {
		t.Fatalf("py lint should shell out to ruff, got %q", f.calls[0].name)
	}
}

// ── config: py stack loads ──────────────────────────────────────────────

func TestLoadPyConfigValid(t *testing.T) {
	body := `stack: py
units:
  - dir: training
checks:
  format:   { enabled: true, tier: pre-commit }
  lint:     { enabled: true, tier: pre-commit }
  static:   { enabled: false, tier: pre-commit }
  coverage: { enabled: true, tier: pre-push, floor: 80, scope: "training export eval data" }
  mutation: { enabled: false, tier: ci }
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("valid py config should Load: %v", err)
	}
	if cfg.Stack != "py" {
		t.Fatalf("stack not parsed: %+v", cfg)
	}
	cc := cfg.Checks["coverage"]
	if cc.Floor != 80 || cc.Scope != "training export eval data" {
		t.Fatalf("coverage floor/scope not parsed: %+v", cc)
	}
}
