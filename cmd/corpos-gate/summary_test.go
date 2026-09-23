package main

// The run-closing summary named neither the
// tier nor the failing check.
//
// Example: a `git commit` followed by a `git push`
// printed the pre-commit hook's "PASS — all checks passed" and the
// pre-push hook's "FAIL — one or more checks failed" adjacent in the
// terminal. Two processes, two tiers, but the lines were tier-anonymous,
// so it read as one run contradicting itself — and the PASS list, being
// the pre-commit tier's, silently omitted the two pre-push checks.
// The reader saw an apparent contradiction and no named culprit, which is
// the condition that lost the per-check output on two earlier occurrences.
//
// This does NOT address the intermittent failure itself, which remains
// undiagnosed — only the reporting that made it hard to diagnose.

import (
	"bytes"
	"strings"
	"testing"
)

// The two tiers' summary lines must be distinguishable from the summary
// line alone — the exact confusion the bug records.
func TestSummaryNamesTier(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)

	var preCommit, errb bytes.Buffer
	if code := runCLI([]string{"run", "--tier=pre-commit", "--config", cfg}, &preCommit, &errb); code != 0 {
		t.Fatalf("pre-commit run exit = %d (%s)", code, errb.String())
	}
	var prePush bytes.Buffer
	if code := runCLI([]string{"run", "--tier=pre-push", "--config", cfg}, &prePush, &errb); code != 0 {
		t.Fatalf("pre-push run exit = %d (%s)", code, errb.String())
	}

	if !strings.Contains(preCommit.String(), "tier=pre-commit") {
		t.Errorf("pre-commit summary must name its tier:\n%s", preCommit.String())
	}
	if !strings.Contains(prePush.String(), "tier=pre-push") {
		t.Errorf("pre-push summary must name its tier:\n%s", prePush.String())
	}
}

// The PASS line states the scope it covered, so a narrower tier can't be
// misread as "everything passed".
func TestSummaryPassStatesScope(t *testing.T) {
	cfg := writeGateYML(t, customOnlyConfig)

	var out, errb bytes.Buffer
	runCLI([]string{"run", "--tier=pre-commit", "--config", cfg}, &out, &errb)
	got := out.String()
	if !strings.Contains(got, "all 1 checks passed") {
		t.Errorf("pre-commit PASS must count its plan:\n%s", got)
	}
	if !strings.Contains(got, "yes-check") {
		t.Errorf("pre-commit PASS must name what it covered:\n%s", got)
	}
	if strings.Contains(got, "push-only") {
		t.Errorf("pre-commit PASS must not claim the pre-push check:\n%s", got)
	}

	out.Reset()
	runCLI([]string{"run", "--tier=pre-push", "--config", cfg}, &out, &errb)
	if got := out.String(); !strings.Contains(got, "all 2 checks passed") {
		t.Errorf("pre-push PASS must count both checks:\n%s", got)
	}
}

// A reader who kept only the tail of a failing run still learns which
// check failed, and that the plan stopped early.
func TestSummaryFailNamesCheckAndNotRunCount(t *testing.T) {
	body := `stack: go
units:
  - dir: go
custom:
  - { name: fine,  cmd: "true",  tier: pre-commit }
  - { name: boom,  cmd: "false", tier: pre-commit }
  - { name: never, cmd: "true",  tier: pre-commit }
`
	cfg := writeGateYML(t, body)
	var out, errb bytes.Buffer
	if code := runCLI([]string{"run", "--tier=pre-commit", "--config", cfg}, &out, &errb); code != 1 {
		t.Fatalf("failing run exit = %d, want 1\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "boom failed") {
		t.Errorf("FAIL summary must name the failing check:\n%s", got)
	}
	if !strings.Contains(got, "1 of 3 checks not run") {
		t.Errorf("FAIL summary must report the fail-fast remainder:\n%s", got)
	}
	// The contradictory pair the bug reports: a run must never emit both
	// verdicts.
	if strings.Contains(got, "PASS — tier=") {
		t.Errorf("a failing run must not also print a PASS summary:\n%s", got)
	}
}
