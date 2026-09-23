package gate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── fake runner ─────────────────────────────────────────────────────────

type recordedCall struct {
	dir  string
	name string
	args []string
}

// fakeRunner records every command it's asked to run and delegates the
// response to a per-test respond func, so a check's command CONSTRUCTION
// is asserted without executing anything.
type fakeRunner struct {
	calls   []recordedCall
	respond func(name string, args []string) (string, int, error)
}

func (f *fakeRunner) run(_ context.Context, dir, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, recordedCall{dir: dir, name: name, args: append([]string(nil), args...)})
	if f.respond != nil {
		return f.respond(name, args)
	}
	return "", 0, nil
}

func (f *fakeRunner) env(root string) RunEnv {
	return RunEnv{RepoRoot: root, Run: f.run, Out: io.Discard}
}

// ── tier ────────────────────────────────────────────────────────────────

func TestTierIncludes(t *testing.T) {
	cases := []struct {
		run, check Tier
		want       bool
	}{
		{TierPreCommit, TierPreCommit, true},
		{TierPreCommit, TierPrePush, false}, // pre-commit run excludes pre-push
		{TierPreCommit, TierCI, false},      // pre-commit run excludes ci
		{TierPrePush, TierPreCommit, true},  // pre-push is a superset of pre-commit
		{TierPrePush, TierPrePush, true},
		{TierPrePush, TierCI, false},  // pre-push run excludes ci
		{TierCI, TierPreCommit, true}, // ci is a superset of everything
		{TierCI, TierPrePush, true},
		{TierCI, TierCI, true},
	}
	for _, c := range cases {
		if got := c.run.Includes(c.check); got != c.want {
			t.Errorf("Tier(%s).Includes(%s) = %v, want %v", c.run, c.check, got, c.want)
		}
	}
	// The superset ordering must be strict: pre-commit < pre-push < ci.
	if !(TierPreCommit < TierPrePush && TierPrePush < TierCI) {
		t.Fatalf("tier iota ordering broken: %d %d %d", TierPreCommit, TierPrePush, TierCI)
	}
}

func TestParseTierAndString(t *testing.T) {
	if tr, err := ParseTier("pre-commit"); err != nil || tr != TierPreCommit {
		t.Fatalf("ParseTier(pre-commit) = %v, %v", tr, err)
	}
	if tr, err := ParseTier("pre-push"); err != nil || tr != TierPrePush {
		t.Fatalf("ParseTier(pre-push) = %v, %v", tr, err)
	}
	if tr, err := ParseTier("ci"); err != nil || tr != TierCI {
		t.Fatalf("ParseTier(ci) = %v, %v", tr, err)
	}
	if _, err := ParseTier("nope"); err == nil {
		t.Fatalf("ParseTier(nope) expected error")
	}
	if TierPreCommit.String() != "pre-commit" || TierPrePush.String() != "pre-push" || TierCI.String() != "ci" {
		t.Fatalf("Tier.String mismatch: %q %q %q", TierPreCommit, TierPrePush, TierCI)
	}
	if !strings.Contains(Tier(99).String(), "99") {
		t.Fatalf("unknown tier String should include the int")
	}
	if tierOf("bogus") != TierPreCommit {
		t.Fatalf("tierOf(bogus) should default to pre-commit")
	}
}

// ── config ──────────────────────────────────────────────────────────────

const validYAML = `stack: go
units:
  - dir: go
    tags: sqlite_fts5
checks:
  format:   { enabled: true,  tier: pre-commit }
  test:     { enabled: true,  tier: pre-push }
  coverage: { enabled: true,  tier: pre-push, floor: 66, scope: "./..." }
custom:
  - { name: guard, cmd: "true", tier: pre-commit }
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gate.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Stack != "go" || len(cfg.Units) != 1 || cfg.Units[0].Tags != "sqlite_fts5" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.Checks["coverage"].Floor != 66 || cfg.Checks["coverage"].Scope != "./..." {
		t.Fatalf("coverage config not parsed: %+v", cfg.Checks["coverage"])
	}
	if len(cfg.Custom) != 1 || cfg.Custom[0].Cmd != "true" {
		t.Fatalf("custom not parsed: %+v", cfg.Custom)
	}
}

// TestLoadCITier proves tier "ci" is accepted on both a go check and a
// custom check (validation flows through ParseTier, which now knows ci).
func TestLoadCITier(t *testing.T) {
	body := `stack: go
units:
  - dir: go
checks:
  mutation: { enabled: false, tier: ci }
