package gate

import (
	"context"
	"fmt"
	"time"
)

// PlanEntry is one line of a resolved gate plan: the check name and the
// tier it is registered at, in execution order.
type PlanEntry struct {
	Name string
	Tier Tier
}

// skipSet turns a skip name list into a lookup set. A nil/empty list
// yields a nil map, and lookups against a nil map are always false — so
// the no-skip path costs nothing.
func skipSet(skip []string) map[string]bool {
	if len(skip) == 0 {
		return nil
	}
	s := make(map[string]bool, len(skip))
	for _, name := range skip {
		s[name] = true
	}
	return s
}

// adapterChecks returns the stack-specific adapter checks for cfg:
// GoChecks for "go", TSChecks for "ts", PyChecks for "py", and an empty
// set for "shell" (custom-only, no adapter). The stack has already passed
// Load validation, so an unrecognized value here is unreachable and
// yields no adapter checks (custom checks still run).
func adapterChecks(cfg *Config) []Check {
	switch cfg.Stack {
	case "go":
		return GoChecks(cfg)
	case "ts":
		return TSChecks(cfg)
	case "py":
		return PyChecks(cfg)
	default:
		// "shell" (and any unreachable value): custom-only.
		return nil
	}
}

// selectChecks assembles the adapter checks followed by the custom
// checks, then filters to those whose tier is included by runTier and
// whose name is NOT in skip. The adapters already omit disabled checks,
// so the filters here are the tier-superset relation plus the explicit
// skip set. Order is stable: adapter checks in adapter order, then custom
// checks in config order.
func selectChecks(cfg *Config, runTier Tier, skip []string) []Check {
	skipped := skipSet(skip)
	// Git-level checks first (secrets, repo-identity): they are the cheapest and
	// most safety-critical, stack-agnostic, and run before the adapter suite so a
	// secret is caught before the slow stages. Then the adapter checks, then the
	// repo-specific custom checks.
	all := append(GitChecks(cfg), adapterChecks(cfg)...)
	all = append(all, RulesChecks(cfg)...)
	all = append(all, CustomChecks(cfg)...)
	var selected []Check
	for _, c := range all {
		if skipped[c.Name()] {
			continue
		}
		if runTier.Includes(c.Tier()) {
			selected = append(selected, c)
		}
	}
	return selected
}

// Plan returns the ordered check plan for runTier WITHOUT executing
// anything, omitting any check whose name is in skip. It is the dry-run
// projection the CLI's `plan` subcommand prints and the superset proof
// relies on.
func Plan(cfg *Config, runTier Tier, skip []string) []PlanEntry {
	checks := selectChecks(cfg, runTier, skip)
	entries := make([]PlanEntry, 0, len(checks))
	for _, c := range checks {
		entries = append(entries, PlanEntry{Name: c.Name(), Tier: c.Tier()})
	}
	return entries
}

// Run executes the checks selected for runTier in stable order,
// omitting any check whose name is in skip, and printing a per-check
// progress line to env.Out. By default it fail-fasts — stopping at the
// first failing (non-skipped) check; env.KeepGoing runs them all. It
// returns every result produced, an overall ok (true iff no check
// failed), and a non-nil err only when a check hit an infrastructure
// error (could not run its command).
func Run(ctx context.Context, cfg *Config, runTier Tier, env RunEnv, skip []string) ([]Result, bool, error) {
	checks := selectChecks(cfg, runTier, skip)
	out := env.out()
	results := make([]Result, 0, len(checks))
	ok := true
	for _, c := range checks {
		r := c.Run(ctx, env)
		results = append(results, r)
		fmt.Fprintf(out, "[corpos-gate] %s ... %s (%s)\n", r.Name, status(r), fmtDur(r.Duration))
		if r.Err != nil {
			// Infrastructure failure — report and stop; the check
			// could not run, so its pass/fail is undefined.
			fmt.Fprintf(out, "[corpos-gate]   error: %v\n", r.Err)
			return results, false, fmt.Errorf("check %q could not run: %w", r.Name, r.Err)
		}
		if !r.OK && !r.Skipped {
			ok = false
			if r.Output != "" {
				fmt.Fprintf(out, "[corpos-gate]   %s\n", r.Output)
			}
			if !env.KeepGoing {
				return results, false, nil
			}
		} else if r.OK && !r.Skipped && r.Report && r.Output != "" {
			// A report-only check (e.g. mutation) always passes but its
			// Output IS the report — surface it, or the run nobody reads
			// would produce nothing visible.
			fmt.Fprintf(out, "[corpos-gate]   %s\n", r.Output)
		}
	}
	return results, ok, nil
}

// status maps a Result to its progress-line verdict.
func status(r Result) string {
	switch {
	case r.Skipped:
		return "SKIP"
	case r.OK:
		return "PASS"
	default:
		return "FAIL"
	}
}

// fmtDur renders a duration at millisecond resolution for progress lines.
func fmtDur(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
