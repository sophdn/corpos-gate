package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ruleCheckOn writes files into a temp repo, wires a fakeRunner that answers the
// git file-list command with listOut, and returns the built rule check + env.
func ruleCheckOn(t *testing.T, r Rule, files map[string]string, listOut string) (Check, RunEnv) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" {
			return listOut, 0, nil
		}
		return "", 0, nil
	}}
	checks := RulesChecks(&Config{Rules: []Rule{r}})
	return findCheck(t, checks, "rule:"+r.Name), f.env(dir)
}

func TestRuleMustNotMatchDetects(t *testing.T) {
	r := Rule{Name: "cgo-free", Pattern: `import "C"`, Sense: "must_not_match", Scope: "tree", Message: "CGo-free invariant: no .go file may import C", Tier: "pre-commit"}
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "package a\n\nimport \"C\"\n"}, "a.go\n")
	res := check.Run(context.Background(), env)
	if res.OK {
		t.Fatalf("must_not_match should FAIL when the pattern is present")
	}
	if !strings.Contains(res.Output, "CGo-free invariant") || !strings.Contains(res.Output, "a.go:3") {
		t.Fatalf("failure should carry the rule message + the hit: %+v", res.Output)
	}
}

func TestRuleMustNotMatchClean(t *testing.T) {
	r := Rule{Name: "cgo-free", Pattern: `import "C"`, Sense: "must_not_match", Scope: "tree", Message: "no C", Tier: "pre-commit"}
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "package a\n"}, "a.go\n")
	if res := check.Run(context.Background(), env); !res.OK {
		t.Fatalf("must_not_match should pass when the pattern is absent: %+v", res)
	}
}

func TestRuleMustMatchPresent(t *testing.T) {
	r := Rule{Name: "license", Pattern: `SPDX-License-Identifier`, Sense: "must_match", Scope: "tree", Message: "need a license header", Tier: "pre-commit"}
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "// SPDX-License-Identifier: MIT\npackage a\n"}, "a.go\n")
	if res := check.Run(context.Background(), env); !res.OK {
		t.Fatalf("must_match should pass when the pattern is present: %+v", res)
	}
}

func TestRuleMustMatchFilesButNoneMatch(t *testing.T) {
	r := Rule{Name: "license", Pattern: `SPDX-License-Identifier`, Sense: "must_match", Scope: "tree", Message: "need a license header", Tier: "pre-commit"}
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "package a\n"}, "a.go\n")
	res := check.Run(context.Background(), env)
	if res.OK {
		t.Fatalf("must_match should FAIL when files are in scope but none match")
	}
	if !strings.Contains(res.Output, "none matched") {
		t.Fatalf("must_match failure should say files were scanned but none matched: %+v", res.Output)
	}
}

func TestRuleMustMatchNoFilesInScope(t *testing.T) {
	// list returns a file, but the paths glob excludes it → nothing scanned.
	r := Rule{Name: "license", Pattern: `X`, Sense: "must_match", Scope: "tree", Paths: []string{"src/**"}, Message: "need X", Tier: "pre-commit"}
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "package a\n"}, "a.go\n")
	res := check.Run(context.Background(), env)
	if res.OK {
		t.Fatalf("must_match with no files in scope must NOT be a pass-by-vacuity")
	}
	if !strings.Contains(res.Output, "NO files in scope") {
		t.Fatalf("must_match with empty scope should say so distinctly: %+v", res.Output)
	}
}

func TestRulePathsFilter(t *testing.T) {
	r := Rule{Name: "no-todo", Pattern: `TODO`, Sense: "must_not_match", Scope: "tree", Paths: []string{"*.go"}, Message: "no TODO in go", Tier: "pre-commit"}
	// b.txt has TODO but is out of the *.go glob → not scanned; a.go is clean.
	check, env := ruleCheckOn(t, r, map[string]string{"a.go": "package a\n", "b.txt": "TODO fix\n"}, "a.go\nb.txt\n")
	if res := check.Run(context.Background(), env); !res.OK {
		t.Fatalf("paths glob should exclude b.txt from the scan: %+v", res)
	}
}

func TestRuleScopeDiffUsesStagedList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("import \"C\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var gotArgs []string
	f := &fakeRunner{respond: func(name string, args []string) (string, int, error) {
		if name == "git" {
			gotArgs = args
			return "a.go\n", 0, nil
		}
		return "", 0, nil
	}}
	r := Rule{Name: "cgo", Pattern: `import "C"`, Sense: "must_not_match", Scope: "diff", Message: "no C", Tier: "pre-commit"}
	check := findCheck(t, RulesChecks(&Config{Rules: []Rule{r}}), "rule:cgo")
	if res := check.Run(context.Background(), f.env(dir)); res.OK {
		t.Fatalf("diff-scope rule should still detect the staged C import")
	}
	if len(gotArgs) == 0 || gotArgs[0] != "diff" || !containsArg(gotArgs, "--cached") {
		t.Fatalf("diff scope must consult git diff --cached: %+v", gotArgs)
	}
}

func TestRuleGitError(t *testing.T) {
	f := &fakeRunner{respond: func(_ string, _ []string) (string, int, error) { return "", -1, errors.New("git gone") }}
	r := Rule{Name: "x", Pattern: `y`, Sense: "must_not_match", Scope: "tree", Message: "m", Tier: "pre-commit"}
	check := findCheck(t, RulesChecks(&Config{Rules: []Rule{r}}), "rule:x")
	if res := check.Run(context.Background(), f.env("/repo")); res.Err == nil {
		t.Fatalf("a git exec error should surface: %+v", res)
	}
}

func TestLoadRulesValidAndInvalid(t *testing.T) {
	valid := `stack: go
units: [{dir: .}]
rules:
  - name: cgo-free
    pattern: 'import "C"'
    paths: ["**/*.go"]
    sense: must_not_match
    scope: tree
    message: no cgo
    tier: pre-commit
`
	cfg, err := Load(writeConfig(t, valid))
	if err != nil {
		t.Fatalf("valid rule should Load: %v", err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "cgo-free" || cfg.Rules[0].Sense != "must_not_match" {
		t.Fatalf("rule not parsed: %+v", cfg.Rules)
	}
	bad := map[string]string{
		"no name":    "stack: go\nunits: [{dir: .}]\nrules:\n  - { pattern: x, sense: must_match, message: m, tier: pre-commit }\n",
		"no pattern": "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, sense: must_match, message: m, tier: pre-commit }\n",
		"bad regex":  "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, pattern: \"[bad\", sense: must_match, message: m, tier: pre-commit }\n",
		"bad sense":  "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, pattern: x, sense: maybe, message: m, tier: pre-commit }\n",
		"bad scope":  "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, pattern: x, sense: must_match, scope: sideways, message: m, tier: pre-commit }\n",
		"no message": "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, pattern: x, sense: must_match, tier: pre-commit }\n",
		"bad tier":   "stack: go\nunits: [{dir: .}]\nrules:\n  - { name: r, pattern: x, sense: must_match, message: m, tier: whenever }\n",
	}
	for name, body := range bad {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected a Load error, got nil", name)
		}
	}
}