custom:
  - { name: slow-guard, cmd: "true", tier: ci }
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("ci-tier config should Load: %v", err)
	}
	if cfg.Checks["mutation"].Tier != "ci" || cfg.Custom[0].Tier != "ci" {
		t.Fatalf("ci tier not parsed: %+v / %+v", cfg.Checks["mutation"], cfg.Custom)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown stack":   "stack: rust\nunits:\n  - dir: go\n",
		"no units":        "stack: go\nunits: []\n",
		"empty unit dir":  "stack: go\nunits:\n  - tags: x\n",
		"unknown check":   "stack: go\nunits:\n  - dir: go\nchecks:\n  bogus: { enabled: true, tier: pre-commit }\n",
		"bad tier":        "stack: go\nunits:\n  - dir: go\nchecks:\n  vet: { enabled: true, tier: whenever }\n",
		"unknown key":     "stack: go\nunits:\n  - dir: go\nmystery: 3\n",
		"custom no cmd":   "stack: go\nunits:\n  - dir: go\ncustom:\n  - { name: g, tier: pre-commit }\n",
		"custom bad tier": "stack: go\nunits:\n  - dir: go\ncustom:\n  - { name: g, cmd: \"true\", tier: nope }\n",
		"custom no name":  "stack: go\nunits:\n  - dir: go\ncustom:\n  - { cmd: \"true\", tier: pre-commit }\n",
	}
	for name, body := range cases {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected Load error, got nil", name)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

// ── go adapter command construction ─────────────────────────────────────

func goCfg(checks map[string]CheckConfig) *Config {
	return &Config{
		Stack:  "go",
		Units:  []Unit{{Dir: "go", Tags: "sqlite_fts5"}},
		Checks: checks,
	}
}

// findCheck returns the built check with the given name.
func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("check %q not built; have %v", name, names(checks))
	return nil
}

func names(checks []Check) []string {
	var out []string
	for _, c := range checks {
		out = append(out, c.Name())
	}
	return out
}

func TestGoChecksOrderAndEnabled(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"format":   {Enabled: true, Tier: "pre-commit"},
		"vet":      {Enabled: true, Tier: "pre-commit"},
		"lint":     {Enabled: false, Tier: "pre-commit"}, // disabled → omitted
		"build":    {Enabled: true, Tier: "pre-commit"},
		"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."},
		"mutation": {Enabled: true, Tier: "pre-push"},
	})
	got := names(GoChecks(cfg))
	want := []string{"format", "vet", "build", "coverage", "mutation"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestFormatConstruction(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"format": {Enabled: true, Tier: "pre-commit"}})
	// Clean tree: empty output → PASS.
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 0, nil }}
	r := findCheck(t, GoChecks(cfg), "format").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("format on clean tree should pass: %+v", r)
	}
	call := f.calls[0]
	if call.dir != filepath.Join("/repo", "go") || call.name != "gofmt" ||
		strings.Join(call.args, " ") != "-l ." {
		t.Fatalf("gofmt command wrong: %+v", call)
	}
	// Drift: non-empty output → FAIL.
	f2 := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "bad.go\n", 0, nil }}
	r2 := findCheck(t, GoChecks(cfg), "format").Run(context.Background(), f2.env("/repo"))
	if r2.OK || !strings.Contains(r2.Output, "bad.go") {
		t.Fatalf("format drift should fail with file list: %+v", r2)
	}
}

func TestVetBuildTestConstruction(t *testing.T) {
	for _, sub := range []string{"vet", "build", "test"} {
		cfg := goCfg(map[string]CheckConfig{sub: {Enabled: true, Tier: "pre-commit"}})
		f := &fakeRunner{}
		r := findCheck(t, GoChecks(cfg), sub).Run(context.Background(), f.env("/repo"))
		if !r.OK {
			t.Fatalf("%s should pass on code 0: %+v", sub, r)
		}
		call := f.calls[0]
		want := sub + " -tags sqlite_fts5 ./..."
		if call.name != "go" || strings.Join(call.args, " ") != want {
			t.Fatalf("%s command = %q, want %q", sub, strings.Join(call.args, " "), want)
		}
	}
	// Non-zero exit → FAIL.
	cfg := goCfg(map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "vet error", 2, nil }}
	r := findCheck(t, GoChecks(cfg), "vet").Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("vet with code 2 should fail")
	}
}

func TestVetNoTags(t *testing.T) {
	cfg := &Config{Stack: "go", Units: []Unit{{Dir: "go"}}, Checks: map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}}}
	f := &fakeRunner{}
	findCheck(t, GoChecks(cfg), "vet").Run(context.Background(), f.env("/repo"))
	if got := strings.Join(f.calls[0].args, " "); got != "vet ./..." {
		t.Fatalf("no-tags vet args = %q, want %q", got, "vet ./...")
	}
}

