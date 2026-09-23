package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sophdn/corpos-gate/internal/gitenv"
)

// cmdInit handles `corpos-gate init [dir]`: it wires gate-only git hooks
// (pre-commit → the fast tier, pre-push → the slow tier) and, if absent,
// a starter gate.yml into the repo containing dir (default: the current
// directory). Re-running is idempotent — the managed hooks are rewritten,
// an existing gate.yml is left untouched.
func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "corpos-gate init [dir]\n\n"+
			"Installs gate-only git hooks + a starter gate.yml into the repo that\n"+
			"contains dir (default: the current directory). Idempotent: re-running\n"+
			"rewrites the managed hooks and never clobbers an existing gate.yml.\n\n"+
			"Requires the target to be a git repo (run 'git init' first) and the\n"+
			"corpos-gate binary to be on PATH for the hooks to fire (install it once\n"+
			"with 'go install github.com/sophdn/corpos-gate/cmd/corpos-gate@latest').\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := "."
	if fs.NArg() >= 1 {
		dir = fs.Arg(0)
	}
	if err := initRepo(dir, gitExec, stdout); err != nil {
		fmt.Fprintf(stderr, "corpos-gate init: %v\n", err)
		return 1
	}
	return 0
}

// gitFn runs a git command in dir and returns its trimmed stdout. It is
// injected so initRepo's repo/worktree resolution is hermetically
// testable without a real git process.
type gitFn func(dir string, args ...string) (string, error)

// gitExec is the production gitFn: a real `git -C dir …` invocation.
func gitExec(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// `-C dir` is NOT authoritative on its own: an inherited GIT_DIR /
	// GIT_WORK_TREE outranks it, so a `corpos-gate init` invoked from inside a
	// git hook would wire a DIFFERENT repository than the one named. Scrub the
	// context channels so the directory argument means what it says.
	cmd.Env = gitenv.Clean()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// initRepo performs the wiring for `init`, printing a per-action summary
// to out. All git access flows through git (injectable). Filesystem
// writes hit the real repo the resolved paths point at.
func initRepo(dir string, git gitFn, out io.Writer) error {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return fmt.Errorf("%s is not inside a git repository (run `git init` first)", dir)
	}

	if err := writeStarterConfig(root, out); err != nil {
		return err
	}
	if err := installHooks(root, git, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "corpos-gate: init complete for %s\n", root)
	return nil
}

// ── starter gate.yml ────────────────────────────────────────────────────

// writeStarterConfig writes a minimal stack-appropriate gate.yml at the
// repo root IFF none exists. An existing gate.yml is never clobbered. When
// it writes, it also prints a detection summary (stack, wired checks, and
// whether a coverage threshold was inferred from the app's own config).
func writeStarterConfig(root string, out io.Writer) error {
	path := filepath.Join(root, "gate.yml")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(out, "corpos-gate: gate.yml exists, keeping it\n")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat gate.yml: %w", err)
	}
	plan := planStarter(root)
	if err := os.WriteFile(path, []byte(plan.body), 0o644); err != nil {
		return fmt.Errorf("write starter gate.yml: %w", err)
	}
	fmt.Fprintf(out, "corpos-gate: wrote starter gate.yml (stack=%s)\n", plan.stack)
	printDetectionSummary(out, plan)
	return nil
}

// printDetectionSummary reports what init aligned the starter to: the
// detected stack + unit, the checks it wired, and whether the coverage
// threshold was inferred (and from which file) or left at a default. It is
// a few concise lines so a reader sees, without opening gate.yml, that init
// aligned to the app rather than dumping a fixed template.
func printDetectionSummary(out io.Writer, plan starterPlan) {
	fmt.Fprintf(out, "corpos-gate: detected stack=%s (unit dir: %s)\n", plan.stack, plan.unit)
	fmt.Fprintf(out, "corpos-gate: wired checks: %s\n", plan.checksLine)
	fmt.Fprintf(out, "corpos-gate: coverage: %s\n", plan.coverageLine)
}

