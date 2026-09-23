package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coverprofilePath extracts the -coverprofile=<path> the coverage check
// asks the runner to write, so a fake runner can drop a real profile there.
func coverprofilePath(args []string) string {
	const pfx = "-coverprofile="
	for _, a := range args {
		if strings.HasPrefix(a, pfx) {
			return strings.TrimPrefix(a, pfx)
		}
	}
	return ""
}

// coverResponder returns a respond func that, on the `go test` invocation,
// writes profileBody into the requested -coverprofile path (returning testCode),
// and on `go tool cover -func` returns funcOut/coverCode. This lets a runCoverage
// call exercise its real profile-read + per-package-floor path with a fake runner.
func coverResponder(profileBody, funcOut string, testCode, coverCode int) func(string, []string) (string, int, error) {
	return func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "test" {
			if p := coverprofilePath(args); p != "" && profileBody != "" {
				_ = os.WriteFile(p, []byte(profileBody), 0o644)
			}
			return "", testCode, nil
		}
		if len(args) > 0 && args[0] == "tool" {
			return funcOut, coverCode, nil
		}
		return "", 0, nil
	}
}

// ── runCoverage: per-package floor path (the 49% → covered gap) ──────────

// TestRunCoveragePerPackageFloorMasked drives runCoverage end-to-end with a
// package_floor set: the aggregate passes but one package sits below the
// per-package bar, so the check FAILS and names the masking. This exercises
// the whole per-package branch of runCoverage (profile read → packagesBelowFloor
// → the "aggregate passed and hid this" report), which the direct
// packagesBelowFloor unit tests do not reach.
func TestRunCoveragePerPackageFloorMasked(t *testing.T) {
	// good: 990/1000 = 99%; bad: 5/10 = 50%. Aggregate reported as 98.5%.
	profile := prof(
		[3]any{"m/internal/good/a.go", 990, 1},
		[3]any{"m/internal/good/b.go", 10, 0},
		[3]any{"m/internal/bad/a.go", 5, 1},
		[3]any{"m/internal/bad/b.go", 5, 0},
	)
	cfg := goCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 95, PackageFloor: 95, Scope: "./...",
	}})
	f := &fakeRunner{respond: coverResponder(profile, "total:\t(statements)\t98.5%\n", 0, 0)}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("a package below the per-package floor must FAIL even when the aggregate passes: %+v", r)
	}
	if !strings.Contains(r.Output, "per-package floor") || !strings.Contains(r.Output, "m/internal/bad") {
		t.Fatalf("report should name the package below the floor: %+v", r.Output)
	}
	if !strings.Contains(r.Output, "hid this") {
		t.Fatalf("report should name the aggregate masking: %+v", r.Output)
	}
}

// TestRunCoveragePerPackageFloorAllPass proves the passing per-package branch:
// aggregate and every package above the floor → PASS with the reassurance line.
func TestRunCoveragePerPackageFloorAllPass(t *testing.T) {
	profile := prof(
		[3]any{"m/internal/a/x.go", 100, 1},
		[3]any{"m/internal/b/y.go", 100, 1},
	)
	cfg := goCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 95, PackageFloor: 95, Scope: "./...",
	}})
	f := &fakeRunner{respond: coverResponder(profile, "total:\t(statements)\t100.0%\n", 0, 0)}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("aggregate + every package above floor should PASS: %+v", r)
	}
	if !strings.Contains(r.Output, "every package meets") {
		t.Fatalf("passing per-package run should say so: %+v", r.Output)
	}
}

// TestRunCoveragePerPackageExemptionError proves a reasonless exemption surfaces
// as a check failure through runCoverage (not just the direct helper).
func TestRunCoveragePerPackageExemptionError(t *testing.T) {
	profile := prof([3]any{"m/internal/a/x.go", 10, 1})
	cfg := goCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Floor: 95, PackageFloor: 95, Scope: "./...",
		PackageFloorExemptions: []PackageExemption{{Package: "m/internal/a", Floor: 10}}, // no reason
	}})
	f := &fakeRunner{respond: coverResponder(profile, "total:\t(statements)\t100.0%\n", 0, 0)}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "no reason") {
		t.Fatalf("a reasonless exemption must fail the check with a clear message: %+v", r)
	}
}

// TestRunCoverageToolCoverNonZero proves a non-zero `go tool cover` fails the
// check with a clear message.
func TestRunCoverageToolCoverNonZero(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "cover: bad profile", 2, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "go tool cover failed") {
		t.Fatalf("non-zero go tool cover should fail: %+v", r)
	}
}