func TestLintFoundAndSkip(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"lint": {Enabled: true, Tier: "pre-commit"}})
	// Found: runs the resolved binary.
	f := &fakeRunner{}
	env := f.env("/repo")
	env.LookupTool = func(name string) (string, bool) {
		if name != "golangci-lint" {
			t.Fatalf("looked up %q", name)
		}
		return "/opt/golangci-lint", true
	}
	r := findCheck(t, GoChecks(cfg), "lint").Run(context.Background(), env)
	if !r.OK || r.Skipped {
		t.Fatalf("lint found should pass: %+v", r)
	}
	// config verify FIRST, then run.
	if len(f.calls) != 2 {
		t.Fatalf("lint should run `config verify` then `run`, got %d calls", len(f.calls))
	}
	if f.calls[0].name != "/opt/golangci-lint" || strings.Join(f.calls[0].args, " ") != "config verify" {
		t.Fatalf("lint call 0 should be `config verify`: %+v", f.calls[0])
	}
	if f.calls[1].name != "/opt/golangci-lint" || strings.Join(f.calls[1].args, " ") != "run ./..." {
		t.Fatalf("lint call 1 should be `run ./...`: %+v", f.calls[1])
	}
	// config verify FAILS: the check FAILs and never reaches `run`.
	fVfy := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 2 && args[0] == "config" && args[1] == "verify" {
			return "bad config", 3, nil
		}
		return "", 0, nil
	}}
	envVfy := fVfy.env("/repo")
	envVfy.LookupTool = func(string) (string, bool) { return "/opt/golangci-lint", true }
	rVfy := findCheck(t, GoChecks(cfg), "lint").Run(context.Background(), envVfy)
	if rVfy.OK || rVfy.Skipped {
		t.Fatalf("lint should FAIL when config verify fails: %+v", rVfy)
	}
	if len(fVfy.calls) != 1 {
		t.Fatalf("lint should stop after a failed config verify (1 call), got %d", len(fVfy.calls))
	}
	// Absent: SKIP, not fail, and no command run.
	f2 := &fakeRunner{}
	env2 := f2.env("/repo")
	env2.LookupTool = func(string) (string, bool) { return "", false }
	r2 := findCheck(t, GoChecks(cfg), "lint").Run(context.Background(), env2)
	if !r2.Skipped || !r2.OK {
		t.Fatalf("lint absent should skip+ok: %+v", r2)
	}
	if len(f2.calls) != 0 {
		t.Fatalf("lint absent should run no command, ran %d", len(f2.calls))
	}
}

func TestCoverageConstructionAndParse(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	// Above floor.
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "example/foo.go:1:\tFoo\t100.0%\ntotal:\t(statements)\t72.5%\n", 0, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f.env("/repo"))
	if !r.OK || !strings.Contains(r.Output, "72.5") {
		t.Fatalf("coverage above floor should pass: %+v", r)
	}
	// Assert the test command carried -coverprofile and scope.
	testCall := f.calls[0]
	if testCall.args[0] != "test" || !containsArg(testCall.args, "./...") {
		t.Fatalf("coverage test command wrong: %+v", testCall)
	}
	var hasProfile bool
	for _, a := range testCall.args {
		if strings.HasPrefix(a, "-coverprofile=") {
			hasProfile = true
		}
	}
	if !hasProfile {
		t.Fatalf("coverage test missing -coverprofile: %+v", testCall)
	}
	// Below floor → FAIL.
	f2 := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "total:\t(statements)\t40.0%\n", 0, nil
		}
		return "", 0, nil
	}}
	r2 := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f2.env("/repo"))
	if r2.OK || !strings.Contains(r2.Output, "< 66") {
		t.Fatalf("coverage below floor should fail: %+v", r2)
	}
	// Test run itself fails.
	f3 := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "test" {
			return "boom", 1, nil
		}
		return "", 0, nil
	}}
	r3 := findCheck(t, GoChecks(cfg), "coverage").Run(context.Background(), f3.env("/repo"))
	if r3.OK {
		t.Fatalf("coverage with failing test run should fail")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestParseCoverageTotal(t *testing.T) {
	if v, ok := parseCoverageTotal("total:\t(statements)\t67.0%\n"); !ok || v != 67.0 {
		t.Fatalf("parse = %v, %v", v, ok)
	}
	if _, ok := parseCoverageTotal("no total here\n"); ok {
		t.Fatalf("expected no-parse for missing total")
	}
}

func TestVulnConstruction(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"vuln": {Enabled: true, Tier: "pre-push"}})
	f := &fakeRunner{}
	r := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("vuln should pass on all-zero: %+v", r)
	}
	if strings.Join(f.calls[0].args, " ") != "mod verify" {
		t.Fatalf("first vuln call should be `go mod verify`: %+v", f.calls[0])
	}
	if want := "tool govulncheck -tags sqlite_fts5 ./..."; strings.Join(f.calls[1].args, " ") != want {
		t.Fatalf("govulncheck call = %q, want %q", strings.Join(f.calls[1].args, " "), want)
	}
	// mod verify failure short-circuits.
	f2 := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		if len(args) >= 2 && args[0] == "mod" {
			return "hash mismatch", 1, nil
		}
		return "", 0, nil
	}}
	r2 := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f2.env("/repo"))
	if r2.OK || len(f2.calls) != 1 {
		t.Fatalf("vuln should fail+short-circuit on mod verify: %+v calls=%d", r2, len(f2.calls))
	}
}

// TestVulnGovulncheckVersion proves that setting govulncheck_version routes
// the scan through `go run …@<version>` (the dependency-free go.mod path)
// instead of `go tool govulncheck`.
func TestVulnGovulncheckVersion(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"vuln": {Enabled: true, Tier: "pre-push", GovulncheckVersion: "v1.3.0"}})
	f := &fakeRunner{}
	r := findCheck(t, GoChecks(cfg), "vuln").Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("vuln (go run path) should pass on all-zero: %+v", r)
	}
	if strings.Join(f.calls[0].args, " ") != "mod verify" {
		t.Fatalf("first call should be `go mod verify`: %+v", f.calls[0])
	}
	want := "run golang.org/x/vuln/cmd/govulncheck@v1.3.0 -tags sqlite_fts5 ./..."
	if got := strings.Join(f.calls[1].args, " "); got != want {
		t.Fatalf("govulncheck call = %q, want %q", got, want)
	}
}