// starterPlan is the outcome of pointing init at a repo: the detected
// stack, the gate.yml body to write, and the human-readable lines the
// detection summary prints.
type starterPlan struct {
	stack        string
	unit         string
	body         string
	checksLine   string // e.g. "format, vet, build [pre-commit]; test [pre-push]"
	coverageLine string // e.g. "inferred floor=80 from pyproject.toml [tool.coverage.report] fail_under"
}

// starterConfig auto-detects the repo's stack and returns its name plus a
// minimal gate.yml body — the (stack, body) view of planStarter, kept for
// callers that only need those two.
func starterConfig(root string) (stack, body string) {
	p := planStarter(root)
	return p.stack, p.body
}

// planStarter auto-detects the repo's stack and builds its starter plan.
// Detection order: a Go module (go.mod at the root or one dir down) → go;
// package.json → ts; a Python project marker (pyproject.toml / setup.py /
// setup.cfg / requirements.txt) → py; otherwise → shell. Each starter
// aligns to the app it is pointed at: the go/ts/py adapters are wired, and
// coverage thresholds are inferred from the app's own config where a
// standard location exists (jest for ts, coverage.py for py; Go has no
// standard coverage-floor location, so its floor is left unset).
func planStarter(root string) starterPlan {
	if unit, ok := detectGoUnit(root); ok {
		return goPlan(unit)
	}
	if fileExists(filepath.Join(root, "package.json")) {
		return tsPlan(root)
	}
	if detectPy(root) {
		return pyPlan(root)
	}
	return shellPlan()
}

// goPlan builds the go-stack starter. Go's cover tooling has no standard
// on-disk floor location (unlike coverage.py's fail_under or jest's
// coverageThreshold), so init does not invent a coverage number — the
// starter ships without a coverage check.
func goPlan(unit string) starterPlan {
	body := fmt.Sprintf(`# corpos-gate config — starter written by `+"`corpos-gate init`"+`.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. pre-commit is the
# FAST gate (keep it well under ~20s); push slow checks to pre-push/ci.
stack: go
units:
  - dir: %s
checks:
  format: { enabled: true,  tier: pre-commit }
  vet:    { enabled: true,  tier: pre-commit }
  build:  { enabled: true,  tier: pre-commit }
  test:   { enabled: true,  tier: pre-push }
`, unit)
	return starterPlan{
		stack:        "go",
		unit:         unit,
		body:         body,
		checksLine:   "format, vet, build [pre-commit]; test [pre-push]",
		coverageLine: "no standard Go coverage-floor config; left at default (no coverage check)",
	}
}

// tsHeader is the shared prefix of the ts starter (everything above the
// coverage check).
const tsHeader = `# corpos-gate config — starter written by ` + "`corpos-gate init`" + `.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. The ts adapter runs
# each tool via npx --no-install — edit to match this project.
stack: ts
units:
  - dir: .
checks:
  static: { enabled: true,  tier: pre-commit }
  lint:   { enabled: true,  tier: pre-commit }
  build:  { enabled: false, tier: pre-commit }
`

// tsPlan builds the ts-stack starter, seeding the coverage check's
// per-metric thresholds from jest's coverageThreshold.global in
// package.json when present. A vitest config lives in a JS/TS file that is
// not reliably parseable, so when no jest thresholds are found the coverage
// check is left un-thresholded (and it says so) rather than guessing.
func tsPlan(root string) starterPlan {
	if th, ok := parseJestThresholds(filepath.Join(root, "package.json")); ok {
		body := tsHeader + fmt.Sprintf(`  coverage:
    enabled: true
    tier: pre-push
    runner: jest
    thresholds: { %s }
`, th.inline())
		return starterPlan{
			stack:        "ts",
			unit:         ".",
			body:         body,
			checksLine:   "static, lint, build, coverage",
			coverageLine: fmt.Sprintf("inferred jest thresholds (%s) from package.json jest.coverageThreshold.global; runner=jest", th.inline()),
		}
	}
	body := tsHeader + `  coverage:
    enabled: true
    tier: pre-push
    # No jest coverageThreshold.global found in package.json. A vitest config
    # lives in a JS/TS file that is not reliably parseable, so no thresholds
    # were inferred — add a thresholds: map (and runner: vitest|jest) here.
`
	return starterPlan{
		stack:        "ts",
		unit:         ".",
		body:         body,
		checksLine:   "static, lint, build, coverage",
		coverageLine: "no jest coverageThreshold.global in package.json; coverage left without inferred thresholds (a vitest config is not parseable — add them manually)",
	}
}

