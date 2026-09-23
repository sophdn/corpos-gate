package gate

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	yaml "go.yaml.in/yaml/v3"
)

// Config is the parsed gate.yml — the stack-agnostic description of what
// the gate should run for a repository.
type Config struct {
	Stack  string                 `yaml:"stack"`
	Units  []Unit                 `yaml:"units"`
	Checks map[string]CheckConfig `yaml:"checks"`
	Custom []Custom               `yaml:"custom"`
	Rules  []Rule                 `yaml:"rules,omitempty"`
}

// Rule is a declarative text-pattern invariant: a named regex matched against a
// path scope with a must-match / must-not-match sense. It lets a repo state an
// architectural invariant (e.g. "no .go file may import C") as DATA in gate.yml
// rather than another one-off bash guard — and it travels to any project it
// gates. Deliberately text-only: no AST, no semgrep/ast-grep dependency. A rule
// that genuinely needs syntax matching is the signal to reconsider, recorded as
// such rather than worked around.
type Rule struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
	// Paths are globs selecting the files in scope ("dir/**" for a subtree,
	// else filepath.Match). Empty → every file in the scope.
	Paths []string `yaml:"paths,omitempty"`
	// Sense is "must_match" (the pattern must appear somewhere in scope) or
	// "must_not_match" (the pattern must appear nowhere in scope).
	Sense string `yaml:"sense"`
	// Scope is "tree" (all tracked files, the default) or "diff" (only files
	// staged in this commit).
	Scope   string `yaml:"scope,omitempty"`
	Message string `yaml:"message"`
	Tier    string `yaml:"tier"`
}

// Unit is one build unit within the repo — a module directory plus its
// build tags. The go adapter runs its checks inside Dir.
type Unit struct {
	Dir  string `yaml:"dir"`
	Tags string `yaml:"tags,omitempty"`
	// Tsconfig lists the tsconfig files the TS-stack `static` (typecheck)
	// check runs `tsc -p <cfg> --noEmit` against, one per entry. Empty →
	// a single bare `tsc --noEmit`. Only meaningful for the ts adapter.
	Tsconfig []string `yaml:"tsconfig,omitempty"`
}

// PackageExemption documents one package permitted below the per-package
// coverage floor. Package is matched against the import path as it appears
// in the coverage profile, either exactly or as a path prefix ending in
// "/..." (e.g. "internal/generated/..."). Reason is REQUIRED — see
// CheckConfig.PackageFloorExemptions.
type PackageExemption struct {
	Package string `yaml:"package"`
	Floor   int    `yaml:"floor"`
	Reason  string `yaml:"reason"`
}

// CheckConfig is the per-check toggle block. Floor and Scope are only
// meaningful for the coverage check. Race is meaningful for the coverage
// and test checks (it enables the data-race detector).
type CheckConfig struct {
	Enabled bool   `yaml:"enabled"`
	Tier    string `yaml:"tier"`
	Floor   int    `yaml:"floor,omitempty"`
	Scope   string `yaml:"scope,omitempty"`
	// PackageFloor, when > 0, additionally requires EVERY package in scope
	// to meet this percentage on its own. Floor alone is an AGGREGATE over
	// the scope, so a package can erode arbitrarily low while the total
	// stays above the bar: a package can lose its tests in a refactor and
	// fall to nothing while the aggregate never goes red. That is the blind
	// spot this closes. Zero (absent) → aggregate-only, the prior behavior.
	PackageFloor int `yaml:"package_floor,omitempty"`
	// PackageFloorExemptions carves out packages that legitimately sit
	// below PackageFloor (thin wrappers, generated code). Each entry must
	// state a floor AND a reason: an unexplained lower number is exactly
	// the silent erosion this check exists to surface, so a reasonless
	// exemption is a config error rather than a quiet pass.
	PackageFloorExemptions []PackageExemption `yaml:"package_floor_exemptions,omitempty"`
	// Race enables `go test -race` for the coverage/test checks. It
	// roughly DOUBLES suite runtime (the race detector instruments every
	// memory access), so it is opt-in per gate.yml rather than on by
	// default. Go runs `-race -coverprofile` together in a single pass,
	// so enabling it on the coverage check adds race detection without a
	// second suite run.
	Race bool `yaml:"race,omitempty"`
	// Thresholds carries the per-metric coverage floors for the TS-stack
	// coverage check — keys are the four vitest/jest metrics (statements,
	// branches, functions, lines), values are minimum percentages. A
	// metric with no entry here is not gated. The Go coverage check
	// ignores this and uses the scalar Floor instead.
	Thresholds map[string]float64 `yaml:"thresholds,omitempty"`
	// Runner selects the TS coverage test runner: "vitest" (the default
	// when empty) or "jest". Both emit the same istanbul-shaped
	// coverage-summary.json, so only the command differs. Ignored by the
	// Go adapter.
	Runner string `yaml:"runner,omitempty"`
	// GovulncheckVersion, when set on the Go vuln check, makes it run
	// govulncheck via `go run golang.org/x/vuln/cmd/govulncheck@<version>`
	// instead of `go tool govulncheck`. Use it for repos that keep go.mod
	// dependency-free (no govulncheck tool directive) — the go-run path
	// needs no go.mod entry. Empty → the default `go tool govulncheck`.
	GovulncheckVersion string `yaml:"govulncheck_version,omitempty"`
	// Denylist, on the `secrets` check, names the repo-local file of
	// project-specific PII regexes (one per line, `#` comments ignored) scanned
	// in addition to the always-on secret patterns. Empty → the default
	// ".pii-denylist"; a repo without that file runs secrets-only, which is the
	// documented default posture.
	Denylist string `yaml:"denylist,omitempty"`
	// Exclude, on the `shell` check, lists glob patterns for tracked shell
	// files to skip (e.g. deliberately-broken benchmark fixture scripts). A
	// pattern ending in "/**" excludes a whole subtree; others are matched with
	// filepath.Match. Empty → every tracked *.sh file is linted.
	Exclude []string `yaml:"exclude,omitempty"`
}