// TestMutationReportOnly proves the mutation check is REPORT-ONLY: it
// never fails the gate regardless of the tool's outcome, and SKIPs
// cleanly when the tool is absent.
func TestMutationReportOnly(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"mutation": {Enabled: true, Tier: "pre-push"}})
	// cfgScoped carries an explicit mutation scope — required now that the
	// cross-repo hardcoded default is gone (a scopeless mutation check SKIPs).
	cfgScoped := goCfg(map[string]CheckConfig{"mutation": {Enabled: true, Tier: "pre-push", Scope: "internal/foo"}})

	// Tool absent → OK + Skipped, no command run (like runLint).
	fAbsent := &fakeRunner{}
	envAbsent := fAbsent.env("/repo")
	envAbsent.LookupTool = func(string) (string, bool) { return "", false }
	rAbsent := findCheck(t, GoChecks(cfg), "mutation").Run(context.Background(), envAbsent)
	if !rAbsent.OK || !rAbsent.Skipped {
		t.Fatalf("mutation with tool absent should be ok+skip, never fail: %+v", rAbsent)
	}
	if len(fAbsent.calls) != 0 {
		t.Fatalf("mutation with tool absent should run no command, ran %d", len(fAbsent.calls))
	}

	// Tool present + clean run (code 0) → OK, output captured, and the
	// command drives go-mutesting through scripts/go-mutesting-exec.sh.
	fOK := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "The mutation score is 1.000000", 0, nil
	}}
	envOK := fOK.env("/repo")
	envOK.LookupTool = func(name string) (string, bool) {
		if name != "go-mutesting" {
			t.Fatalf("looked up %q, want go-mutesting", name)
		}
		return "/opt/go-mutesting", true
	}
	rOK := findCheck(t, GoChecks(cfgScoped), "mutation").Run(context.Background(), envOK)
	if !rOK.OK || rOK.Skipped {
		t.Fatalf("mutation with tool present + clean run should be plain OK: %+v", rOK)
	}
	if !strings.Contains(rOK.Output, "1.000000") {
		t.Fatalf("mutation should capture tool output: %+v", rOK)
	}
	call := fOK.calls[0]
	if call.name != "/opt/go-mutesting" || call.dir != filepath.Join("/repo", "go") {
		t.Fatalf("mutation should run resolved go-mutesting in unit dir: %+v", call)
	}
	execArg := filepath.Join("/repo", "scripts", "go-mutesting-exec.sh")
	if !containsArg(call.args, "--exec="+execArg) {
		t.Fatalf("mutation should drive go-mutesting-exec.sh: %+v", call.args)
	}
	if !containsArg(call.args, "internal/foo") {
		t.Fatalf("mutation should run the configured scope: %+v", call.args)
	}

	// Tool present but NO scope configured → OK + Skipped, a message naming
	// the missing setting, and NO command run. The cross-repo hardcoded
	// default is gone (fix-mutation-default-scope), so a scopeless mutation
	// check must not mutate an arbitrary package.
	fNoScope := &fakeRunner{}
	envNoScope := fNoScope.env("/repo")
	envNoScope.LookupTool = func(string) (string, bool) { return "/opt/go-mutesting", true }
	rNoScope := findCheck(t, GoChecks(cfg), "mutation").Run(context.Background(), envNoScope)
	if !rNoScope.OK || !rNoScope.Skipped {
		t.Fatalf("mutation with no scope should be ok+skip, never fail: %+v", rNoScope)
	}
	if !strings.Contains(rNoScope.Output, "no scope") {
		t.Fatalf("mutation no-scope skip should name the missing setting: %+v", rNoScope)
	}
	if len(fNoScope.calls) != 0 {
		t.Fatalf("mutation with no scope should run no command, ran %d", len(fNoScope.calls))
	}

	// Tool present + NON-ZERO exit (surviving mutants) → STILL OK
	// (report-only), output captured. A non-zero tool exit must never
	// fail the gate.
	fSurvive := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "PASS \"removed statement\" survived", 1, nil
	}}
	envSurvive := fSurvive.env("/repo")
	envSurvive.LookupTool = func(string) (string, bool) { return "/opt/go-mutesting", true }
	rSurvive := findCheck(t, GoChecks(cfgScoped), "mutation").Run(context.Background(), envSurvive)
	if !rSurvive.OK {
		t.Fatalf("mutation non-zero tool exit must stay OK (report-only): %+v", rSurvive)
	}
	if !strings.Contains(rSurvive.Output, "survived") {
		t.Fatalf("mutation should capture surviving-mutant output: %+v", rSurvive)
	}

	// Tool present but couldn't START (exec error) → STILL OK.
	fErr := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("permission denied")
	}}
	envErr := fErr.env("/repo")
	envErr.LookupTool = func(string) (string, bool) { return "/opt/go-mutesting", true }
	rErr := findCheck(t, GoChecks(cfgScoped), "mutation").Run(context.Background(), envErr)
	if !rErr.OK {
		t.Fatalf("mutation exec error must stay OK (report-only): %+v", rErr)
	}
	if rErr.Err != nil {
		t.Fatalf("mutation report-only must not surface an infra Err: %+v", rErr)
	}
}