// pyHeader is the shared prefix of the py starter (everything above the
// coverage check).
const pyHeader = `# corpos-gate config — starter written by ` + "`corpos-gate init`" + `.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. The py adapter runs
# ruff (format + lint) and pytest-cov (coverage); pytest-cov must be a dev
# dependency for the coverage check to measure anything.
stack: py
units:
  - dir: .
checks:
  format: { enabled: true,  tier: pre-commit }
  lint:   { enabled: true,  tier: pre-commit }
`

// pyPlan builds the py-stack starter (ruff format + lint, pytest-cov
// coverage). It seeds the coverage floor from coverage.py's fail_under when
// found (pyproject.toml [tool.coverage.report], else setup.cfg
// [coverage:report]); when absent it omits the floor rather than inventing
// one.
func pyPlan(root string) starterPlan {
	if floor, source, ok := inferPyFloor(root); ok {
		body := pyHeader + fmt.Sprintf(`  coverage:
    enabled: true
    tier: pre-push
    floor: %d
    scope: "."
`, floor)
		return starterPlan{
			stack:        "py",
			unit:         ".",
			body:         body,
			checksLine:   "format, lint, coverage",
			coverageLine: fmt.Sprintf("inferred floor=%d from %s", floor, source),
		}
	}
	body := pyHeader + `  coverage:
    enabled: true
    tier: pre-push
    scope: "."
    # No coverage.py fail_under found (pyproject.toml [tool.coverage.report]
    # or setup.cfg [coverage:report]); the floor is omitted rather than
    # invented — set an integer percent here to gate the coverage level.
`
	return starterPlan{
		stack:        "py",
		unit:         ".",
		body:         body,
		checksLine:   "format, lint, coverage",
		coverageLine: "no coverage.py fail_under found (pyproject.toml / setup.cfg); floor omitted (not invented)",
	}
}

// shellPlan builds the fallback shell starter (custom-only; no adapter).
func shellPlan() starterPlan {
	body := `# corpos-gate config — starter written by ` + "`corpos-gate init`" + `.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. Edit the commands to
# match this project (shell stack: everything is a custom check).
stack: shell
units:
  - dir: .
custom:
  - { name: shellcheck, cmd: "find . -name '*.sh' -print0 | xargs -0 -r shellcheck", tier: pre-commit }
`
	return starterPlan{
		stack:        "shell",
		unit:         ".",
		body:         body,
		checksLine:   "shellcheck [pre-commit] (custom)",
		coverageLine: "shell stack has no coverage check",
	}
}

// ── coverage-threshold inference ────────────────────────────────────────

// jestThresholds holds the subset of jest coverageThreshold.global metrics
// that corpos-gate gates on (statements, branches, functions, lines).
type jestThresholds struct {
	vals map[string]float64
}

// coverageMetricOrder is the stable emit order for coverage metrics, so a
// generated thresholds map and its summary line are deterministic.
var coverageMetricOrder = []string{"statements", "branches", "functions", "lines"}

// inline renders the present metrics as a YAML flow-map body
// ("statements: 80, branches: 70"), in coverageMetricOrder.
func (j jestThresholds) inline() string {
	parts := make([]string, 0, len(coverageMetricOrder))
	for _, m := range coverageMetricOrder {
		if v, ok := j.vals[m]; ok {
			parts = append(parts, fmt.Sprintf("%s: %s", m, strconv.FormatFloat(v, 'g', -1, 64)))
		}
	}
	return strings.Join(parts, ", ")
}

