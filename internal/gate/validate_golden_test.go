package gate

import "testing"

// TestValidateErrorGolden pins validate's exact message for each rejection
// class (one fault per config), so a restructure cannot drift the wording.
func TestValidateErrorGolden(t *testing.T) {
	unit := []Unit{{Dir: "."}}
	okRule := Rule{Name: "r", Pattern: "x", Sense: "must_match", Message: "m", Tier: "pre-commit"}
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"valid", Config{Stack: "go", Units: unit, Rules: []Rule{okRule}}, ""},
		{"unknown stack", Config{Stack: "cobol", Units: unit}, `unknown stack "cobol" (want go, ts, py, or shell)`},
		{"no units", Config{Stack: "go"}, "at least one unit is required"},
		{"unit dir", Config{Stack: "go", Units: []Unit{{Dir: "."}, {}}}, "unit 1: dir is required"},
		{"unknown check", Config{Stack: "go", Units: unit, Checks: map[string]CheckConfig{"nope": {}}}, `unknown check "nope"`},
		{"check tier", Config{Stack: "go", Units: unit, Checks: map[string]CheckConfig{"vet": {Tier: "whenever"}}}, `check "vet": ` + tierErr(t, "whenever")},
		{"coverage metric", Config{Stack: "go", Units: unit, Checks: map[string]CheckConfig{"coverage": {Tier: "pre-push", Thresholds: map[string]float64{"vibes": 1}}}},
			`check "coverage": unknown coverage metric "vibes" in thresholds (want statements, branches, functions, or lines)`},
		{"coverage runner", Config{Stack: "go", Units: unit, Checks: map[string]CheckConfig{"coverage": {Tier: "pre-push", Runner: "mocha"}}},
			`check "coverage": unknown coverage runner "mocha" (want vitest or jest)`},
		{"custom name", Config{Stack: "go", Units: unit, Custom: []Custom{{Cmd: "true"}}}, "custom 0: name is required"},
		{"custom cmd", Config{Stack: "go", Units: unit, Custom: []Custom{{Name: "c"}}}, `custom "c": cmd is required`},
		{"custom tier", Config{Stack: "go", Units: unit, Custom: []Custom{{Name: "c", Cmd: "true", Tier: "whenever"}}}, `custom "c": ` + tierErr(t, "whenever")},
		{"rule name", Config{Stack: "go", Units: unit, Rules: []Rule{{Pattern: "x"}}}, "rule 0: name is required"},
		{"rule pattern", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r"}}}, `rule "r": pattern is required`},
		{"rule regex", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r", Pattern: "[bad"}}}, `rule "r": invalid pattern: error parsing regexp: missing closing ]: ` + "`[bad`"},
		{"rule sense", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r", Pattern: "x", Sense: "maybe"}}}, `rule "r": sense must be must_match or must_not_match`},
		{"rule scope", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r", Pattern: "x", Sense: "must_not_match", Scope: "sideways"}}}, `rule "r": scope must be tree or diff`},
		{"rule message", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r", Pattern: "x", Sense: "must_match", Scope: "diff"}}}, `rule "r": message is required`},
		{"rule tier", Config{Stack: "go", Units: unit, Rules: []Rule{{Name: "r", Pattern: "x", Sense: "must_match", Message: "m", Tier: "whenever"}}}, `rule "r": ` + tierErr(t, "whenever")},
	}
	for _, c := range cases {
		err := c.cfg.validate()
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func tierErr(t *testing.T, tier string) string {
	t.Helper()
	_, err := ParseTier(tier)
	if err == nil {
		t.Fatalf("ParseTier(%q) should fail", tier)
	}
	return err.Error()
}
