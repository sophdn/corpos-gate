package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// GitChecks are the stack-AGNOSTIC, git-level checks: they guard properties any
// repository can have (a secret entering a diff, a leaked repo-local commit
// identity) regardless of its language, so they sit ABOVE the go/ts/py adapter
// split rather than inside one adapter. selectChecks assembles them before the
// adapter and custom checks. Each is opt-in per gate.yml `checks:` and omitted
// when disabled, so a repo that declares none pays nothing.
//
// They were bash custom guards duplicated per repo (scripts/pii-scan.sh was
// byte-identical across repos; scripts/check-repo-identity.sh had already
// drifted). Promoting them to first-class checks gives every repo it gates
// one implementation instead of a copy to keep in step.
func GitChecks(cfg *Config) []Check {
	var checks []Check
	if cc, ok := cfg.Checks["secrets"]; ok && cc.Enabled {
		checks = append(checks, newSecretsCheck(cc))
	}
	if cc, ok := cfg.Checks["repo-identity"]; ok && cc.Enabled {
		checks = append(checks, newRepoIdentityCheck(cc))
	}
	if cc, ok := cfg.Checks["shell"]; ok && cc.Enabled {
		checks = append(checks, newShellCheck(cc))
	}
	if cc, ok := cfg.Checks["gate-tamper"]; ok && cc.Enabled {
		checks = append(checks, newGateTamperCheck(cc))
	}
	return checks
}

// ── gate-tamper ─────────────────────────────────────────────────────────

// gateConfigFile is the config the gate-tamper check guards. The check is about
// the gate's own definition, so it is fixed to the canonical gate.yml rather
// than following a --config override.
const gateConfigFile = "gate.yml"

// newGateTamperCheck builds the self-enforcement check: an agent that can edit
// the gate has no gate, and weakening CI is the cheapest path to green. It
// compares the staged gate.yml against `git show HEAD:gate.yml` and FAILS when
// the gate is weakened — a previously-enabled check removed or flipped to
// disabled, or a coverage floor / package_floor moved down. It also FAILS when
// the staged gate.yml cannot be parsed (an unexpected syntax must not pass
// silently). It carries the floor-never-lowered guarantee scripts/check-gate-parity.sh
// used to provide, so retiring that script opens no gap. A deliberate weakening
// is still possible — the one bypass is `git commit --no-verify`; there is no
// in-config way to disable this check (disabling it in gate.yml is itself a
// tamper event, caught as a removed check). First-commit / no-HEAD-gate.yml is
// handled as "nothing to compare" rather than a failure.
func newGateTamperCheck(cc CheckConfig) Check {
	tier := tierOf(cc.Tier)
	return funcCheck{"gate-tamper", tier, func(ctx context.Context, env RunEnv) Result {
		start := time.Now()
		res := Result{Name: "gate-tamper", Tier: tier}
		curData, err := os.ReadFile(filepath.Join(env.RepoRoot, gateConfigFile))
		if err != nil {
			// No gate.yml in the working tree — nothing to guard.
			res.OK, res.Duration = true, time.Since(start)
			res.Output = "no gate.yml in the working tree — nothing to guard"
			return res
		}
		cur, perr := parseGateForTamper(curData)
		if perr != nil {
			// FAIL on an unparseable staged gate.yml — never pass silently.
			res.OK, res.Duration = false, time.Since(start)
			res.Output = "cannot parse the staged gate.yml (refusing to pass on unexpected syntax): " + perr.Error()
			return res
		}
		headData, code, gerr := env.runner()(ctx, env.RepoRoot, "git", "show", "HEAD:"+gateConfigFile)
		if gerr != nil {
			res.Err, res.Duration = gerr, time.Since(start)
			return res
		}
		if code != 0 {
			// No HEAD, or gate.yml is new at this commit — nothing to compare.
			res.OK, res.Duration = true, time.Since(start)
			res.Output = "no gate.yml at HEAD — new file, nothing to compare"
			return res
		}
		head, herr := parseGateForTamper([]byte(headData))
		if herr != nil {
			// A baseline we cannot parse cannot have been weakened FROM in a
			// measurable way; do not block on it (the staged-side parse above is
			// the guard that matters).
			res.OK, res.Duration = true, time.Since(start)
			res.Output = "HEAD gate.yml is unparseable — cannot compare, allowing"
			return res
		}
		var viol []string
		for name, hc := range head.Checks {
			cur2, ok := cur.Checks[name]
			if hc.Enabled && (!ok || !cur2.Enabled) {
				where := "disabled"
				if !ok {
					where = "removed"
				}
				viol = append(viol, fmt.Sprintf("check %q was enabled at HEAD and is now %s", name, where))
			}
			if ok {
				if hc.Floor > 0 && cur2.Floor < hc.Floor {
					viol = append(viol, fmt.Sprintf("check %q coverage floor lowered %d → %d", name, hc.Floor, cur2.Floor))
				}
				if hc.PackageFloor > 0 && cur2.PackageFloor < hc.PackageFloor {
					viol = append(viol, fmt.Sprintf("check %q package_floor lowered %d → %d", name, hc.PackageFloor, cur2.PackageFloor))
				}
			}
		}
		curCustom := map[string]bool{}
		for _, c := range cur.Custom {
			curCustom[c.Name] = true
		}
		for _, hc := range head.Custom {
			if !curCustom[hc.Name] {
				viol = append(viol, fmt.Sprintf("custom check %q removed", hc.Name))
			}
		}
		res.Duration = time.Since(start)
		if len(viol) > 0 {
			res.OK = false
			res.Output = "gate.yml weakened vs HEAD — refusing (a deliberate change lands with `git commit --no-verify`):\n  " +
				strings.Join(viol, "\n  ")
			return res
		}
		res.OK = true
		return res
	}}
}

