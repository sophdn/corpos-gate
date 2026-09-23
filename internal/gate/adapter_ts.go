package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tsCheckOrder is the stable execution order for the TypeScript-stack
// checks — a fast→slow ordering: the typecheck + lint pair (pre-commit
// tier in a typical config), then build, then the slower coverage and
// mutation runs. `race`/`goleak` trail at the end: they are go-only
// concerns that SKIP-with-reason if a ts unit names them.
var tsCheckOrder = []string{
	"static", "lint", "build", "coverage", "mutation", "race", "goleak",
}

// TSChecks builds one Check per ENABLED ts-stack check in cfg, in the
// canonical tsCheckOrder, run in each unit's dir via the injected
// Runner. When more than one unit is configured, each check name is
// suffixed with the unit dir so results stay distinguishable — mirroring
// GoChecks.
func TSChecks(cfg *Config) []Check {
	var checks []Check
	multi := len(cfg.Units) > 1
	for _, name := range tsCheckOrder {
		cc, ok := cfg.Checks[name]
		if !ok || !cc.Enabled {
			continue
		}
		tier := tierOf(cc.Tier)
		for _, unit := range cfg.Units {
			label := name
			if multi {
				label = name + ":" + unit.Dir
			}
			if c := buildTSCheck(name, label, tier, cc, unit); c != nil {
				checks = append(checks, c)
			}
		}
	}
	return checks
}

// buildTSCheck constructs the Check for one (checkName, unit) pair.
func buildTSCheck(name, label string, tier Tier, cc CheckConfig, unit Unit) Check {
	switch name {
	case "lint":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runTSLint(ctx, env, label, tier, unit)
		}}
	case "static":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runTSStatic(ctx, env, label, tier, unit)
		}}
	case "build":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runTSBuild(ctx, env, label, tier, unit)
		}}
	case "coverage":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runTSCoverage(ctx, env, label, tier, unit, cc)
		}}
	case "mutation":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runTSMutation(ctx, env, label, tier, unit)
		}}
	case "race", "goleak":
		return funcCheck{label, tier, func(_ context.Context, _ RunEnv) Result {
			return runTSNotApplicable(label, tier)
		}}
	}
	return nil
}

// npxArgs prefixes `--no-install <tool>` so a missing tool FAILS loudly
// rather than auto-installing.
func npxArgs(tool string, rest ...string) []string {
	return append([]string{"--no-install", tool}, rest...)
}