// TestRunCoverageToolCoverExecError proves an exec error running `go tool cover`
// surfaces as res.Err (an infra failure, not a check verdict).
func TestRunCoverageToolCoverExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "", -1, errors.New("go missing")
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("an exec error from go tool cover should surface as res.Err: %+v", r)
	}
}

// TestRunCoverageTestExecError proves an exec error running the `go test` pass
// surfaces as res.Err.
func TestRunCoverageTestExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "test" {
			return "", -1, errors.New("go missing")
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("an exec error from the go test pass should surface as res.Err: %+v", r)
	}
}

// TestRunCoverageParseFails proves a `go tool cover` output with no
// total: line fails the check with the parse message.
func TestRunCoverageParseFails(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "no total anywhere in here\n", 0, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "could not parse coverage total") {
		t.Fatalf("unparseable coverage output should fail: %+v", r)
	}
}

// TestRunCoverageDefaultScope proves an empty Scope defaults to ./... (the
// scope == "" branch of runCoverage).
func TestRunCoverageDefaultScope(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66}}) // no Scope
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "total:\t(statements)\t80.0%\n", 0, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("default-scope coverage above floor should pass: %+v", r)
	}
	if !containsArg(f.calls[0].args, "./...") {
		t.Fatalf("empty scope should default to ./...: %+v", f.calls[0].args)
	}
}

// TestRunCoveragePreCommitRaceSkips proves a race-enabled coverage check at
// pre-commit SKIPs when git reports no changed Go packages (the skip branch
// of runCoverage).
func TestRunCoveragePreCommitRaceSkips(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-commit", Floor: 66, Scope: "./...", Race: true}})
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "git" {
			return "README.md\n", 0, nil // no Go change
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK || !r.Skipped {
		t.Fatalf("coverage race at pre-commit with no Go change should SKIP: %+v", r)
	}
}

// TestRunCoveragePreCommitRaceScopeError proves a git-diff failure while
// resolving the changed-package scope surfaces as res.Err.
func TestRunCoveragePreCommitRaceScopeError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-commit", Floor: 66, Scope: "./...", Race: true}})
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "git" {
			return "", -1, errors.New("git gone")
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a git-diff failure in scope resolution should surface as res.Err: %+v", r)
	}
}

// ── go adapter: infra-error / non-zero branches ─────────────────────────

func TestRunFormatExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"format": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("gofmt missing")
	}}
	r := findCheck(t, GoChecks(cfg), "format").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a gofmt exec error should surface as res.Err: %+v", r)
	}
}

func TestRunFormatNonZeroExit(t *testing.T) {
	// gofmt exits non-zero (no drift listed) → FAIL on the code alone.
	cfg := goCfg(map[string]CheckConfig{"format": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 3, nil }}
	r := findCheck(t, GoChecks(cfg), "format").Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("a non-zero gofmt exit should fail even with no listed files: %+v", r)
	}
}

func TestRunGoSimpleRaceScopeError(t *testing.T) {
	// test check, race at pre-commit, git diff errors → res.Err.
	cfg := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-commit", Race: true}})
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "git" {
			return "", -1, errors.New("git gone")
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "test").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a git-diff failure in race-scope resolution should surface as res.Err: %+v", r)
	}
}

func TestRunLintConfigVerifyExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 2 && args[0] == "config" && args[1] == "verify" {
			return "", -1, errors.New("cannot start golangci-lint")
		}
		return "", 0, nil
	}}
	env := f.env("/repo")
	env.LookupTool = func(string) (string, bool) { return "/opt/golangci-lint", true }
	r := findCheck(t, GoChecks(cfg), "lint").Run(context.Background(), env)
	if r.Err == nil {
		t.Fatalf("a golangci-lint config-verify exec error should surface as res.Err: %+v", r)
	}
}

func TestRunLintRunNonZero(t *testing.T) {
	// config verify passes, `run ./...` reports findings (code != 0) → FAIL.
	cfg := goCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 1 && args[0] == "run" {
			return "internal/x.go:1: some lint finding", 1, nil
		}
		return "", 0, nil
	}}
	env := f.env("/repo")
	env.LookupTool = func(string) (string, bool) { return "/opt/golangci-lint", true }
	r := findCheck(t, GoChecks(cfg), "lint").Run(context.Background(), env)
	if r.OK {
		t.Fatalf("golangci-lint findings (non-zero run) should fail: %+v", r)
	}
}

func TestRunVulnModVerifyExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"vuln": {Enabled: true, Tier: "pre-push"}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 1 && args[0] == "mod" {
			return "", -1, errors.New("go missing")
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a `go mod verify` exec error should surface as res.Err: %+v", r)
	}
}

func TestRunVulnGovulncheckExecError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"vuln": {Enabled: true, Tier: "pre-push"}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 1 && args[0] == "tool" {
			return "", -1, errors.New("govulncheck missing")
		}
		return "", 0, nil // mod verify ok
	}}
	r := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a govulncheck exec error should surface as res.Err: %+v", r)
	}
}

func TestRunVulnGovulncheckFindings(t *testing.T) {
	// mod verify ok, govulncheck reports a vuln (code != 0) → FAIL.
	cfg := goCfg(map[string]CheckConfig{"vuln": {Enabled: true, Tier: "pre-push"}})
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 1 && args[0] == "tool" {
			return "Vulnerability #1: GO-2024-0000", 1, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("a non-zero govulncheck (vulnerabilities found) should fail: %+v", r)
	}
}

// ── buildXCheck: the defensive `return nil` for an unknown check name ────

func TestBuildCheckUnknownNameReturnsNil(t *testing.T) {
	cc := CheckConfig{Enabled: true, Tier: "pre-commit"}
	unit := Unit{Dir: "."}
	if c := buildGoCheck("nope", "nope", TierPreCommit, cc, unit); c != nil {
		t.Fatalf("buildGoCheck(unknown) should return nil, got %v", c)
	}
	if c := buildTSCheck("nope", "nope", TierPreCommit, cc, unit); c != nil {
		t.Fatalf("buildTSCheck(unknown) should return nil, got %v", c)
	}
	if c := buildPyCheck("nope", "nope", TierPreCommit, cc, unit); c != nil {
		t.Fatalf("buildPyCheck(unknown) should return nil, got %v", c)
	}
}

// ── multi-unit labels for the ts and py adapters ─────────────────────────

func TestTSMultiUnitLabels(t *testing.T) {
	cfg := &Config{
		Stack:  "ts",
		Units:  []Unit{{Dir: "web"}, {Dir: "api"}},
		Checks: map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}},
	}
	if got := names(TSChecks(cfg)); strings.Join(got, ",") != "lint:web,lint:api" {
		t.Fatalf("ts multi-unit labels = %v", got)
	}
}

func TestPyMultiUnitLabels(t *testing.T) {
	cfg := &Config{
		Stack:  "py",
		Units:  []Unit{{Dir: "svc"}, {Dir: "tool"}},
		Checks: map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}},
	}
	if got := names(PyChecks(cfg)); strings.Join(got, ",") != "lint:svc,lint:tool" {
		t.Fatalf("py multi-unit labels = %v", got)
	}
}

// ── ts/py adapter: infra-error branches ─────────────────────────────────

func TestRunTSStaticBareExecError(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"static": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("npx missing")
	}}
	r := findCheck(t, TSChecks(cfg), "static").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a bare-tsc exec error should surface as res.Err: %+v", r)
	}
}

func TestRunTSStaticPerConfigExecError(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"static": {Enabled: true, Tier: "pre-commit"}}, "tsconfig.json")
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("npx missing")
	}}
	r := findCheck(t, TSChecks(cfg), "static").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a per-tsconfig exec error should surface as res.Err: %+v", r)
	}
}

func TestRunTSCoverageRunExecError(t *testing.T) {
	cfg := tsCfg(map[string]CheckConfig{"coverage": {
		Enabled: true, Tier: "pre-push", Thresholds: map[string]float64{"lines": 80},
	}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("npx missing")
	}}
	r := findCheck(t, TSChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a coverage-runner exec error should surface as res.Err: %+v", r)
	}
}

func TestCompareThresholdsNoneConfigured(t *testing.T) {
	ok, msg := compareThresholds(coverageTotals{}, nil)
	if !ok || !strings.Contains(msg, "no thresholds configured") {
		t.Fatalf("no thresholds should pass with a not-gating note: ok=%v msg=%q", ok, msg)
	}
}

func TestRunPyCoverageRunExecError(t *testing.T) {
	cfg := pyCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 80}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("python missing")
	}}
	r := findCheck(t, PyChecks(cfg), "coverage").Run(context.Background(), pyEnvWith(f, "pytest"))
	if r.Err == nil {
		t.Fatalf("a pytest exec error should surface as res.Err: %+v", r)
	}
}

