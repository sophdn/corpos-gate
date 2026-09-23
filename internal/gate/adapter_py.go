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

// pyCheckOrder is the stable execution order for the Python-stack checks —
// a fast→slow ordering: the format + lint + typecheck trio (pre-commit
// tier in a typical config), then the slower coverage and mutation runs.
// `race`/`goleak` trail at the end: they are go-only concerns that
// SKIP-with-reason if a py unit names them, mirroring the ts adapter.
var pyCheckOrder = []string{
	"format", "lint", "static", "coverage", "mutation", "race", "goleak",
}

// PyChecks builds one Check per ENABLED py-stack check in cfg, in the
// canonical pyCheckOrder, run in each unit's dir via the injected Runner.
// When more than one unit is configured, each check name is suffixed with
// the unit dir so results stay distinguishable — mirroring GoChecks and
// TSChecks.
func PyChecks(cfg *Config) []Check {
	var checks []Check
	multi := len(cfg.Units) > 1
	for _, name := range pyCheckOrder {
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
			if c := buildPyCheck(name, label, tier, cc, unit); c != nil {
				checks = append(checks, c)
			}
		}
	}
	return checks
}

// buildPyCheck constructs the Check for one (checkName, unit) pair.
func buildPyCheck(name, label string, tier Tier, cc CheckConfig, unit Unit) Check {
	switch name {
	case "format":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runPyFormat(ctx, env, label, tier, unit)
		}}
	case "lint":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runPyLint(ctx, env, label, tier, unit)
		}}
	case "static":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runPyStatic(ctx, env, label, tier, unit)
		}}
	case "coverage":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runPyCoverage(ctx, env, label, tier, unit, cc)
		}}
	case "mutation":
		return funcCheck{label, tier, func(ctx context.Context, env RunEnv) Result {
			return runPyMutation(ctx, env, label, tier, unit)
		}}
	case "race", "goleak":
		return funcCheck{label, tier, func(_ context.Context, _ RunEnv) Result {
			return runPyNotApplicable(label, tier)
		}}
	}
	return nil
}

// runPyFormat runs `ruff format --check .` in the unit dir and FAILS on
// non-zero exit (a formatting-drift gate — ruff exits non-zero when a file
// would be reformatted). If ruff is absent the check SKIPs with a reason,
// mirroring the go adapter's runLint.
func runPyFormat(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	if _, found := env.lookup("ruff"); !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "ruff not installed — skipping format (install: pip install ruff)",
		}
	}
	out, code, err := env.runner()(ctx, unitDir(env, unit), "ruff", "format", "--check", ".")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	if !res.OK && err == nil {
		res.Output = "ruff format drift (files would be reformatted):\n" + out
	}
	return res
}

// runPyLint runs `ruff check .` in the unit dir and FAILS on non-zero
// exit. If ruff is absent the check SKIPs with a reason.
func runPyLint(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	if _, found := env.lookup("ruff"); !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "ruff not installed — skipping lint (install: pip install ruff)",
		}
	}
	out, code, err := env.runner()(ctx, unitDir(env, unit), "ruff", "check", ".")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// runPyStatic runs `mypy .` in the unit dir and FAILS on non-zero exit. If
// mypy is not resolvable the check SKIPs with a reason — ml-training has
// no mypy configured yet, so this check typically stays disabled or skips.
func runPyStatic(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	if _, found := env.lookup("mypy"); !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "mypy not resolvable — skipping static typecheck (install: pip install mypy)",
		}
	}
	out, code, err := env.runner()(ctx, unitDir(env, unit), "mypy", ".")
	res := Result{Name: name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
	res.OK = err == nil && code == 0
	return res
}

