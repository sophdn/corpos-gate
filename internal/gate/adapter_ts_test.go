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

// tsCfg builds a single-unit ts Config with the given checks. unit
// carries an optional tsconfig list for the static check.
func tsCfg(checks map[string]CheckConfig, tsconfig ...string) *Config {
	return &Config{
		Stack:  "ts",
		Units:  []Unit{{Dir: "app", Tsconfig: tsconfig}},
		Checks: checks,
	}
}

// reportsDirFromArgs extracts the coverage reports directory the check
// asks the runner to write into (vitest's --coverage.reportsDirectory=
// or jest's --coverageDirectory=), so a fake runner can drop a summary
// file there as a side effect.
func reportsDirFromArgs(args []string) string {
	for _, a := range args {
		for _, pfx := range []string{"--coverage.reportsDirectory=", "--coverageDirectory="} {
			if strings.HasPrefix(a, pfx) {
				return strings.TrimPrefix(a, pfx)
			}
		}
	}
	return ""
}

// writeSummaryResponder returns a fakeRunner respond func that, on the
// coverage runner invocation, writes an istanbul json-summary with the
// given per-metric pcts into the reports dir the check requested, then
// returns exit 0.
func writeSummaryResponder(t *testing.T, statements, branches, functions, lines float64) func(string, []string) (string, int, error) {
	t.Helper()
	return func(_ string, args []string) (string, int, error) {
		dir := reportsDirFromArgs(args)
		if dir == "" {
			return "", 0, nil
		}
		body := coverageSummary{Total: coverageTotals{
			Statements: coverageMetric{Pct: statements},
			Branches:   coverageMetric{Pct: branches},
			Functions:  coverageMetric{Pct: functions},
			Lines:      coverageMetric{Pct: lines},
		}}
		data, err := json.Marshal(body)
		if err != nil {
			return "", 0, err
		}
		if err := os.WriteFile(filepath.Join(dir, "coverage-summary.json"), data, 0o644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}
}

// ── build order + command shapes ────────────────────────────────────────

func TestTSChecksOrderAndEnabled(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{
		"static":   {Enabled: true, Tier: "pre-commit"},
		"lint":     {Enabled: true, Tier: "pre-commit"},
		"build":    {Enabled: false, Tier: "pre-commit"}, // disabled → omitted
		"coverage": {Enabled: true, Tier: "pre-push", Thresholds: map[string]float64{"lines": 80}},
		"mutation": {Enabled: true, Tier: "ci"},
	})
	got := names(TSChecks(cfg))
	want := []string{"static", "lint", "coverage", "mutation"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ts order = %v, want %v", got, want)
	}
}

func TestTSLintConstruction(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, TSChecks(cfg), "lint").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("lint on clean run should pass: %+v", r)
	}
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "app") || call.name != "npx" ||
		strings.Join(call.args, " ") != "--no-install eslint ." {
		t.Fatalf("eslint command wrong: %+v", call)
	}
	// Non-zero exit → FAIL.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "lint error", 1, nil }}
	r2 := findCheck(t, TSChecks(cfg), "lint").Run(context.Background(), f2.env("/repo"))
	if r2.OK {
		t.Fatalf("eslint non-zero should fail: %+v", r2)
	}
}

func TestTSStaticBareTsconfig(t *testing.T) {
	// No tsconfig configured → one bare `tsc --noEmit`.
	cfg := tsCfg(map[string]CheckConfig{"static": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, TSChecks(cfg), "static").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("static clean should pass: %+v", r)
	}
	if len(f.calls) != 1 || f.calls[0].name != "npx" ||
		strings.Join(f.calls[0].args, " ") != "--no-install tsc --noEmit" {
		t.Fatalf("bare tsc command wrong: %+v", f.calls)
	}
}