// runTSLint runs `npx --no-install eslint .` in the unit dir and FAILS on
// non-zero exit.
func runTSLint(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	out, code, err := env.runner()(ctx, unitDir(env, unit), "npx", npxArgs("eslint", ".")...)
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// runTSStatic runs the typecheck: `npx --no-install tsc -p <cfg> --noEmit`
// once per configured tsconfig (unit.Tsconfig), or a single bare
// `npx --no-install tsc --noEmit` when none are configured. It FAILS at
// the first non-zero exit, naming the tsconfig that failed.
func runTSStatic(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) (res Result) {
	start := time.Now()
	res = Result{Name: name, Tier: tier}
	defer func() { res.Duration = time.Since(start) }()
	dir := unitDir(env, unit)

	if len(unit.Tsconfig) == 0 {
		out, code, err := env.runner()(ctx, dir, "npx", npxArgs("tsc", "--noEmit")...)
		if err != nil {
			res.Err = err
			res.Output = out
			return res
		}
		res.OK = code == 0
		if code != 0 {
			res.Output = "tsc --noEmit failed:\n" + out
		}
		return res
	}

	var combined strings.Builder
	for _, tc := range unit.Tsconfig {
		out, code, err := env.runner()(ctx, dir, "npx", npxArgs("tsc", "-p", tc, "--noEmit")...)
		if err != nil {
			res.Err = err
			res.Output = out
			return res
		}
		if code != 0 {
			res.OK = false
			res.Output = fmt.Sprintf("tsc -p %s --noEmit failed:\n%s", tc, out)
			return res
		}
		combined.WriteString(out)
	}
	res.OK = true
	res.Output = combined.String()
	return res
}

// runTSBuild runs `npm run build` in the unit dir and FAILS on non-zero
// exit. The build check is optional (enabled per gate.yml).
func runTSBuild(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	out, code, err := env.runner()(ctx, unitDir(env, unit), "npm", "run", "build")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// runTSCoverage runs the configured coverage test runner (vitest or jest)
// in the unit dir, writing an istanbul json-summary into a temp reports
// directory, then reads `<tmp>/coverage-summary.json` and compares its
// `total.{statements,branches,functions,lines}.pct` to the check's
// per-metric Thresholds. It FAILS if any configured metric is below its
// threshold; a metric with no configured threshold is not gated. The
// temp directory is removed before return.
func runTSCoverage(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, cc CheckConfig) (res Result) {
	start := time.Now()
	res = Result{Name: name, Tier: tier}
	defer func() { res.Duration = time.Since(start) }()

	tmpDir, err := os.MkdirTemp("", "corpos-gate-ts-cover-*")
	if err != nil {
		res.Err = fmt.Errorf("create coverage temp dir: %w", err)
		return res
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	out, code, err := env.runner()(ctx, unitDir(env, unit), "npx", coverageArgs(cc.Runner, tmpDir)...)
	if err != nil {
		res.Err = err
		res.Output = out
		return res
	}
	if code != 0 {
		res.OK = false
		res.Output = "coverage test run failed:\n" + out
		return res
	}

	totals, perr := parseCoverageSummary(filepath.Join(tmpDir, "coverage-summary.json"))
	if perr != nil {
		res.OK = false
		res.Output = fmt.Sprintf("coverage summary unreadable: %v", perr)
		return res
	}
	res.OK, res.Output = compareThresholds(totals, cc.Thresholds)
	return res
}

// coverageArgs builds the `npx` args for the configured coverage runner,
// pointing its json-summary reporter at reportsDir. vitest is the default
// when runner is empty. Both runners emit the same
// `<reportsDir>/coverage-summary.json`.
func coverageArgs(runner, reportsDir string) []string {
	switch runner {
	case "jest":
		return npxArgs("jest",
			"--coverage",
			"--coverageReporters=json-summary",
			"--coverageDirectory="+reportsDir)
	default: // "" or "vitest"
		return npxArgs("vitest", "run",
			"--coverage",
			"--coverage.reporter=json-summary",
			"--coverage.reportsDirectory="+reportsDir)
	}
}

// coverageSummary mirrors the istanbul json-summary shape emitted by both
// vitest and jest: a `total` block of per-metric percentages. Typed
// structs (not bare any) so the four metrics are named and checked.
type coverageSummary struct {
	Total coverageTotals `json:"total"`
}

type coverageTotals struct {
	Lines      coverageMetric `json:"lines"`
	Statements coverageMetric `json:"statements"`
	Functions  coverageMetric `json:"functions"`
	Branches   coverageMetric `json:"branches"`
}

type coverageMetric struct {
	Pct float64 `json:"pct"`
}

// coverageMetricOrder is the stable order the four metrics are reported
// in, so a coverage verdict string is deterministic.
var coverageMetricOrder = []string{"statements", "branches", "functions", "lines"}

// parseCoverageSummary reads and decodes a vitest/jest json-summary file.
// A missing or malformed file is an error (surfaced by the caller as a
// check failure) rather than a silent zero.
func parseCoverageSummary(path string) (coverageTotals, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return coverageTotals{}, fmt.Errorf("read coverage summary %s: %w", path, err)
	}
	var s coverageSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return coverageTotals{}, fmt.Errorf("parse coverage summary %s: %w", path, err)
	}
	return s.Total, nil
}

// compareThresholds checks each configured metric's measured pct against
// its floor, returning (ok, verdict). Metrics absent from thresholds are
// not gated. With no thresholds at all the check passes (nothing to
// gate). The verdict names every failing metric (and, when some fail,
// the passing ones too) for a legible report.
func compareThresholds(totals coverageTotals, thresholds map[string]float64) (bool, string) {
	if len(thresholds) == 0 {
		return true, "coverage: no thresholds configured — not gating"
	}
	byName := map[string]float64{
		"statements": totals.Statements.Pct,
		"branches":   totals.Branches.Pct,
		"functions":  totals.Functions.Pct,
		"lines":      totals.Lines.Pct,
	}
	var passed, failed []string
	for _, m := range coverageMetricOrder {
		floor, has := thresholds[m]
		if !has {
			continue
		}
		got := byName[m]
		if got >= floor {
			passed = append(passed, fmt.Sprintf("%s %.1f%% >= %g%%", m, got, floor))
		} else {
			failed = append(failed, fmt.Sprintf("%s %.1f%% < %g%%", m, got, floor))
		}
	}
	if len(failed) == 0 {
		return true, "coverage OK: " + strings.Join(passed, ", ")
	}
	msg := "coverage FAIL: " + strings.Join(failed, ", ")
	if len(passed) > 0 {
		msg += " (ok: " + strings.Join(passed, ", ") + ")"
	}
	return false, msg
}

// runTSMutation drives Stryker as a REPORT-ONLY check: it NEVER fails the
// gate. Whatever stryker reports — surviving mutants, a non-zero exit, or
// a run that couldn't start — the check returns Result{OK: true}, exactly
// like the Go adapter's go-mutesting graduation model. If stryker is not
// resolvable the check SKIPs.
func runTSMutation(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	if _, found := env.lookup("stryker"); !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "stryker not resolvable — skipping mutation report (install: npm i -D @stryker-mutator/core)",
		}
	}
	out, code, err := env.runner()(ctx, unitDir(env, unit), "npx", npxArgs("stryker", "run")...)
	res := Result{
		Name: name, Tier: tier, Duration: time.Since(start),
		OK: true, // report-only: NEVER fails the gate.
	}
	switch {
	case err != nil:
		res.Output = fmt.Sprintf("mutation report (could not run stryker, reporting only): %v\n%s", err, out)
	case code != 0:
		res.Output = fmt.Sprintf("mutation report (stryker exit %d — report-only, surviving mutants are advisory):\n%s", code, out)
	default:
		res.Output = "mutation report (stryker: no surviving mutants):\n" + out
	}
	return res
}

// runTSNotApplicable is the verdict for a go-only check (race, goleak)
// named on a ts unit: SKIP with an explanatory reason rather than a
// misleading pass or a hard error.
func runTSNotApplicable(name string, tier Tier) Result {
	return Result{
		Name: name, Tier: tier,
		OK: true, Skipped: true,
		Output: "not applicable to the TypeScript stack",
	}
}
