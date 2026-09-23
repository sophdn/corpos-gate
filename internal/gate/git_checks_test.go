package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitCfg builds a config carrying only the git-level checks under test.
func gitCfg(checks map[string]CheckConfig) *Config {
	return &Config{Stack: "go", Units: []Unit{{Dir: "."}}, Checks: checks}
}

// ── secrets ─────────────────────────────────────────────────────────────

func secretsCheck(t *testing.T, cc CheckConfig) Check {
	t.Helper()
	return findCheck(t, GitChecks(gitCfg(map[string]CheckConfig{"secrets": cc})), "secrets")
}

func TestSecretsCleanPasses(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "+++ b/main.go\n+func ok() {}\n+// nothing secret here\n", 0, nil
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("clean staged diff should pass: %+v", r)
	}
	// The diff must scan --cached added lines and exclude the guard's own config.
	call := f.calls[0]
	joined := strings.Join(call.args, " ")
	if call.name != "git" || !containsArg(call.args, "--cached") {
		t.Fatalf("secrets should run git diff --cached: %+v", call)
	}
	for _, excl := range []string{":(exclude).pii-denylist", ":(exclude).pii-allow"} {
		if !strings.Contains(joined, excl) {
			t.Fatalf("secrets diff must exclude %q: %q", excl, joined)
		}
	}
}

func TestSecretsDetectsPattern(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "+const key = \"AKIA" + strings.Repeat("A", 16) + "\"\n", 0, nil
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("an AWS key id in a staged add should fail")
	}
	if !strings.Contains(r.Output, "aws-access-key-id") {
		t.Fatalf("report should name the matched pattern: %+v", r.Output)
	}
}

func TestSecretsHonorsWaiver(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "+const key = \"AKIA" + strings.Repeat("A", 16) + "\" // pii-allow\n", 0, nil
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("a pii-allow-waived line must pass: %+v", r)
	}
}

func TestAddedScanLines(t *testing.T) {
	diff := "+++ b/f.go\n" + // file header — skip
		"+kept one\n" + // added — keep (num 1)
		"-removed\n" + // removal — skip
		" context\n" + // context — skip
		"+waived AKIA pii-allow\n" + // added but waived — skip
		"+kept two\n" // added — keep (num 2)
	got := addedScanLines(diff)
	if len(got) != 2 {
		t.Fatalf("expected 2 added scan lines, got %d: %+v", len(got), got)
	}
	if got[0].text != "kept one" || got[0].num != 1 || got[1].text != "kept two" || got[1].num != 2 {
		t.Fatalf("unexpected scan lines: %+v", got)
	}
}

func TestSecretsDenylistMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".pii-denylist"), []byte("# a comment\n\nAcme Corp\n"), 0o644); err != nil {
		t.Fatalf("write denylist: %v", err)
	}
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "+welcome to acme corp headquarters\n", 0, nil // case-insensitive match
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env(dir))
	if r.OK {
		t.Fatalf("a denylist term should fail: %+v", r)
	}
	if !strings.Contains(r.Output, "Acme Corp") {
		t.Fatalf("report should name the denylist term: %+v", r.Output)
	}
}

func TestSecretsCustomDenylistPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom-deny.txt"), []byte("secretname\n"), 0o644); err != nil {
		t.Fatalf("write denylist: %v", err)
	}
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "+the secretname is here\n", 0, nil
	}}
	// Default denylist file does not exist here → only the custom path matches.
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit", Denylist: "custom-deny.txt"}).Run(context.Background(), f.env(dir))
	if r.OK || !strings.Contains(r.Output, "secretname") {
		t.Fatalf("custom denylist path should be honored: %+v", r)
	}
}

func TestSecretsGitDiffError(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "", -1, errors.New("git missing")
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if r.Err == nil {
		t.Fatalf("a git exec error should surface as res.Err: %+v", r)
	}
}

func TestSecretsGitDiffNonZero(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) {
		return "fatal: bad revision", 1, nil
	}}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if r.OK || !strings.Contains(r.Output, "git diff --cached failed") {
		t.Fatalf("a non-zero git diff should fail with a clear message: %+v", r)
	}
}

func TestSecretsNothingStaged(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 0, nil }}
	r := secretsCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), f.env("/repo"))
	if !r.OK {
		t.Fatalf("an empty staged diff should pass: %+v", r)
	}
}

// ── repo-identity ───────────────────────────────────────────────────────

func repoIdentityCheck(t *testing.T) Check {
	t.Helper()
	return findCheck(t, GitChecks(gitCfg(map[string]CheckConfig{"repo-identity": {Enabled: true, Tier: "pre-commit"}})), "repo-identity")
}