// TestMutationMultiScope proves the mutation scope is an allowlist: a
// space-separated scope names several packages, and each reaches go-mutesting
// as its own positional arg (the tool takes several packages). Mutation is too
// slow to run tree-wide, so a repo names the few packages where a wrong-but-not-
// crashing result is the real risk.
func TestMutationMultiScope(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"mutation": {Enabled: true, Tier: "ci", Scope: "internal/coding internal/router"},
	})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "The mutation score is 1.000000", 0, nil
	}}
	env := f.env("/repo")
	env.LookupTool = func(string) (string, bool) { return "/opt/go-mutesting", true }

	r := findCheck(t, GoChecks(cfg), "mutation").Run(context.Background(), env)
	if !r.OK || r.Skipped {
		t.Fatalf("mutation with a multi-package scope should run and be OK: %+v", r)
	}
	call := f.calls[0]
	if !containsArg(call.args, "internal/coding") {
		t.Fatalf("mutation should pass the first scope package as an arg: %+v", call.args)
	}
	if !containsArg(call.args, "internal/router") {
		t.Fatalf("mutation should pass the second scope package as an arg: %+v", call.args)
	}
	// The two packages must be SEPARATE args, not one space-joined arg that
	// go-mutesting would read as a single bad path.
	if containsArg(call.args, "internal/coding internal/router") {
		t.Fatalf("multi-package scope must split into one arg per package, not join them: %+v", call.args)
	}
}

// TestCoverageRace proves the coverage check adds `-race` when cc.Race is
// set (in the SAME run as -coverprofile) and omits it otherwise.
func TestCoverageRace(t *testing.T) {
	coverResp := func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "total:\t(statements)\t80.0%\n", 0, nil
		}
		return "", 0, nil
	}
	// Race enabled → test command carries -race alongside -coverprofile.
	cfgRace := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./...", Race: true}})
	fRace := &fakeRunner{respond: coverResp}
	findCheck(t, GoChecks(cfgRace), "coverage").Run(context.Background(), fRace.env("/repo"))
	testCall := fRace.calls[0]
	if !containsArg(testCall.args, "-race") {
		t.Fatalf("coverage with Race=true should include -race: %+v", testCall.args)
	}
	var hasProfile bool
	for _, a := range testCall.args {
		if strings.HasPrefix(a, "-coverprofile=") {
			hasProfile = true
		}
	}
	if !hasProfile {
		t.Fatalf("coverage -race run must still carry -coverprofile (single pass): %+v", testCall.args)
	}
	// Race disabled → no -race.
	cfgNoRace := goCfg(map[string]CheckConfig{"coverage": {Enabled: true, Tier: "pre-push", Floor: 66, Scope: "./..."}})
	fNoRace := &fakeRunner{respond: coverResp}
	findCheck(t, GoChecks(cfgNoRace), "coverage").Run(context.Background(), fNoRace.env("/repo"))
	if containsArg(fNoRace.calls[0].args, "-race") {
		t.Fatalf("coverage with Race unset should omit -race: %+v", fNoRace.calls[0].args)
	}
}

// TestTestCheckRace proves the standalone `test` check honors cc.Race.
func TestTestCheckRace(t *testing.T) {
	// Race enabled → `test -tags sqlite_fts5 -race ./...`.
	cfgRace := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-push", Race: true}})
	fRace := &fakeRunner{}
	findCheck(t, GoChecks(cfgRace), "test").Run(context.Background(), fRace.env("/repo"))
	if want := "test -tags sqlite_fts5 -race ./..."; strings.Join(fRace.calls[0].args, " ") != want {
		t.Fatalf("test with Race=true args = %q, want %q", strings.Join(fRace.calls[0].args, " "), want)
	}
	// Race disabled → no -race.
	cfgNoRace := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-push"}})
	fNoRace := &fakeRunner{}
	findCheck(t, GoChecks(cfgNoRace), "test").Run(context.Background(), fNoRace.env("/repo"))
	if want := "test -tags sqlite_fts5 ./..."; strings.Join(fNoRace.calls[0].args, " ") != want {
		t.Fatalf("test with Race unset args = %q, want %q", strings.Join(fNoRace.calls[0].args, " "), want)
	}
}

// ── changed-package race scoping ────────────────────────────────────────

// TestChangedGoPackages proves the git-diff → package-pattern mapping:
// staged + unstaged *.go files under the unit map to deduped, sorted
// `./…`-relative patterns; non-Go files and files outside the unit are
// dropped; the unit root maps to ".".
func TestChangedGoPackages(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		switch strings.Join(args, " ") {
		case "diff --name-only HEAD":
			// unstaged: two files in one pkg, one at the unit root, one
			// non-Go (dropped), one OUTSIDE the unit (dropped).
			return "go/internal/foo/a.go\ngo/internal/foo/b.go\ngo/main.go\ngo/README.md\ndocs/x.go\n", 0, nil
		case "diff --name-only --cached":
			// staged: another pkg + a dup of an unstaged pkg.
			return "go/internal/bar/c.go\ngo/internal/foo/d.go\n", 0, nil
		}
		return "", 0, nil
	}}
	unit := Unit{Dir: "go", Tags: "sqlite_fts5"}
	got, err := changedGoPackages(context.Background(), f.env("/repo"), unit)
	if err != nil {
		t.Fatalf("changedGoPackages: %v", err)
	}
	want := []string{".", "./internal/bar", "./internal/foo"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("changed packages = %v, want %v", got, want)
	}
	// Both git diff variants must have been consulted, in the repo root.
	if len(f.calls) != 2 || f.calls[0].dir != "/repo" || f.calls[0].name != "git" {
		t.Fatalf("expected 2 git-diff calls in repo root: %+v", f.calls)
	}
}