// parseJestThresholds reads jest.coverageThreshold.global from a
// package.json (which is JSON, so this is a real parse). It returns the
// four gated metrics that are present. A missing/malformed package.json, a
// missing jest key, or a global block with none of the four metrics all
// return ok=false — init then leaves the ts starter un-thresholded.
func parseJestThresholds(pkgPath string) (jestThresholds, bool) {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return jestThresholds{}, false
	}
	var pkg struct {
		Jest struct {
			CoverageThreshold struct {
				Global map[string]float64 `json:"global"`
			} `json:"coverageThreshold"`
		} `json:"jest"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return jestThresholds{}, false
	}
	vals := map[string]float64{}
	for _, m := range coverageMetricOrder {
		if v, ok := pkg.Jest.CoverageThreshold.Global[m]; ok {
			vals[m] = v
		}
	}
	if len(vals) == 0 {
		return jestThresholds{}, false
	}
	return jestThresholds{vals: vals}, true
}

// inferPyFloor reads coverage.py's fail_under, preferring pyproject.toml's
// [tool.coverage.report] and falling back to setup.cfg's [coverage:report].
// It returns the integer floor plus a human-readable source label.
func inferPyFloor(root string) (floor int, source string, ok bool) {
	if v, found := parseFailUnder(filepath.Join(root, "pyproject.toml"), "tool.coverage.report"); found {
		return v, "pyproject.toml [tool.coverage.report] fail_under", true
	}
	if v, found := parseFailUnder(filepath.Join(root, "setup.cfg"), "coverage:report"); found {
		return v, "setup.cfg [coverage:report] fail_under", true
	}
	return 0, "", false
}

// failUnderValueRe captures the leading numeric token of a fail_under value
// (int or float), ignoring any trailing inline comment or units.
var failUnderValueRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?`)

// parseFailUnder scans an INI/TOML-shaped config for a `fail_under`
// assignment inside the named `[section]`. It is a deliberately targeted
// line scan, NOT a full TOML/INI parse: coverage.py's fail_under is the
// only value init needs, and a targeted scan cannot be broken by malformed
// content elsewhere in the file (a stray unparseable table does not stop it
// reading a well-formed fail_under). coverage.py accepts a float fail_under
// while the gate's floor is an integer percentage, so a fractional value is
// rounded to the nearest whole percent.
func parseFailUnder(path, section string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	inSection := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.TrimSpace(line[1:len(line)-1]) == section
			continue
		}
		if !inSection {
			continue
		}
		key, val, found := cutAssignment(line)
		if !found || strings.TrimSpace(key) != "fail_under" {
			continue
		}
		tok := failUnderValueRe.FindString(strings.TrimSpace(val))
		if tok == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			return 0, false
		}
		return int(math.Round(f)), true
	}
	return 0, false
}

// cutAssignment splits an INI/TOML `key = value` (or INI `key: value`) at
// its first `=` or `:` separator. Section headers are handled by the caller
// before this is reached, so the `:` in an INI section name never confuses
// it.
func cutAssignment(line string) (key, val string, ok bool) {
	i := strings.IndexAny(line, "=:")
	if i < 0 {
		return "", "", false
	}
	return line[:i], line[i+1:], true
}

// detectPy reports whether root looks like a Python project by the presence
// of any standard project-root marker. It deliberately does NOT scan for
// loose *.py files: a single stray script is too weak a signal and produces
// false positives in non-Python repos.
func detectPy(root string) bool {
	for _, marker := range []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"} {
		if fileExists(filepath.Join(root, marker)) {
			return true
		}
	}
	return false
}

// detectGoUnit returns the unit dir for a Go module: "." when go.mod sits
// at the repo root, else the first immediate subdirectory that holds a
// go.mod (a repo that nests its Go module one directory down).
func detectGoUnit(root string) (string, bool) {
	if fileExists(filepath.Join(root, "go.mod")) {
		return ".", true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(root, e.Name(), "go.mod")) {
			return e.Name(), true
		}
	}
	return "", false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ── hooks ───────────────────────────────────────────────────────────────