func TestTSStaticPerTsconfig(t *testing.T) {
	// Two tsconfigs → `tsc -p <cfg> --noEmit` once each.
	cfg := tsCfg(map[string]CheckConfig{"static": {Enabled: true, Tier: "pre-commit"}},
		"tsconfig.json", "tsconfig.node.json")
	f := &fakeRunner{}
	r := findCheck(t, TSChecks(cfg), "static").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("static clean should pass: %+v", r)
	}
	if len(f.calls) != 2 {
		t.Fatalf("static should run once per tsconfig (2), got %d: %+v", len(f.calls), f.calls)
	}
	if strings.Join(f.calls[0].args, " ") != "--no-install tsc -p tsconfig.json --noEmit" {
		t.Fatalf("static call 0 wrong: %+v", f.calls[0])
	}
	if strings.Join(f.calls[1].args, " ") != "--no-install tsc -p tsconfig.node.json --noEmit" {
		t.Fatalf("static call 1 wrong: %+v", f.calls[1])
	}
	// A non-zero on the FIRST tsconfig fails and names it, without running the second.
	f2 := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if containsArg(args, "tsconfig.json") {
			return "type error", 2, nil
		}
		return "", 0, nil
	}}
	r2 := findCheck(t, TSChecks(cfg), "static").Run(context.Background(), f2.env("/repo"))
	if r2.OK || !strings.Contains(r2.Output, "tsconfig.json") {
		t.Fatalf("static should fail naming the bad tsconfig: %+v", r2)
	}
	if len(f2.calls) != 1 {
		t.Fatalf("static should stop at the first failing tsconfig (1 call), got %d", len(f2.calls))
	}
}

func TestTSBuildConstruction(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"build": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	r := findCheck(t, TSChecks(cfg), "build").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("build clean should pass: %+v", r)
	}
	if f.calls[0].name != "npm" || strings.Join(f.calls[0].args, " ") != "run build" {
		t.Fatalf("build command wrong: %+v", f.calls[0])
	}
	// Non-zero → FAIL.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "build broke", 1, nil }}
	if findCheck(t, TSChecks(cfg), "build").Run(context.Background(), f2.env("/repo")).OK {
		t.Fatalf("build non-zero should fail")
	}
}

// ── coverage: four-metric parse + compare ───────────────────────────────

func TestTSCoverageVitestCommandAndPass(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push",
		Thresholds: map[string]float64{"statements": 80, "branches": 70, "functions": 80, "lines": 80},
	}})
	// All metrics above their thresholds.
	f := &fakeRunner{respond: writeSummaryResponder(t, 85, 75, 90, 88)}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("coverage above all thresholds should pass: %+v", r)
	}
	// Assert the vitest command shape.
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "app") || call.name != "npx" {
		t.Fatalf("coverage should run npx in unit dir: %+v", call)
	}
	joined := strings.Join(call.args, " ")
	for _, want := range []string{"--no-install", "vitest", "run", "--coverage", "--coverage.reporter=json-summary", "--coverage.reportsDirectory="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("vitest args missing %q: %q", want, joined)
		}
	}
}

func TestTSCoveragePerMetricFail(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push",
		Thresholds: map[string]float64{"statements": 80, "branches": 70, "functions": 80, "lines": 80},
	}})
	// branches below its floor, the rest above.
	f := &fakeRunner{respond: writeSummaryResponder(t, 85, 60, 90, 88)}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("coverage with a metric below threshold should fail: %+v", r)
	}
	if !strings.Contains(r.Output, "branches") || !strings.Contains(r.Output, "FAIL") {
		t.Fatalf("coverage failure should name the failing metric: %+v", r.Output)
	}
	// The passing metrics must NOT be reported as failures.
	if strings.Contains(strings.SplitN(r.Output, "(ok:", 2)[0], "statements") {
		t.Fatalf("statements passed and should not be in the FAIL list: %+v", r.Output)
	}
}

func TestTSCoverageUnconfiguredMetricNotGated(t *testing.T) {
	// Only `lines` is thresholded; branches is dismal but ungated → PASS.
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push",
		Thresholds: map[string]float64{"lines": 80},
	}})
	f := &fakeRunner{respond: writeSummaryResponder(t, 10, 5, 10, 85)}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("only-lines-thresholded, lines passing → PASS regardless of others: %+v", r)
	}
	if strings.Contains(r.Output, "branches") || strings.Contains(r.Output, "statements") {
		t.Fatalf("ungated metrics must not appear in the verdict: %+v", r.Output)
	}
}

func TestTSCoverageMissingSummaryFails(t *testing.T) {
	// Runner returns exit 0 but writes NO summary file → check failure
	// with a clear message (not a silent zero-coverage pass/fail).
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Thresholds: map[string]float64{"lines": 80},
	}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 0, nil }}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "coverage summary unreadable") {
		t.Fatalf("missing summary should fail with a clear message: %+v", r)
	}
}

