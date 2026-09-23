package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sophdn/corpos-gate/internal/gate"
)

// ── summarize: the FAIL verdict, including the fail-fast "not run" tail ──

func TestSummarizePass(t *testing.T) {
	planned := []gate.PlanEntry{{Name: "format", Tier: gate.TierPreCommit}, {Name: "vet", Tier: gate.TierPreCommit}}
	got := summarize(gate.TierPreCommit, planned, nil, true)
	if !strings.Contains(got, "PASS") || !strings.Contains(got, "tier=pre-commit") {
		t.Fatalf("PASS summary should name the tier: %q", got)
	}
	if !strings.Contains(got, "all 2 checks passed") || !strings.Contains(got, "format") || !strings.Contains(got, "vet") {
		t.Fatalf("PASS summary should count and name the checks: %q", got)
	}
}

func TestSummarizeFailWithNotRun(t *testing.T) {
	// Three planned, fail-fast stopped after the first failed → two not run.
	planned := []gate.PlanEntry{
		{Name: "format", Tier: gate.TierPreCommit},
		{Name: "vet", Tier: gate.TierPreCommit},
		{Name: "build", Tier: gate.TierPreCommit},
	}
	results := []gate.Result{{Name: "format", OK: false}}
	got := summarize(gate.TierPreCommit, planned, results, false)
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, "format failed") {
		t.Fatalf("FAIL summary should name the failing check: %q", got)
	}
	if !strings.Contains(got, "2 of 3 checks not run") {
		t.Fatalf("FAIL summary should report the fail-fast tail: %q", got)
	}
}

// ── findConfig: the not-found path (walk-up miss + git fallback miss) ────

func TestFindConfigNotFound(t *testing.T) {
	// A fresh temp dir with no gate.yml in any ancestor and (almost certainly)
	// no enclosing git repo carrying one → findConfig returns an error.
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if _, err := findConfig(""); err == nil {
		t.Fatalf("findConfig in a gate-less dir should error")
	}
}

// ── gitExec: an error from the underlying git process propagates ─────────

func TestGitExecError(t *testing.T) {
	// A non-git temp dir: `git rev-parse --show-toplevel` exits non-zero.
	if _, err := gitExec(t.TempDir(), "rev-parse", "--show-toplevel"); err == nil {
		t.Fatalf("gitExec in a non-git dir should return the git error")
	}
}

// ── initRepo: an installHooks failure propagates as an init error ────────

func TestInitRepoInstallHooksError(t *testing.T) {
	root := t.TempDir()
	// go.mod so writeStarterConfig succeeds, then installHooks fails because
	// hookDir cannot resolve the common git dir.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	git := func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		if key == "rev-parse --show-toplevel" {
			return root, nil
		}
		if strings.Contains(key, "--git-common-dir") {
			return "", errors.New("git blew up")
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := initRepo(root, git, &out); err == nil {
		t.Fatalf("initRepo should surface an installHooks/hookDir failure")
	}
}

// ── hookDir: each git resolution failure surfaces an error ───────────────

func TestHookDirCommonDirError(t *testing.T) {
	git := func(_ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "--git-common-dir") {
			return "", errors.New("nope")
		}
		return "", nil
	}
	if _, _, err := hookDir("/repo", git); err == nil {
		t.Fatalf("hookDir should error when the common git dir cannot resolve")
	}
}

func TestHookDirWorktreeGitDirError(t *testing.T) {
	// common dir names a main checkout (.git) whose parent differs from root →
	// linked worktree; the follow-up --absolute-git-dir then errors.
	root := "/some/linked/worktree"
	git := func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "--git-common-dir"):
			return filepath.Join("/main/checkout", ".git"), nil
		case strings.Contains(key, "--absolute-git-dir"):
			return "", errors.New("nope")
		}
		return "", nil
	}
	if _, _, err := hookDir(root, git); err == nil {
		t.Fatalf("hookDir should error when the worktree git dir cannot resolve")
	}
}

func TestHookDirMainHooksPathError(t *testing.T) {
	// Main checkout (common dir's parent == root), but resolving the hooks
	// path errors.
	root := "/repo"
	git := func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "--git-common-dir"):
			return filepath.Join(root, ".git"), nil
		case strings.Contains(key, "--git-path hooks"):
			return "", errors.New("nope")
		}
		return "", nil
	}
	if _, _, err := hookDir(root, git); err == nil {
		t.Fatalf("hookDir should error when the hooks path cannot resolve")
	}
}

// ── installHooks: MkdirAll failure and per-worktree config failure ───────

func TestInstallHooksMkdirError(t *testing.T) {
	root := t.TempDir()
	// Make the resolved hooks dir un-creatable: it sits UNDER a regular file.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	hooksPath := filepath.Join(blocker, "hooks") // MkdirAll must fail (parent is a file)
	git := func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "--git-common-dir"):
			return filepath.Join(root, ".git"), nil
		case strings.Contains(key, "--git-path hooks"):
			return hooksPath, nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := installHooks(root, git, &out); err == nil {
		t.Fatalf("installHooks should error when the hooks dir cannot be created")
	}
}

func TestInstallHooksWorktreeConfigError(t *testing.T) {
	mainRoot := t.TempDir()
	wt := t.TempDir()
	gitDir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	git := func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.Contains(key, "--git-common-dir"):
			return filepath.Join(mainRoot, ".git"), nil
		case strings.Contains(key, "--absolute-git-dir"):
			return gitDir, nil
		case strings.Contains(key, "extensions.worktreeConfig"):
			return "", errors.New("config write refused")
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := installHooks(wt, git, &out); err == nil {
		t.Fatalf("installHooks should surface a per-worktree config failure")
	}
}