// ── check.go: RunEnv default fallbacks ──────────────────────────────────

func TestRunEnvDefaults(t *testing.T) {
	var e RunEnv // all fields nil
	if e.runner() == nil {
		t.Fatalf("runner() should fall back to a non-nil default (OSRunner)")
	}
	if e.out() != os.Stdout {
		t.Fatalf("out() should fall back to os.Stdout")
	}
	// lookup() falls back to DefaultLookupTool: a ubiquitous binary resolves.
	if _, found := e.lookup("sh"); !found {
		t.Fatalf("lookup() default should resolve sh via PATH")
	}
}

// ── core.go: a SKIP result drives the SKIP status line ──────────────────

func TestRunStatusSkip(t *testing.T) {
	// lint with golangci-lint absent SKIPs; Run must render its SKIP status.
	cfg := goCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{}
	env := f.env("/repo")
	var buf strings.Builder
	env.Out = &buf
	env.LookupTool = func(string) (string, bool) { return "", false } // absent → skip
	results, ok, err := Run(context.Background(), cfg, TierPreCommit, env, nil)
	if err != nil || !ok {
		t.Fatalf("a skipped lint should leave the run green: ok=%v err=%v", ok, err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("expected one skipped result: %+v", results)
	}
	if !strings.Contains(buf.String(), "SKIP") {
		t.Fatalf("Run should print a SKIP status line: %s", buf.String())
	}
}

// ── exec.go: DefaultLookupTool resolves a GOBIN candidate ───────────────

func TestDefaultLookupToolFindsGOBINCandidate(t *testing.T) {
	dir := t.TempDir()
	// A binary that is NOT on PATH but sits in GOBIN.
	bin := filepath.Join(dir, "corpos-gate-fake-tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	t.Setenv("GOBIN", dir)
	got, found := DefaultLookupTool("corpos-gate-fake-tool")
	if !found || got != bin {
		t.Fatalf("DefaultLookupTool should resolve a GOBIN candidate: got=%q found=%v", got, found)
	}
}

// ── git_checks.go: shellcheck exec error, and orUnset's <unset> branch ──

func TestShellCheckExecError(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) > 0 && args[0] == "ls-files" {
			return "scripts/a.sh\n", 0, nil
		}
		return "", -1, errors.New("shellcheck vanished") // the shellcheck invocation
	}}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if r.Err == nil {
		t.Fatalf("a shellcheck exec error should surface as res.Err: %+v", r)
	}
}

func TestShellCheckGitLsFilesNonZero(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) > 0 && args[0] == "ls-files" {
			return "fatal: not a git repo", 128, nil
		}
		return "", 0, nil
	}}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if r.OK || !strings.Contains(r.Output, "git ls-files") {
		t.Fatalf("a non-zero git ls-files should fail with a clear message: %+v", r)
	}
}

func TestRepoIdentityDiffersWithUnsetFields(t *testing.T) {
	// local has only an email; global has only a name → they differ, and the
	// report renders the missing fields as <unset> (orUnset's empty branch).
	f := identityRunner(map[string]string{
		"--local user.email": "only@local.dev",
		"--global user.name": "Global Name",
	})
	r := repoIdentityCheck(t).Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("a divergent partial identity should fail: %+v", r)
	}
	if !strings.Contains(r.Output, "<unset>") {
		t.Fatalf("report should render a missing field as <unset>: %+v", r.Output)
	}
}

// ── rules.go: a non-zero git file-list surfaces as an error ─────────────

func TestRuleFilesGitNonZero(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "fatal: not a git repo", 128, nil
	}}
	r := Rule{Name: "x", Pattern: `y`, Sense: "must_not_match", Scope: "tree", Message: "m", Tier: "pre-commit"}
	check := findCheck(t, RulesChecks(&Config{Rules: []Rule{r}}), "rule:x")
	res := check.Run(context.Background(), f.env("/repo"))
	if res.Err == nil {
		t.Fatalf("a non-zero git file-list should surface as res.Err: %+v", res)
	}
}

// ── changed.go: a git-diff exec error propagates ────────────────────────

func TestChangedGoPackagesGitError(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("git gone")
	}}
	if _, err := changedGoPackages(context.Background(), f.env("/repo"), Unit{Dir: "go"}); err == nil {
		t.Fatalf("a git-diff exec error should propagate from changedGoPackages")
	}
}