// identityRunner answers `git config <scope> --get <key>` from a map keyed by
// "<scope> <key>" (e.g. "--local user.email"). Missing keys → "" (code 1).
func identityRunner(vals map[string]string) *fakeRunner {
	return &fakeRunner{respond: func(_ string, args []string) (string, int, error) {
		// args: config <scope> --get <key>
		if len(args) == 4 && args[0] == "config" && args[2] == "--get" {
			if v, ok := vals[args[1]+" "+args[3]]; ok && v != "" {
				return v, 0, nil
			}
			return "", 1, nil
		}
		return "", 0, nil
	}}
}

func TestRepoIdentityNoLocal(t *testing.T) {
	f := identityRunner(nil) // nothing local or global
	r := repoIdentityCheck(t).Run(context.Background(), f.env("/repo"))
	if !r.OK || r.Skipped {
		t.Fatalf("no repo-local identity should pass: %+v", r)
	}
}

func TestRepoIdentityMatchesGlobal(t *testing.T) {
	f := identityRunner(map[string]string{
		"--local user.email": "a@b.c", "--local user.name": "A",
		"--global user.email": "a@b.c", "--global user.name": "A",
	})
	r := repoIdentityCheck(t).Run(context.Background(), f.env("/repo"))
	if !r.OK || r.Skipped {
		t.Fatalf("a repo-local identity matching global should pass: %+v", r)
	}
}

func TestRepoIdentityDiffers(t *testing.T) {
	f := identityRunner(map[string]string{
		"--local user.email": "fixture@example.com", "--local user.name": "fixture",
		"--global user.email": "real@me.dev", "--global user.name": "Real",
	})
	r := repoIdentityCheck(t).Run(context.Background(), f.env("/repo"))
	if r.OK {
		t.Fatalf("a divergent repo-local identity must fail")
	}
	if !strings.Contains(r.Output, "fixture@example.com") || !strings.Contains(r.Output, "real@me.dev") {
		t.Fatalf("report should show both identities: %+v", r.Output)
	}
}

func TestRepoIdentityNoGlobal(t *testing.T) {
	f := identityRunner(map[string]string{
		"--local user.email": "only@local.dev", "--local user.name": "Local",
	})
	r := repoIdentityCheck(t).Run(context.Background(), f.env("/repo"))
	if !r.Skipped || !r.OK {
		t.Fatalf("a local identity with no global to compare should SKIP+ok: %+v", r)
	}
}

// ── shell ───────────────────────────────────────────────────────────────

func shellCheck(t *testing.T, cc CheckConfig) Check {
	t.Helper()
	return findCheck(t, GitChecks(gitCfg(map[string]CheckConfig{"shell": cc})), "shell")
}

// shellEnv wires a fakeRunner env that resolves shellcheck to a fake path.
func shellEnv(f *fakeRunner) RunEnv {
	env := f.env("/repo")
	env.LookupTool = func(name string) (string, bool) {
		if name == "shellcheck" {
			return "/opt/shellcheck", true
		}
		return "", false
	}
	return env
}

func TestShellAbsentHardFails(t *testing.T) {
	f := &fakeRunner{}
	env := f.env("/repo")
	env.LookupTool = func(string) (string, bool) { return "", false }
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), env)
	if r.OK || r.Skipped {
		t.Fatalf("absent shellcheck must HARD-FAIL, never skip: %+v", r)
	}
	if len(f.calls) != 0 {
		t.Fatalf("absent shellcheck should run no command, ran %d", len(f.calls))
	}
}

func TestShellCleanPasses(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) > 0 && args[0] == "ls-files" {
			return "scripts/a.sh\nscripts/b.sh\n", 0, nil
		}
		return "", 0, nil // shellcheck clean
	}}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if !r.OK {
		t.Fatalf("clean shellcheck run should pass: %+v", r)
	}
	// git ls-files then shellcheck over the two files.
	if f.calls[0].name != "git" || !containsArg(f.calls[0].args, "*.sh") {
		t.Fatalf("shell should enumerate via git ls-files '*.sh': %+v", f.calls[0])
	}
	sc := f.calls[1]
	if sc.name != "/opt/shellcheck" || !containsArg(sc.args, "scripts/a.sh") || !containsArg(sc.args, "scripts/b.sh") {
		t.Fatalf("shellcheck should run over the tracked files: %+v", sc)
	}
}

func TestShellReportsFindings(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) > 0 && args[0] == "ls-files" {
			return "scripts/a.sh\n", 0, nil
		}
		return "In scripts/a.sh line 3: SC2086 double quote to prevent globbing", 1, nil
	}}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if r.OK || !strings.Contains(r.Output, "SC2086") {
		t.Fatalf("shellcheck findings should fail with the output: %+v", r)
	}
}