// ── init.go small helpers: the remaining error / miss branches ───────────

func TestCutAssignmentNoSeparator(t *testing.T) {
	if _, _, ok := cutAssignment("nosepatall"); ok {
		t.Fatalf("a line with no = or : must not split")
	}
	if k, v, ok := cutAssignment("key = 5"); !ok || strings.TrimSpace(k) != "key" || strings.TrimSpace(v) != "5" {
		t.Fatalf("cutAssignment should split at the first separator: %q %q %v", k, v, ok)
	}
}

func TestDetectGoUnitReadDirError(t *testing.T) {
	// Point detectGoUnit at a regular FILE: no go.mod under it, and os.ReadDir
	// fails → (\"\", false).
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if unit, ok := detectGoUnit(f); ok {
		t.Fatalf("detectGoUnit on a non-directory should report no unit, got %q", unit)
	}
}

func TestParseJestThresholdsMissingFile(t *testing.T) {
	if _, ok := parseJestThresholds(filepath.Join(t.TempDir(), "nope.json")); ok {
		t.Fatalf("a missing package.json should yield no thresholds")
	}
}

func TestParseFailUnderNonNumeric(t *testing.T) {
	// fail_under present in-section but not a number → no floor inferred.
	dir := t.TempDir()
	p := filepath.Join(dir, "pyproject.toml")
	if err := os.WriteFile(p, []byte("[tool.coverage.report]\nfail_under = notanumber\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := parseFailUnder(p, "tool.coverage.report"); ok {
		t.Fatalf("a non-numeric fail_under must not parse")
	}
}

func TestParseFailUnderIgnoresNonAssignmentLine(t *testing.T) {
	// A bare in-section line with no key=value separator is skipped, and the
	// well-formed fail_under below it is still read.
	dir := t.TempDir()
	p := filepath.Join(dir, "setup.cfg")
	body := "[coverage:report]\nshow_missing\nfail_under = 77\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	v, ok := parseFailUnder(p, "coverage:report")
	if !ok || v != 77 {
		t.Fatalf("fail_under should read past a non-assignment line: v=%d ok=%v", v, ok)
	}
}

// ── cmdInit CLI: bad flag (usage + exit 2) and a failing init (exit 1) ───

func TestCmdInitBadFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCLI([]string{"init", "--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("init with an unknown flag should exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "corpos-gate init") {
		t.Fatalf("a flag error should print the init usage: %s", errb.String())
	}
}

func TestCmdInitFailureExit1(t *testing.T) {
	// Pointing init at a non-git dir → initRepo errors → exit 1 with a message.
	var out, errb bytes.Buffer
	if code := runCLI([]string{"init", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("init on a non-git dir should exit 1, got %d (%s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "corpos-gate init:") {
		t.Fatalf("a failed init should print an error: %s", errb.String())
	}
}

func TestCmdPlanBadFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCLI([]string{"plan", "--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("plan with an unknown flag should exit 2, got %d", code)
	}
}

// ── initRepo → writeStarterConfig: a stat error (root is not a dir) ───────

func TestInitRepoWriteConfigError(t *testing.T) {
	// root resolves to a regular FILE, so os.Stat(root/gate.yml) fails with a
	// non-"not exist" error → writeStarterConfig returns it and initRepo
	// surfaces it.
	fileRoot := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git := func(_ string, args ...string) (string, error) {
		if strings.Join(args, " ") == "rev-parse --show-toplevel" {
			return fileRoot, nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := initRepo(fileRoot, git, &out); err == nil {
		t.Fatalf("initRepo should surface a writeStarterConfig stat error")
	}
}

// ── findConfig: git-root fallback runs when walk-up finds nothing ────────

func TestFindConfigGitRootFallback(t *testing.T) {
	if _, err := gitExec(t.TempDir(), "--version"); err != nil {
		t.Skip("git not available")
	}
	// A real git repo with NO gate.yml anywhere: walk-up finds nothing, the
	// git-root fallback runs `git rev-parse --show-toplevel`, stats
	// <root>/gate.yml, misses, and findConfig returns not-found.
	root := t.TempDir()
	if _, err := gitExec(root, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if _, err := findConfig(""); err == nil {
		t.Fatalf("a git repo with no gate.yml should still return not-found")
	}
}

// ── loadForCmd: findConfig failure (no --config, gate-less dir) → exit 1 ──

func TestLoadForCmdFindConfigError(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	var out, errb bytes.Buffer
	// plan with no --config: loadForCmd → findConfig cannot locate gate.yml.
	if code := runCLI([]string{"plan", "--tier=pre-commit"}, &out, &errb); code != 1 {
		t.Fatalf("plan with no locatable gate.yml should exit 1, got %d", code)
	}
	if !strings.Contains(errb.String(), "gate.yml not found") {
		t.Fatalf("the not-found error should reach stderr: %s", errb.String())
	}
}

// ── cmdRun: the --list path already prints the plan (kept green) ─────────

func TestCmdRunListPrintsPlanAndPasses(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)
	var out, errb bytes.Buffer
	if code := cmdRun([]string{"--tier=pre-commit", "--list", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("cmdRun --list exit = %d (%s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "plan") || !strings.Contains(out.String(), "yes-check") {
		t.Fatalf("cmdRun --list should print the plan: %s", out.String())
	}
}