// TestChangedGoPackagesNone proves an all-non-Go / empty diff yields no
// packages (the SKIP signal for a pre-commit race run).
func TestChangedGoPackagesNone(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "README.md\ndocs/notes.txt\n", 0, nil
	}}
	got, err := changedGoPackages(context.Background(), f.env("/repo"), Unit{Dir: "go"})
	if err != nil {
		t.Fatalf("changedGoPackages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no Go change should yield no packages, got %v", got)
	}
}

// TestPreCommitRaceScopesToChanged proves a race-enabled TEST check at
// the pre-commit tier runs `-race` over ONLY the changed packages (not
// ./...), keeping pre-commit fast.
func TestPreCommitRaceScopesToChanged(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-commit", Race: true}})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" {
			return "go/internal/foo/a.go\n", 0, nil
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "test").Run(context.Background(), f.env("/repo"))
	if !r.OK || r.Skipped {
		t.Fatalf("changed-pkg race run should pass: %+v", r)
	}
	// The go test call must scope to ./internal/foo with -race, NOT ./...
	var testCall *recordedCall
	for i := range f.calls {
		if f.calls[i].name == "go" && len(f.calls[i].args) > 0 && f.calls[i].args[0] == "test" {
			testCall = &f.calls[i]
		}
	}
	if testCall == nil {
		t.Fatalf("no go test call recorded: %+v", f.calls)
	}
	joined := strings.Join(testCall.args, " ")
	if !containsArg(testCall.args, "-race") || !containsArg(testCall.args, "./internal/foo") {
		t.Fatalf("pre-commit race should scope to changed pkg with -race: %q", joined)
	}
	if containsArg(testCall.args, "./...") {
		t.Fatalf("pre-commit race must NOT run over ./...: %q", joined)
	}
}

// TestPreCommitRaceSkipsWhenNoChange proves the race check SKIPs (never
// fails) when git reports no changed Go packages.
func TestPreCommitRaceSkipsWhenNoChange(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-commit", Race: true}})
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "git" {
			return "README.md\n", 0, nil // no Go change
		}
		return "", 0, nil
	}}
	r := findCheck(t, GoChecks(cfg), "test").Run(context.Background(), f.env("/repo"))
	if !r.OK || !r.Skipped {
		t.Fatalf("no changed Go pkgs should SKIP (ok+skip): %+v", r)
	}
	if !strings.Contains(r.Output, "no changed Go packages") {
		t.Fatalf("skip note missing: %+v", r)
	}
	// No `go test` should have run.
	for _, c := range f.calls {
		if c.name == "go" && len(c.args) > 0 && c.args[0] == "test" {
			t.Fatalf("skipped race run must not shell out to go test: %+v", c)
		}
	}
}

// TestPrePushRaceRunsWholeTree proves that at a NON-pre-commit tier a
// race-enabled check still runs ./... (no changed-package narrowing, no
// git diff consulted).
func TestPrePushRaceRunsWholeTree(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"test": {Enabled: true, Tier: "pre-push", Race: true}})
	f := &fakeRunner{}
	findCheck(t, GoChecks(cfg), "test").Run(context.Background(), f.env("/repo"))
	if want := "test -tags sqlite_fts5 -race ./..."; strings.Join(f.calls[0].args, " ") != want {
		t.Fatalf("pre-push race args = %q, want %q", strings.Join(f.calls[0].args, " "), want)
	}
	// git diff must NOT have been consulted at pre-push.
	for _, c := range f.calls {
		if c.name == "git" {
			t.Fatalf("pre-push race must not consult git diff: %+v", c)
		}
	}
}

func TestMultiUnitLabels(t *testing.T) {
	cfg := &Config{
		Stack:  "go",
		Units:  []Unit{{Dir: "a"}, {Dir: "b"}},
		Checks: map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}},
	}
	got := names(GoChecks(cfg))
	if strings.Join(got, ",") != "vet:a,vet:b" {
		t.Fatalf("multi-unit labels = %v", got)
	}
}

// ── custom checks ───────────────────────────────────────────────────────

func TestCustomChecks(t *testing.T) {
	cfg := &Config{Custom: []Custom{
		{Name: "ok", Cmd: "true", Tier: "pre-commit"},
		{Name: "bad", Cmd: "false", Tier: "pre-commit"},
	}}
	checks := CustomChecks(cfg)
	f := &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		// args = ["-c", cmd]
		if args[1] == "false" {
			return "", 1, nil
		}
		return "", 0, nil
	}}
	rok := findCheck(t, checks, "ok").Run(context.Background(), f.env("/repo"))
	if !rok.OK {
		t.Fatalf("custom ok should pass: %+v", rok)
	}
	if f.calls[0].name != "sh" || f.calls[0].args[0] != "-c" || f.calls[0].dir != "/repo" {
		t.Fatalf("custom should run sh -c in repo root: %+v", f.calls[0])
	}
	rbad := findCheck(t, checks, "bad").Run(context.Background(), f.env("/repo"))
	if rbad.OK {
		t.Fatalf("custom bad should fail")
	}
}