func TestTSCoverageRunFails(t *testing.T) {
	// The runner itself exits non-zero → check failure, summary never read.
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Thresholds: map[string]float64{"lines": 80},
	}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "tests failed", 1, nil }}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "coverage test run failed") {
		t.Fatalf("failing coverage run should fail the check: %+v", r)
	}
}

func TestTSCoverageJestRunner(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Runner: "jest",
		Thresholds: map[string]float64{"lines": 80},
	}})
	f := &fakeRunner{respond: writeSummaryResponder(t, 90, 90, 90, 90)}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("jest coverage above threshold should pass: %+v", r)
	}
	joined := strings.Join(f.calls[0].args, " ")
	for _, want := range []string{"--no-install", "jest", "--coverage", "--coverageReporters=json-summary", "--coverageDirectory="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("jest args missing %q: %q", want, joined)
		}
	}
	// jest must NOT carry vitest-shaped flags.
	if strings.Contains(joined, "vitest") || strings.Contains(joined, "--coverage.reporter") {
		t.Fatalf("jest command leaked vitest flags: %q", joined)
	}
}

func TestTSCoverageDefaultRunnerIsVitest(t *testing.T) {
	// Empty Runner → vitest command.
	args := coverageArgs("", "/tmp/x")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "vitest") || strings.Contains(joined, "jest") {
		t.Fatalf("empty runner should default to vitest: %q", joined)
	}
}

func TestParseCoverageSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "coverage-summary.json")
	if err := os.WriteFile(p, []byte(`{"total":{"lines":{"pct":91.2},"statements":{"pct":88.0},"functions":{"pct":80.0},"branches":{"pct":72.5}}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	totals, err := parseCoverageSummary(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if totals.Lines.Pct != 91.2 || totals.Statements.Pct != 88.0 || totals.Functions.Pct != 80.0 || totals.Branches.Pct != 72.5 {
		t.Fatalf("parsed totals wrong: %+v", totals)
	}
	// Missing file → error.
	if _, err := parseCoverageSummary(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatalf("missing summary should error")
	}
	// Malformed JSON → error.
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := parseCoverageSummary(bad); err == nil {
		t.Fatalf("malformed summary should error")
	}
}

// ── mutation: report-only ───────────────────────────────────────────────

func TestTSMutationReportOnly(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"mutation": {Enabled: true, Tier: "ci"}})

	// stryker not resolvable → OK + Skipped, no command run.
	fAbsent := &fakeRunner{}
	envAbsent := fAbsent.env("/repo")
	envAbsent.LookupTool = func(string) (string, bool) { return "", false }
	rAbsent := findCheck(t, TSChecks(cfg), "mutation").Run(context.Background(), envAbsent)
	if !rAbsent.OK || !rAbsent.Skipped {
		t.Fatalf("mutation with stryker absent should be ok+skip: %+v", rAbsent)
	}
	if len(fAbsent.calls) != 0 {
		t.Fatalf("mutation with stryker absent should run no command, ran %d", len(fAbsent.calls))
	}

	strykerFound := func(string) (string, bool) { return "/opt/stryker", true }

	// Clean run → OK, command shape `npx --no-install stryker run`.
	fOK := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "mutation score 100", 0, nil
	}}
	envOK := fOK.env("/repo")
	envOK.LookupTool = strykerFound
	rOK := findCheck(t, TSChecks(cfg), "mutation").Run(context.Background(), envOK)
	if !rOK.OK || rOK.Skipped {
		t.Fatalf("mutation clean run should be plain OK: %+v", rOK)
	}
	if fOK.calls[0].name != "npx" || strings.Join(fOK.calls[0].args, " ") != "--no-install stryker run" {
		t.Fatalf("stryker command wrong: %+v", fOK.calls[0])
	}

	// Surviving mutants (non-zero) → STILL OK.
	fSurvive := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "2 mutants survived", 1, nil
	}}
	envSurvive := fSurvive.env("/repo")
	envSurvive.LookupTool = strykerFound
	if !findCheck(t, TSChecks(cfg), "mutation").Run(context.Background(), envSurvive).OK {
		t.Fatalf("mutation non-zero exit must stay OK (report-only)")
	}

	// Couldn't start (exec error) → STILL OK, no infra Err surfaced.
	fErr := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("permission denied")
	}}
	envErr := fErr.env("/repo")
	envErr.LookupTool = strykerFound
	rErr := findCheck(t, TSChecks(cfg), "mutation").Run(context.Background(), envErr)
	if !rErr.OK || rErr.Err != nil {
		t.Fatalf("mutation exec error must stay OK with no infra Err: %+v", rErr)
	}
}

// ── race/goleak on a ts unit → SKIP with reason ─────────────────────────

func TestTSRaceGoleakSkipped(t *testing.T) {
	for _, name := range []string{"race", "goleak"} {
		cfg := tsCfg(map[string]CheckConfig{name: {Enabled: true, Tier: "pre-push"}})
		f := &fakeRunner{}
		r := findCheck(t, TSChecks(cfg), name).Run(context.Background(), f.env("/repo"))
		if !r.OK || !r.Skipped {
			t.Fatalf("%s on a ts unit should SKIP+ok: %+v", name, r)
		}
		if !strings.Contains(r.Output, "not applicable to the TypeScript stack") {
			t.Fatalf("%s skip should carry the reason: %+v", name, r.Output)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%s skip should run no command, ran %d", name, len(f.calls))
		}
	}
}

// ── stack dispatch ──────────────────────────────────────────────────────

func TestAdapterChecksStackDispatch(t *testing.T) {
	// stack: ts → TSChecks (npx-driven), never GoChecks.
	tsC := tsCfg(map[string]CheckConfig{
		"lint":   {Enabled: true, Tier: "pre-commit"},
		"static": {Enabled: true, Tier: "pre-commit"},
	})
	got := names(adapterChecks(tsC))
	if strings.Join(got, ",") != "static,lint" {
		t.Fatalf("ts adapter checks = %v, want static,lint", got)
	}
	// Prove it dispatched to the TS adapter, not the Go one: run lint and
	// confirm the command is npx eslint, not a go/gofmt tool.
	f := &fakeRunner{}
	findCheck(t, adapterChecks(tsC), "lint").Run(context.Background(), f.env("/repo"))
	if f.calls[0].name != "npx" {
		t.Fatalf("ts lint should shell out to npx, got %q", f.calls[0].name)
	}

	// stack: shell → no adapter checks; only custom checks run.
	shellC := &Config{
		Stack:  "shell",
		Units:  []Unit{{Dir: "."}},
		Custom: []Custom{{Name: "golden", Cmd: "true", Tier: "pre-commit"}},
	}
	if got := names(adapterChecks(shellC)); len(got) != 0 {
		t.Fatalf("shell stack should have no adapter checks, got %v", got)
	}
	plan := Plan(shellC, TierPreCommit, nil)
	if len(plan) != 1 || plan[0].Name != "golden" {
		t.Fatalf("shell plan should be custom-only: %+v", plan)
	}

	// stack: go → GoChecks (sanity: dispatch still routes go).
	goC := goCfg(map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}})
	if got := names(adapterChecks(goC)); strings.Join(got, ",") != "vet" {
		t.Fatalf("go adapter checks = %v, want vet", got)
	}
}

// ── config: thresholds + runner validation ──────────────────────────────

func TestLoadTSConfigValid(t *testing.T) {
	body := `stack: ts
units:
  - dir: app
    tsconfig: [tsconfig.json, tsconfig.node.json]
checks:
  static:   { enabled: true, tier: pre-commit }
  lint:     { enabled: true, tier: pre-commit }
  coverage: { enabled: true, tier: pre-push, runner: jest, thresholds: { statements: 80, branches: 70, functions: 80, lines: 80 } }
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("valid ts config should Load: %v", err)
	}
	if len(cfg.Units[0].Tsconfig) != 2 {
		t.Fatalf("tsconfig not parsed: %+v", cfg.Units[0])
	}
	cc := cfg.Checks["coverage"]
	if cc.Runner != "jest" || cc.Thresholds["branches"] != 70 || len(cc.Thresholds) != 4 {
		t.Fatalf("coverage runner/thresholds not parsed: %+v", cc)
	}
}

func TestLoadTSConfigInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown metric": "stack: ts\nunits:\n  - dir: app\nchecks:\n  coverage: { enabled: true, tier: pre-push, thresholds: { bogus: 80 } }\n",
		"unknown runner": "stack: ts\nunits:\n  - dir: app\nchecks:\n  coverage: { enabled: true, tier: pre-push, runner: mocha }\n",
	}
	for name, body := range cases {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected Load error, got nil", name)
		}
	}
}
