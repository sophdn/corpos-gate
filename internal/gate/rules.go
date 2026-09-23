package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RulesChecks builds one Check per declarative Rule in cfg — the stack-agnostic
// text-invariant mechanism (see config.Rule). Each rule is DATA in gate.yml, so
// a repo's invariants stop multiplying as one-off bash guards, and they travel
// to any project it gates. selectChecks runs these after the adapter checks
// and before the custom checks.
func RulesChecks(cfg *Config) []Check {
	checks := make([]Check, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		checks = append(checks, newRuleCheck(r))
	}
	return checks
}

// newRuleCheck builds the Check for one Rule. The check name is "rule:<name>" so
// a rule failure is attributed to its own name and message, not a raw grep dump.
// A must_not_match rule FAILS when the pattern appears anywhere in scope
// (listing the hits); a must_match rule FAILS when it appears nowhere, and the
// message distinguishes "no files in scope" from "files in scope, none matched"
// so a must_match rule is never a silent pass-by-vacuity.
func newRuleCheck(r Rule) Check {
	name := "rule:" + r.Name
	tier := tierOf(r.Tier)
	re := regexp.MustCompile(r.Pattern) // compiled once; validated at Load
	mustMatch := r.Sense == "must_match"
	return funcCheck{name, tier, func(ctx context.Context, env RunEnv) Result {
		start := time.Now()
		res := Result{Name: name, Tier: tier}
		files, err := ruleFiles(ctx, env, r.Scope, r.Paths)
		if err != nil {
			res.Err, res.Duration = err, time.Since(start)
			return res
		}
		var hits []string
		matchedAny, scanned := false, 0
		for _, f := range files {
			data, rerr := os.ReadFile(filepath.Join(env.RepoRoot, f))
			if rerr != nil {
				continue // a staged-then-deleted or unreadable file: skip
			}
			scanned++
			for i, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) {
					matchedAny = true
					hits = append(hits, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
				}
			}
		}
		res.Duration = time.Since(start)
		if mustMatch {
			if matchedAny {
				res.OK = true
				return res
			}
			res.OK = false
			if scanned == 0 {
				res.Output = r.Message + "\n(must_match: NO files in scope — the pattern matched nothing because nothing was scanned, not because the invariant holds)"
			} else {
				res.Output = fmt.Sprintf("%s\n(must_match: %d file(s) in scope, none matched the pattern)", r.Message, scanned)
			}
			return res
		}
		// must_not_match
		if matchedAny {
			res.OK = false
			res.Output = r.Message + "\n" + strings.Join(hits, "\n")
			return res
		}
		res.OK = true
		return res
	}}
}

// ruleFiles returns the in-scope file paths for a rule. scope "diff" is the
// files staged in this commit (git diff --cached), anything else is the whole
// tracked tree (git ls-files); each is then filtered to the rule's path globs
// (empty → all files in scope).
func ruleFiles(ctx context.Context, env RunEnv, scope string, paths []string) ([]string, error) {
	var args []string
	if scope == "diff" {
		args = []string{"diff", "--cached", "--name-only", "--diff-filter=ACM"}
	} else {
		args = []string{"ls-files"}
	}
	out, code, err := env.runner()(ctx, env.RepoRoot, "git", args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("git %s failed: %s", strings.Join(args, " "), out)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if len(paths) == 0 || matchesAnyGlob(f, paths) {
			files = append(files, f)
		}
	}
	return files, nil
}