// ── core orchestration ──────────────────────────────────────────────────

func TestPlanSuperset(t *testing.T) {
	// One check per tier proves the strict 3-tier superset:
	// ci ⊃ pre-push ⊃ pre-commit.
	cfg := goCfg(map[string]CheckConfig{
		"format":   {Enabled: true, Tier: "pre-commit"},
		"test":     {Enabled: true, Tier: "pre-push"},
		"mutation": {Enabled: true, Tier: "ci"},
	})
	pre := Plan(cfg, TierPreCommit, nil)
	push := Plan(cfg, TierPrePush, nil)
	ci := Plan(cfg, TierCI, nil)
	if len(pre) != 1 || pre[0].Name != "format" {
		t.Fatalf("pre-commit plan = %+v", pre)
	}
	if len(push) != 2 {
		t.Fatalf("pre-push plan should be 2 checks (format+test): %+v", push)
	}
	if len(ci) != 3 {
		t.Fatalf("ci plan should be 3 checks (all): %+v", ci)
	}
	// No ci-tier check may leak into the pre-commit or pre-push plan.
	for _, e := range append(append([]PlanEntry{}, pre...), push...) {
		if e.Name == "mutation" {
			t.Fatalf("ci-tier mutation must not appear below ci: %+v", e)
		}
	}
	// Superset: every lower-tier entry appears in every higher-tier plan.
	inPush := planSet(push)
	inCI := planSet(ci)
	for _, e := range pre {
		if !inPush[e.Name] || !inCI[e.Name] {
			t.Fatalf("pre-commit check %q missing from a superset plan", e.Name)
		}
	}
	for _, e := range push {
		if !inCI[e.Name] {
			t.Fatalf("pre-push check %q missing from ci plan", e.Name)
		}
	}
}

func planSet(entries []PlanEntry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		m[e.Name] = true
	}
	return m
}

// TestPlanSkip proves --skip drops a named check from the resolved plan
// while leaving the rest untouched (order + membership).
func TestPlanSkip(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"format":   {Enabled: true, Tier: "pre-commit"},
		"vet":      {Enabled: true, Tier: "pre-commit"},
		"vuln":     {Enabled: true, Tier: "pre-push"},
		"coverage": {Enabled: true, Tier: "pre-push", Floor: 66},
	})
	// Without skip, pre-push has all four.
	full := Plan(cfg, TierPrePush, nil)
	if len(full) != 4 {
		t.Fatalf("pre-push plan should have 4 checks: %+v", full)
	}
	// With skip=[vuln], vuln is gone and the other three remain in order.
	skipped := Plan(cfg, TierPrePush, []string{"vuln"})
	got := make([]string, 0, len(skipped))
	for _, e := range skipped {
		if e.Name == "vuln" {
			t.Fatalf("skip=[vuln] must omit vuln: %+v", skipped)
		}
		got = append(got, e.Name)
	}
	want := "format,vet,coverage"
	if strings.Join(got, ",") != want {
		t.Fatalf("skipped plan = %q, want %q", strings.Join(got, ","), want)
	}
}

// TestRunSkip proves --skip prevents a skipped check from ever running:
// the fakeRunner never sees the vuln command and the results omit vuln.
func TestRunSkip(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"vet":  {Enabled: true, Tier: "pre-commit"},
		"vuln": {Enabled: true, Tier: "pre-push"},
	})
	f := &fakeRunner{}
	results, ok, err := Run(context.Background(), cfg, TierPrePush, f.env("/repo"), []string{"vuln"})
	if err != nil || !ok {
		t.Fatalf("expected clean pass: ok=%v err=%v", ok, err)
	}
	for _, r := range results {
		if r.Name == "vuln" {
			t.Fatalf("skip=[vuln] must not produce a vuln result: %+v", results)
		}
	}
	// The vuln check shells out to `go mod verify` / `go tool govulncheck`;
	// with vuln skipped, no such call should have been recorded.
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "mod" {
			t.Fatalf("skipped vuln check still ran a command: %+v", c)
		}
	}
}

func TestRunFailFast(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"format": {Enabled: true, Tier: "pre-commit"},
		"vet":    {Enabled: true, Tier: "pre-commit"},
		"build":  {Enabled: true, Tier: "pre-commit"},
	})
	// gofmt returns drift (fail); nothing after format should run.
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "gofmt" {
			return "drift.go\n", 0, nil
		}
		return "", 0, nil
	}}
	results, ok, err := Run(context.Background(), cfg, TierPreCommit, f.env("/repo"), nil)
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if ok {
		t.Fatalf("Run should report not-ok on format failure")
	}
	if len(results) != 1 || results[0].Name != "format" {
		t.Fatalf("fail-fast should stop after format: %v", names(resultsToChecks(results)))
	}
}

func TestRunKeepGoing(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"format": {Enabled: true, Tier: "pre-commit"},
		"vet":    {Enabled: true, Tier: "pre-commit"},
	})
	f := &fakeRunner{respond: func(name string, _ []string) (string, int, error) {
		if name == "gofmt" {
			return "drift.go\n", 0, nil
		}
		return "", 0, nil
	}}
	env := f.env("/repo")
	env.KeepGoing = true
	results, ok, err := Run(context.Background(), cfg, TierPreCommit, env, nil)
	if err != nil || ok {
		t.Fatalf("expected not-ok, no err: ok=%v err=%v", ok, err)
	}
	if len(results) != 2 {
		t.Fatalf("KeepGoing should run all checks: got %d", len(results))
	}
}