// Custom is a repo-specific escape-hatch check: an arbitrary shell
// command run in the repo root, failing on non-zero exit.
type Custom struct {
	Name string `yaml:"name"`
	Cmd  string `yaml:"cmd"`
	Tier string `yaml:"tier"`
}

// knownStacks and knownChecks bound the config's vocabulary. `go`, `ts`,
// and `py` have adapters (GoChecks / TSChecks / PyChecks); `shell` parses
// but is custom-only (no adapter checks).
var (
	knownStacks = map[string]bool{"go": true, "ts": true, "shell": true, "py": true}
	knownChecks = map[string]bool{
		"format": true, "vet": true, "lint": true, "build": true,
		"test": true, "coverage": true, "vuln": true, "mutation": true,
		// Stack-agnostic git-level checks (GitChecks), declarable by any repo.
		"secrets": true, "repo-identity": true, "shell": true, "gate-tamper": true,
		// TS-stack check names: `static` (typecheck). `race`/`goleak` are
		// go-only concerns that SKIP-with-reason if named on a ts unit, so
		// they parse here too. The py adapter reuses `format`, `lint`,
		// `static` (mypy), `coverage` (pytest-cov), and `mutation` (mutmut)
		// — all already present above — and SKIPs `race`/`goleak`.
		"static": true, "race": true, "goleak": true,
	}
	// coverageMetrics bounds the keys allowed in a coverage check's
	// `thresholds:` map — the four istanbul/vitest/jest metrics.
	coverageMetrics = map[string]bool{
		"statements": true, "branches": true, "functions": true, "lines": true,
	}
	// coverageRunners bounds the TS coverage `runner:` values.
	coverageRunners = map[string]bool{"vitest": true, "jest": true}
)

// Load reads and validates gate.yml at path. Parsing is strict: unknown
// keys are an error (KnownFields). Validation enforces a known stack, at
// least one unit, known check names, and valid tiers on every check and
// custom entry.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gate config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse gate config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid gate config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if !knownStacks[c.Stack] {
		return fmt.Errorf("unknown stack %q (want go, ts, py, or shell)", c.Stack)
	}
	if len(c.Units) == 0 {
		return fmt.Errorf("at least one unit is required")
	}
	for i, u := range c.Units {
		if u.Dir == "" {
			return fmt.Errorf("unit %d: dir is required", i)
		}
	}
	for name, cc := range c.Checks {
		if !knownChecks[name] {
			return fmt.Errorf("unknown check %q", name)
		}
		if _, err := ParseTier(cc.Tier); err != nil {
			return fmt.Errorf("check %q: %w", name, err)
		}
		for metric := range cc.Thresholds {
			if !coverageMetrics[metric] {
				return fmt.Errorf("check %q: unknown coverage metric %q in thresholds (want statements, branches, functions, or lines)", name, metric)
			}
		}
		if cc.Runner != "" && !coverageRunners[cc.Runner] {
			return fmt.Errorf("check %q: unknown coverage runner %q (want vitest or jest)", name, cc.Runner)
		}
	}
	for i, cu := range c.Custom {
		if cu.Name == "" {
			return fmt.Errorf("custom %d: name is required", i)
		}
		if cu.Cmd == "" {
			return fmt.Errorf("custom %q: cmd is required", cu.Name)
		}
		if _, err := ParseTier(cu.Tier); err != nil {
			return fmt.Errorf("custom %q: %w", cu.Name, err)
		}
	}
	for i, r := range c.Rules {
		if r.Name == "" {
			return fmt.Errorf("rule %d: name is required", i)
		}
		if r.Pattern == "" {
			return fmt.Errorf("rule %q: pattern is required", r.Name)
		}
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("rule %q: invalid pattern: %w", r.Name, err)
		}
		if r.Sense != "must_match" && r.Sense != "must_not_match" {
			return fmt.Errorf("rule %q: sense must be must_match or must_not_match", r.Name)
		}
		if r.Scope != "" && r.Scope != "tree" && r.Scope != "diff" {
			return fmt.Errorf("rule %q: scope must be tree or diff", r.Name)
		}
		if r.Message == "" {
			return fmt.Errorf("rule %q: message is required", r.Name)
		}
		if _, err := ParseTier(r.Tier); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	return nil
}