// runPyCoverage runs `python -m pytest --cov=<target> [--cov=<target>...]
// --cov-report=json:<tmp>/coverage.json` in the unit dir, then reads the
// coverage.py json report and compares its `totals.percent_covered` to the
// scalar `Floor` (like the Go adapter — Python coverage is a single
// line-coverage metric, NOT the four-metric TS shape). It FAILS if the
// measured percentage is below the floor. The temp report dir is removed
// before return. If pytest is not resolvable the check SKIPs with a reason.
func runPyCoverage(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit, cc CheckConfig) (res Result) {
	start := time.Now()
	res = Result{Name: name, Tier: tier}
	// Named return so the deferred Duration stamp reaches the RETURNED
	// value (an unnamed return copies res before the defer mutates it).
	defer func() { res.Duration = time.Since(start) }()

	if _, found := env.lookup("pytest"); !found {
		res.OK = true
		res.Skipped = true
		res.Output = "pytest not resolvable — skipping coverage (install: pip install pytest pytest-cov)"
		return res
	}

	tmpDir, err := os.MkdirTemp("", "corpos-gate-py-cover-*")
	if err != nil {
		res.Err = fmt.Errorf("create coverage temp dir: %w", err)
		return res
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	jsonPath := filepath.Join(tmpDir, "coverage.json")
	out, code, err := env.runner()(ctx, unitDir(env, unit), "python", pyCoverageArgs(cc.Scope, jsonPath)...)
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

	measured, perr := parsePyCoverage(jsonPath)
	if perr != nil {
		res.OK = false
		res.Output = fmt.Sprintf("coverage json unreadable: %v", perr)
		return res
	}
	floor := float64(cc.Floor)
	if measured >= floor {
		res.OK = true
		res.Output = fmt.Sprintf("coverage %.1f%% >= %g%% floor", measured, floor)
	} else {
		res.OK = false
		res.Output = fmt.Sprintf("coverage %.1f%% < %g%% floor", measured, floor)
	}
	return res
}

// pyCoverageArgs builds the `python` args for the pytest-cov run: `-m
// pytest` followed by one `--cov=<target>` per scope target, then the
// json report flag pointed at jsonPath. Scope may hold several targets
// separated by whitespace and/or commas; empty scope defaults to a single
// `--cov=.` (whole unit dir).
func pyCoverageArgs(scope, jsonPath string) []string {
	args := []string{"-m", "pytest"}
	for _, target := range pyScopeTargets(scope) {
		args = append(args, "--cov="+target)
	}
	args = append(args, "--cov-report=json:"+jsonPath)
	return args
}

// pyScopeTargets splits a coverage scope into its individual targets,
// accepting whitespace- and/or comma-separated lists. An empty scope
// yields the single default target ".".
func pyScopeTargets(scope string) []string {
	fields := strings.FieldsFunc(scope, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(fields) == 0 {
		return []string{"."}
	}
	return fields
}

// pyCoverageReport mirrors the coverage.py json report shape: a `totals`
// block whose `percent_covered` is the single aggregate line-coverage
// percentage. Typed structs (not bare any) so the field is named.
type pyCoverageReport struct {
	Totals pyCoverageTotals `json:"totals"`
}

type pyCoverageTotals struct {
	PercentCovered float64 `json:"percent_covered"`
}

// parsePyCoverage reads and decodes a coverage.py json report, returning
// its `totals.percent_covered`. A missing or malformed file is an error
// (surfaced by the caller as a check failure) rather than a silent zero.
func parsePyCoverage(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read coverage json %s: %w", path, err)
	}
	var rep pyCoverageReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return 0, fmt.Errorf("parse coverage json %s: %w", path, err)
	}
	return rep.Totals.PercentCovered, nil
}

// runPyMutation drives mutmut as a REPORT-ONLY check: it NEVER fails the
// gate. Whatever mutmut reports — surviving mutants, a non-zero exit, or a
// run that couldn't start — the check returns Result{OK: true}, exactly
// like the Go and TS adapters' mutation graduation model. If mutmut is not
// resolvable the check SKIPs.
func runPyMutation(ctx context.Context, env RunEnv, name string, tier Tier, unit Unit) Result {
	start := time.Now()
	if _, found := env.lookup("mutmut"); !found {
		return Result{
			Name: name, Tier: tier, Duration: time.Since(start),
			OK: true, Skipped: true,
			Output: "mutmut not resolvable — skipping mutation report (install: pip install mutmut)",
		}
	}
	out, code, err := env.runner()(ctx, unitDir(env, unit), "mutmut", "run")
	res := Result{
		Name: name, Tier: tier, Duration: time.Since(start),
		OK:     true, // report-only: NEVER fails the gate.
		Report: true, // Output is the report; surface it even though OK.
	}
	switch {
	case err != nil:
		res.Output = fmt.Sprintf("mutation report (could not run mutmut, reporting only): %v\n%s", err, out)
	case code != 0:
		res.Output = fmt.Sprintf("mutation report (mutmut exit %d — report-only, surviving mutants are advisory):\n%s", code, out)
	default:
		res.Output = "mutation report (mutmut: no surviving mutants):\n" + out
	}
	return res
}

// runPyNotApplicable is the verdict for a go-only check (race, goleak)
// named on a py unit: SKIP with an explanatory reason rather than a
// misleading pass or a hard error.
func runPyNotApplicable(name string, tier Tier) Result {
	return Result{
		Name: name, Tier: tier,
		OK: true, Skipped: true,
		Output: "not applicable to the Python stack",
	}
}