// parseGateForTamper leniently unmarshals a gate.yml into its Config shape for
// the tamper comparison — NO strict KnownFields, so a HEAD or staged version
// carrying fields this binary doesn't know still parses (an unknown field is not
// a weakening). Returns an error only on malformed YAML.
func parseGateForTamper(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ── shell ───────────────────────────────────────────────────────────────

// newShellCheck builds the shell-lint check: it runs shellcheck over the repo's
// tracked *.sh files. It HARD-FAILS when shellcheck is absent rather than
// skipping — a linter that silently stops running is worse than none, because it
// reports green while checking nothing. Files matching any CheckConfig.Exclude
// glob are skipped, so deliberately-broken fixture scripts (benchmark inputs)
// are not held to the repo's lint bar. Fast → best declared at pre-commit.
func newShellCheck(cc CheckConfig) Check {
	tier := tierOf(cc.Tier)
	excludes := cc.Exclude
	return funcCheck{"shell", tier, func(ctx context.Context, env RunEnv) Result {
		start := time.Now()
		res := Result{Name: "shell", Tier: tier}
		bin, found := env.lookup("shellcheck")
		if !found {
			// HARD FAIL, not skip: an absent linter must be loud.
			res.Duration = time.Since(start)
			res.OK = false
			res.Output = "shellcheck not installed — the shell check hard-fails rather than skipping (a lint that silently stops running is worse than none). Install shellcheck and re-run."
			return res
		}
		listOut, code, err := env.runner()(ctx, env.RepoRoot, "git", "ls-files", "*.sh")
		if err != nil {
			res.Err = err
			res.Duration = time.Since(start)
			return res
		}
		if code != 0 {
			res.Duration = time.Since(start)
			res.Output = "git ls-files '*.sh' failed:\n" + listOut
			return res
		}
		var files []string
		for _, f := range strings.Split(strings.TrimSpace(listOut), "\n") {
			f = strings.TrimSpace(f)
			if f == "" || matchesAnyGlob(f, excludes) {
				continue
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			res.Duration = time.Since(start)
			res.OK = true
			res.Output = "no tracked shell files in scope"
			return res
		}
		out, code, err := env.runner()(ctx, env.RepoRoot, bin, files...)
		res.Duration = time.Since(start)
		if err != nil {
			res.Err = err
			return res
		}
		res.OK = code == 0
		if !res.OK {
			res.Output = out
		}
		return res
	}}
}

// matchesAnyGlob reports whether path matches any of the glob patterns. A
// pattern ending in "/**" matches path and everything beneath that prefix; any
// other pattern is matched with filepath.Match against the whole path. A
// malformed pattern never matches (it is skipped, not fatal).
func matchesAnyGlob(path string, globs []string) bool {
	for _, g := range globs {
		if strings.HasSuffix(g, "/**") {
			prefix := strings.TrimSuffix(g, "/**")
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(g, path); ok {
			return true
		}
	}
	return false
}

// ── secrets ─────────────────────────────────────────────────────────────

// waiverMarker is the inline escape hatch for a false positive: a staged line
// carrying this substring is skipped. It is the ecosystem-wide marker (also
// honoured by the skills-embed sync), so it stays a constant, not config.
const waiverMarker = "pii-allow"

// defaultDenylistFile is the repo-local file of project-specific PII regexes
// (one per line, `#` comments ignored) the secrets check reads when
// CheckConfig.Denylist is unset. Its absence simply turns that dimension off.
const defaultDenylistFile = ".pii-denylist"

// secretPattern pairs a compiled high-precision regex with the label the
// report prints. The set is deliberately narrow — provider token shapes and
// private-key headers essentially never appear by accident — so the guard does
// not cry wolf and train anyone to reach for --no-verify. Do NOT widen these
// without accepting the false-positive cost; that narrowness is the design.
type secretPattern struct {
	label string
	re    *regexp.Regexp
}

var secretPatterns = []secretPattern{
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"aws-access-key-id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"github-pat", regexp.MustCompile(`ghp_[0-9A-Za-z]{36}`)},
	{"github-oauth", regexp.MustCompile(`gho_[0-9A-Za-z]{36}`)},
	{"github-user-to-server", regexp.MustCompile(`ghu_[0-9A-Za-z]{36}`)},
	{"github-server-to-server", regexp.MustCompile(`ghs_[0-9A-Za-z]{36}`)},
	{"github-refresh", regexp.MustCompile(`ghr_[0-9A-Za-z]{36}`)},
	{"github-fine-grained-pat", regexp.MustCompile(`github_pat_[0-9A-Za-z_]{82}`)},
	{"slack-token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`)},
	{"google-api-key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"stripe-secret-key", regexp.MustCompile(`sk_live_[0-9A-Za-z]{24,}`)},
	{"stripe-restricted-key", regexp.MustCompile(`rk_live_[0-9A-Za-z]{24,}`)},
}

// newSecretsCheck builds the secrets check. It scans STAGED, newly-ADDED lines
// only (git diff --cached), so pre-existing content and unstaged work are never
// flagged — only what this commit introduces. Its own config files are excluded
// so a denylist never matches itself, and a line carrying the waiver marker is
// skipped. Fast → best declared at the pre-commit tier.
func newSecretsCheck(cc CheckConfig) Check {
	tier := tierOf(cc.Tier)
	denylistFile := cc.Denylist
	if denylistFile == "" {
		denylistFile = defaultDenylistFile
	}
	return funcCheck{"secrets", tier, func(ctx context.Context, env RunEnv) Result {
		start := time.Now()
		res := Result{Name: "secrets", Tier: tier}
		diff, code, err := env.runner()(ctx, env.RepoRoot, "git",
			"diff", "--cached", "-U0", "--diff-filter=ACM", "--",
			".", ":(exclude)"+denylistFile, ":(exclude).pii-allow")
		if err != nil {
			res.Err = err
			res.Duration = time.Since(start)
			return res
		}
		if code != 0 {
			// A non-zero git diff here is an infra fault (not a diff result);
			// surface it rather than silently passing.
			res.Duration = time.Since(start)
			res.Output = "git diff --cached failed:\n" + diff
			return res
		}
		added := addedScanLines(diff)
		var hits []string
		for _, sp := range secretPatterns {
			for _, ln := range added {
				if sp.re.MatchString(ln.text) {
					hits = append(hits, fmt.Sprintf("[secret ~ %s] %d: %s", sp.label, ln.num, ln.text))
				}
			}
		}
		for _, term := range denylistTerms(env, denylistFile) {
			re, cerr := regexp.Compile("(?i)" + term)
			if cerr != nil {
				continue // a malformed denylist line is skipped, not fatal
			}
			for _, ln := range added {
				if re.MatchString(ln.text) {
					hits = append(hits, fmt.Sprintf("[pii ~ %s] %d: %s", term, ln.num, ln.text))
				}
			}
		}
		res.Duration = time.Since(start)
		if len(hits) > 0 {
			res.OK = false
			res.Output = "possible secret / PII in staged changes:\n" + strings.Join(hits, "\n") +
				"\nIf this is a false positive: append '" + waiverMarker + "' to the line, or refine " + denylistFile + "."
			return res
		}
		res.OK = true
		return res
	}}
}

// scanLine is one staged-added line: its 1-based position in the added set and
// its text (the leading '+' stripped).
type scanLine struct {
	num  int
	text string
}

// addedScanLines extracts the newly-added lines from a unified diff: lines
// starting with '+' but not the '+++ ' file header, with a line carrying the
// waiver marker dropped. The number is the 1-based index within the added set
// (a stable handle for the report, matching the bash grep -n over the filtered
// stream).
func addedScanLines(diff string) []scanLine {
	var out []scanLine
	n := 0
	for _, raw := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "+++ ") {
			continue
		}
		if strings.Contains(raw, waiverMarker) {
			continue
		}
		n++
		out = append(out, scanLine{num: n, text: strings.TrimPrefix(raw, "+")})
	}
	return out
}

// denylistTerms reads the repo-local denylist regexes (one per line, blank
// lines and `#` comments ignored). A missing file yields no terms — that
// dimension of the guard is simply off, which is the documented default.
func denylistTerms(env RunEnv, file string) []string {
	data, err := os.ReadFile(filepath.Join(env.RepoRoot, file))
	if err != nil {
		return nil
	}
	var terms []string
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		terms = append(terms, t)
	}
	return terms
}

// ── repo-identity ───────────────────────────────────────────────────────

// newRepoIdentityCheck builds the repo-identity check: it FAILS when the repo
// carries a repo-local git identity that differs from the global one. A
// repo-local [user] section beats the global one and git prints nothing at
// commit time when they differ, so a test-fixture identity leaked into
// .git/config silently misattributes every commit (commits were silently
// authored by a fixture identity before anyone read
// `git log --format=%ae`). This detects the RESULT rather than trusting each
// git-running test helper to scrub GIT_DIR/GIT_WORK_TREE. Read-only, instant →
// pre-commit. Linked worktrees share the common config, so it is correct from a
// worktree too.
func newRepoIdentityCheck(cc CheckConfig) Check {
	tier := tierOf(cc.Tier)
	return funcCheck{"repo-identity", tier, func(ctx context.Context, env RunEnv) Result {
		start := time.Now()
		res := Result{Name: "repo-identity", Tier: tier, OK: true}
		localEmail := gitConfigGet(ctx, env, "--local", "user.email")
		localName := gitConfigGet(ctx, env, "--local", "user.name")
		if localEmail == "" && localName == "" {
			res.Duration = time.Since(start)
			res.Output = "no repo-local git identity — commits use the global one"
			return res
		}
		globalEmail := gitConfigGet(ctx, env, "--global", "user.email")
		globalName := gitConfigGet(ctx, env, "--global", "user.name")
		if localEmail == globalEmail && localName == globalName {
			res.Duration = time.Since(start)
			res.Output = fmt.Sprintf("repo-local identity is set but matches the global one (%s)", localEmail)
			return res
		}
		if globalEmail == "" && globalName == "" {
			// No global identity to compare against (a CI container legitimately
			// has neither, or only a local one): cannot judge, so SKIP.
			res.Skipped = true
			res.Duration = time.Since(start)
			res.Output = "no global git identity configured — nothing to compare against"
			return res
		}
		res.OK = false
		res.Duration = time.Since(start)
		res.Output = fmt.Sprintf(
			"this repository has a REPO-LOCAL git identity that differs from your global one.\n"+
				"Repo-local wins, so every commit made here is authored as:\n"+
				"    local :  %s <%s>\n"+
				"    global:  %s <%s>\n"+
				"git does not warn about this at commit time. Clear it with:\n"+
				"    git config --local --unset user.email\n"+
				"    git config --local --unset user.name\n"+
				"then audit authorship: git log --all --format='%%ae' | sort | uniq -c | sort -rn",
			orUnset(localName), orUnset(localEmail), orUnset(globalName), orUnset(globalEmail))
		return res
	}}
}

// gitConfigGet reads one git config value at the given scope (--local /
// --global). A missing key or a non-zero exit yields "" — the callers treat
// absence and emptiness identically.
func gitConfigGet(ctx context.Context, env RunEnv, scope, key string) string {
	out, code, err := env.runner()(ctx, env.RepoRoot, "git", "config", scope, "--get", key)
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// orUnset renders an empty identity field as <unset> for the report.
func orUnset(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}