// managedMarker tags a hook file corpos-gate wrote, so re-running init
// knows the file is its own to overwrite.
const managedMarker = "# corpos-gate-managed hook — safe to overwrite via `corpos-gate init`."

// installHooks writes the pre-commit + pre-push gate hooks, honoring the
// worktree workflow: in a LINKED worktree they go into that worktree's
// private git dir + a per-worktree core.hooksPath (never touching the
// main checkout or a shared config); in a MAIN checkout they go straight
// into the repo's hooks dir (.git/hooks) so no shared core.hooksPath is
// set that a worktree-discipline guard could collide with.
func installHooks(root string, git gitFn, out io.Writer) error {
	hooksDir, worktree, err := hookDir(root, git)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir %s: %w", hooksDir, err)
	}
	for hook, tier := range map[string]string{"pre-commit": "pre-commit", "pre-push": "pre-push"} {
		p := filepath.Join(hooksDir, hook)
		if err := os.WriteFile(p, []byte(hookScript(tier)), 0o755); err != nil {
			return fmt.Errorf("write %s hook: %w", hook, err)
		}
	}
	if worktree {
		// Point THIS worktree's hooks at the gate-only dir without
		// disturbing the main checkout (mirrors scripts/worktree-setup.sh).
		if _, err := git(root, "config", "extensions.worktreeConfig", "true"); err != nil {
			return fmt.Errorf("enable worktreeConfig: %w", err)
		}
		if _, err := git(root, "config", "--worktree", "core.hooksPath", hooksDir); err != nil {
			return fmt.Errorf("set per-worktree core.hooksPath: %w", err)
		}
		fmt.Fprintf(out, "corpos-gate: installed gate hooks for linked worktree at %s (per-worktree core.hooksPath)\n", hooksDir)
	} else {
		fmt.Fprintf(out, "corpos-gate: installed gate hooks at %s\n", hooksDir)
	}
	return nil
}

// hookDir resolves where the gate hooks go and whether root is a linked
// worktree. For a linked worktree it returns "<private-git-dir>/gate-only-hooks"
// (invisible to the main checkout, removed with the worktree). For a main
// checkout it returns the repo's real hooks dir (git rev-parse --git-path
// hooks, typically .git/hooks).
func hookDir(root string, git gitFn) (dir string, worktree bool, err error) {
	common, err := git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false, fmt.Errorf("resolve git common dir: %w", err)
	}
	mainRoot := ""
	if strings.HasSuffix(common, string(filepath.Separator)+".git") || filepath.Base(common) == ".git" {
		mainRoot = filepath.Dir(common)
	}
	// A linked worktree's root differs from the main checkout that owns
	// the common git dir.
	if mainRoot != "" && mainRoot != root {
		gitDir, gerr := git(root, "rev-parse", "--absolute-git-dir")
		if gerr != nil {
			return "", false, fmt.Errorf("resolve worktree git dir: %w", gerr)
		}
		return filepath.Join(gitDir, "gate-only-hooks"), true, nil
	}
	// Main checkout: install into the real hooks dir.
	hooksPath, herr := git(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if herr != nil {
		return "", false, fmt.Errorf("resolve hooks dir: %w", herr)
	}
	return hooksPath, false, nil
}

// hookScript is the body of a managed gate hook for the given tier. It
// calls corpos-gate from PATH (with a clear error if absent) and relies
// on git's native --no-verify for emergency bypass — it does nothing that
// would defeat it.
func hookScript(tier string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
%s
# Runs the corpos-gate %s tier. Emergency bypass is git-native:
#   git commit --no-verify     (or: git push --no-verify)
# This hook does nothing to defeat that.
set -euo pipefail
if ! command -v corpos-gate >/dev/null 2>&1; then
    echo "corpos-gate: binary not on PATH — install it once with" >&2
    echo "  go install github.com/sophdn/corpos-gate/cmd/corpos-gate@latest" >&2
    echo "then retry, or bypass this hook with --no-verify." >&2
    exit 1
fi
exec corpos-gate run --tier=%s
`, managedMarker, tier, tier)
}