func TestShellExcludesFixtures(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) > 0 && args[0] == "ls-files" {
			return "scripts/a.sh\nbench/ladder-a/items/x/gate.sh\nbench/ladder-a/run.sh\n", 0, nil
		}
		return "", 0, nil
	}}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit", Exclude: []string{"bench/ladder-a/items/**"}}).Run(context.Background(), shellEnv(f))
	if !r.OK {
		t.Fatalf("clean run should pass: %+v", r)
	}
	sc := f.calls[1]
	if containsArg(sc.args, "bench/ladder-a/items/x/gate.sh") {
		t.Fatalf("excluded fixture must not be linted: %+v", sc.args)
	}
	if !containsArg(sc.args, "scripts/a.sh") || !containsArg(sc.args, "bench/ladder-a/run.sh") {
		t.Fatalf("non-excluded shell files must be linted: %+v", sc.args)
	}
}

func TestShellNoFiles(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", 0, nil }}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if !r.OK || !strings.Contains(r.Output, "no tracked shell files") {
		t.Fatalf("no shell files should pass with a clear note: %+v", r)
	}
	if len(f.calls) != 1 {
		t.Fatalf("no shell files should not invoke shellcheck: %+v", f.calls)
	}
}

func TestShellGitLsFilesError(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", -1, errors.New("git gone") }}
	r := shellCheck(t, CheckConfig{Enabled: true, Tier: "pre-commit"}).Run(context.Background(), shellEnv(f))
	if r.Err == nil {
		t.Fatalf("a git ls-files exec error should surface: %+v", r)
	}
}

func TestMatchesAnyGlob(t *testing.T) {
	globs := []string{"bench/ladder-a/items/**", "scripts/*.tmp.sh", "[bad"}
	cases := map[string]bool{
		"bench/ladder-a/items/x/gate.sh": true,  // subtree
		"bench/ladder-a/items":           true,  // the prefix itself
		"bench/ladder-a/run.sh":          false, // sibling, not under items
		"scripts/foo.tmp.sh":             true,  // filepath.Match
		"scripts/foo.sh":                 false, // no match
	}
	for path, want := range cases {
		if got := matchesAnyGlob(path, globs); got != want {
			t.Errorf("matchesAnyGlob(%q) = %v, want %v", path, got, want)
		}
	}
}

// ── gate-tamper ─────────────────────────────────────────────────────────

// runGateTamper writes curYAML as the working-tree gate.yml (unless empty) and
// answers `git show HEAD:gate.yml` with headYAML/headCode/headErr, then runs the
// gate-tamper check.
func runGateTamper(t *testing.T, curYAML, headYAML string, headCode int, headErr error) Result {
	t.Helper()
	dir := t.TempDir()
	if curYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "gate.yml"), []byte(curYAML), 0o644); err != nil {
			t.Fatalf("write gate.yml: %v", err)
		}
	}
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" && len(args) >= 1 && args[0] == "show" {
			return headYAML, headCode, headErr
		}
		return "", 0, nil
	}}
	check := findCheck(t, GitChecks(gitCfg(map[string]CheckConfig{"gate-tamper": {Enabled: true, Tier: "pre-commit"}})), "gate-tamper")
	return check.Run(context.Background(), f.env(dir))
}

const headGate = `stack: go
units:
  - dir: .
checks:
  coverage: { enabled: true, tier: pre-push, floor: 95, package_floor: 95 }
  vet:      { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`

func TestGateTamperCleanPasses(t *testing.T) {
	// Identical current and HEAD → no weakening.
	if r := runGateTamper(t, headGate, headGate, 0, nil); !r.OK {
		t.Fatalf("identical gate.yml should pass: %+v", r)
	}
	// Strengthening (floor raised, a check added) → still OK.
	stronger := `stack: go
units:
  - dir: .
checks:
  coverage: { enabled: true, tier: pre-push, floor: 96, package_floor: 95 }
  vet:      { enabled: true, tier: pre-commit }
  build:    { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`
	if r := runGateTamper(t, stronger, headGate, 0, nil); !r.OK {
		t.Fatalf("strengthening the gate should pass: %+v", r)
	}
}

func TestGateTamperDetectsWeakening(t *testing.T) {
	cases := map[string]string{
		"check removed": `stack: go
units: [{dir: .}]
checks:
  vet: { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`,
		"check disabled": `stack: go
units: [{dir: .}]
checks:
  coverage: { enabled: false, tier: pre-push, floor: 95, package_floor: 95 }
  vet:      { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`,
		"coverage floor lowered": `stack: go
units: [{dir: .}]
checks:
  coverage: { enabled: true, tier: pre-push, floor: 80, package_floor: 95 }
  vet:      { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`,
		"package_floor lowered": `stack: go
units: [{dir: .}]
checks:
  coverage: { enabled: true, tier: pre-push, floor: 95, package_floor: 80 }
  vet:      { enabled: true, tier: pre-commit }
custom:
  - { name: pii, cmd: "true", tier: pre-commit }
`,
		"custom removed": `stack: go
units: [{dir: .}]
checks:
  coverage: { enabled: true, tier: pre-push, floor: 95, package_floor: 95 }
  vet:      { enabled: true, tier: pre-commit }
`,
	}
	for name, cur := range cases {
		r := runGateTamper(t, cur, headGate, 0, nil)
		if r.OK {
			t.Errorf("%s: gate-tamper should FAIL, got pass: %+v", name, r)
		}
	}
}

func TestGateTamperUnparseableCurrentFails(t *testing.T) {
	r := runGateTamper(t, "checks: [this is: not valid: yaml", headGate, 0, nil)
	if r.OK || !strings.Contains(r.Output, "cannot parse") {
		t.Fatalf("an unparseable staged gate.yml must FAIL: %+v", r)
	}
}

func TestGateTamperNoHeadPasses(t *testing.T) {
	// git show returns non-zero → gate.yml new at this commit → nothing to compare.
	if r := runGateTamper(t, headGate, "", 128, nil); !r.OK {
		t.Fatalf("a new gate.yml (no HEAD version) should pass: %+v", r)
	}
}

func TestGateTamperNoWorkingTreeFilePasses(t *testing.T) {
	// No gate.yml written to the temp dir → nothing to guard.
	if r := runGateTamper(t, "", headGate, 0, nil); !r.OK {
		t.Fatalf("no working-tree gate.yml should pass: %+v", r)
	}
}

func TestGateTamperGitShowError(t *testing.T) {
	r := runGateTamper(t, headGate, "", -1, errors.New("git gone"))
	if r.Err == nil {
		t.Fatalf("a git show exec error should surface: %+v", r)
	}
}

func TestGateTamperUnparseableHeadAllows(t *testing.T) {
	// A HEAD we cannot parse cannot be measurably weakened from → allow.
	if r := runGateTamper(t, headGate, "::: not yaml :::", 0, nil); !r.OK {
		t.Fatalf("an unparseable HEAD should allow (staged parse is the guard): %+v", r)
	}
}

// ── assembly + config ───────────────────────────────────────────────────

func TestGitChecksEnabledDisabled(t *testing.T) {
	// Both enabled → both built, git checks first.
	both := names(GitChecks(gitCfg(map[string]CheckConfig{
		"secrets":       {Enabled: true, Tier: "pre-commit"},
		"repo-identity": {Enabled: true, Tier: "pre-commit"},
	})))
	if strings.Join(both, ",") != "secrets,repo-identity" {
		t.Fatalf("both enabled should build secrets,repo-identity in order: %v", both)
	}
	// Disabled → omitted.
	if got := GitChecks(gitCfg(map[string]CheckConfig{"secrets": {Enabled: false, Tier: "pre-commit"}})); len(got) != 0 {
		t.Fatalf("a disabled git check must be omitted: %v", names(got))
	}
	// Absent → omitted (no panic on a config without them).
	if got := GitChecks(gitCfg(map[string]CheckConfig{"vet": {Enabled: true, Tier: "pre-commit"}})); len(got) != 0 {
		t.Fatalf("git checks absent from config should build none: %v", names(got))
	}
}

func TestSelectChecksPutsGitChecksFirst(t *testing.T) {
	cfg := &Config{
		Stack:  "go",
		Units:  []Unit{{Dir: "."}},
		Checks: map[string]CheckConfig{"secrets": {Enabled: true, Tier: "pre-commit"}, "vet": {Enabled: true, Tier: "pre-commit"}},
		Custom: []Custom{{Name: "z-guard", Cmd: "true", Tier: "pre-commit"}},
	}
	got := []string{}
	for _, e := range Plan(cfg, TierPreCommit, nil) {
		got = append(got, e.Name)
	}
	if strings.Join(got, ",") != "secrets,vet,z-guard" {
		t.Fatalf("plan order should be git,adapter,custom: %v", got)
	}
}

func TestLoadGitCheckNamesAndDenylist(t *testing.T) {
	body := `stack: go
units:
  - dir: .
checks:
  secrets:       { enabled: true, tier: pre-commit, denylist: .pii-denylist }
  repo-identity: { enabled: true, tier: pre-commit }
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("secrets/repo-identity config should Load: %v", err)
	}
	if cfg.Checks["secrets"].Denylist != ".pii-denylist" {
		t.Fatalf("denylist field not parsed: %+v", cfg.Checks["secrets"])
	}
	if !cfg.Checks["repo-identity"].Enabled {
		t.Fatalf("repo-identity not parsed: %+v", cfg.Checks["repo-identity"])
	}
}