func TestRunAllPass(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		"format": {Enabled: true, Tier: "pre-commit"},
		"vet":    {Enabled: true, Tier: "pre-commit"},
	})
	f := &fakeRunner{}
	results, ok, err := Run(context.Background(), cfg, TierPreCommit, f.env("/repo"), nil)
	if err != nil || !ok || len(results) != 2 {
		t.Fatalf("all-pass expected: ok=%v err=%v n=%d", ok, err, len(results))
	}
}

// TestRunSurfacesReportOnlyOutput proves a report-only check that PASSES still
// has its Output printed. Without this, mutation (always OK) would run and print
// only "mutation ... PASS", and the surviving-mutant report — the whole point —
// would be invisible.
func TestRunSurfacesReportOnlyOutput(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		// mutation at pre-commit so it runs in this TierPreCommit call.
		"mutation": {Enabled: true, Tier: "pre-commit", Scope: "internal/foo"},
	})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		// Non-zero exit = a mutant survived; go-mutesting prints the diff.
		return "--- a/x.go\n+++ b/x.go\n@@ removed statement survived", 1, nil
	}}
	env := f.env("/repo")
	var buf bytes.Buffer
	env.Out = &buf
	env.LookupTool = func(string) (string, bool) { return "/opt/go-mutesting", true }

	results, ok, err := Run(context.Background(), cfg, TierPreCommit, env, nil)
	if err != nil || !ok {
		t.Fatalf("report-only mutation must pass: ok=%v err=%v", ok, err)
	}
	if len(results) != 1 || !results[0].Report {
		t.Fatalf("expected one report-only result: %+v", results)
	}
	printed := buf.String()
	if !strings.Contains(printed, "mutation report") || !strings.Contains(printed, "survived") {
		t.Fatalf("report-only Output must be surfaced even on PASS, got:\n%s", printed)
	}
}

// TestRunHidesOrdinaryPassOutput proves the report-only surfacing is NARROW: an
// ordinary passing check (Report=false) whose Output carries only a terse OK
// message does not dump that message into the progress stream.
func TestRunHidesOrdinaryPassOutput(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{
		// coverage sets an Output on PASS ("coverage NN% >= floor") but is not
		// a Report check — it must stay quiet.
		"coverage": {Enabled: true, Tier: "pre-commit", Floor: 66, Scope: "./..."},
	})
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if len(args) > 0 && args[0] == "tool" {
			return "total:\t(statements)\t80.0%\n", 0, nil
		}
		return "", 0, nil
	}}
	env := f.env("/repo")
	var buf bytes.Buffer
	env.Out = &buf
	if _, ok, err := Run(context.Background(), cfg, TierPreCommit, env, nil); err != nil || !ok {
		t.Fatalf("coverage above floor should pass: ok=%v err=%v", ok, err)
	}
	if strings.Contains(buf.String(), "coverage 80") {
		t.Fatalf("an ordinary PASS must not dump its Output: %s", buf.String())
	}
}

func TestRunInfraError(t *testing.T) {
	cfg := goCfg(map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}})
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("binary not found")
	}}
	_, ok, err := Run(context.Background(), cfg, TierPreCommit, f.env("/repo"), nil)
	if ok || err == nil {
		t.Fatalf("infra error should surface: ok=%v err=%v", ok, err)
	}
}

// resultsToChecks is a tiny shim so names() can report result names in
// failure messages.
func resultsToChecks(rs []Result) []Check {
	var out []Check
	for _, r := range rs {
		out = append(out, funcCheck{name: r.Name})
	}
	return out
}

// ── real exec.OSRunner + DefaultLookupTool ──────────────────────────────

func TestOSRunner(t *testing.T) {
	// true → code 0.
	if out, code, err := OSRunner(context.Background(), "", "true"); err != nil || code != 0 || out != "" {
		t.Fatalf("true = %q %d %v", out, code, err)
	}
	// false → code 1, no exec error.
	if _, code, err := OSRunner(context.Background(), "", "false"); err != nil || code != 1 {
		t.Fatalf("false = %d %v", code, err)
	}
	// echo captures stdout.
	if out, code, err := OSRunner(context.Background(), "", "sh", "-c", "echo hi"); err != nil || code != 0 || strings.TrimSpace(out) != "hi" {
		t.Fatalf("echo = %q %d %v", out, code, err)
	}
	// missing binary → exec error.
	if _, _, err := OSRunner(context.Background(), "", "definitely-not-a-real-binary-xyz"); err == nil {
		t.Fatalf("missing binary should error")
	}
}

func TestDefaultLookupTool(t *testing.T) {
	// A ubiquitous binary resolves via PATH.
	if _, found := DefaultLookupTool("sh"); !found {
		t.Fatalf("expected to find sh on PATH")
	}
	if _, found := DefaultLookupTool("definitely-not-a-real-binary-xyz"); found {
		t.Fatalf("nonexistent tool should not resolve")
	}
}
