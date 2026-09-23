// Package gate is corpos-gate's core: a reusable, stack-agnostic gate
// orchestrator that reads a declarative gate.yml, assembles the enabled
// checks for a run tier, and executes them in a stable, fail-fast order.
//
// ## Intended use
//
// **Workflow served:** a developer or agent about to commit/push runs
// one gate that every supported stack shares, instead of a per-repo
// bespoke precommit script. gate.yml declares the stack, build units,
// per-check enable/tier toggles, and repo-specific custom guards; the
// orchestrator turns that into an ordered check plan and runs the slice
// the tier selects (pre-push is a superset of pre-commit).
//
// **Invocation pattern:** `cfg, err := gate.Load("gate.yml")` then
// `results, ok, err := gate.Run(ctx, cfg, gate.TierPreCommit, env)`,
// where `env gate.RunEnv` injects the command Runner, tool locator, and
// output sink so command CONSTRUCTION is unit-testable without
// executing anything. `gate.Plan(cfg, tier)` returns the ordered plan
// without running it (the CLI's `plan` subcommand + the superset proof).
// Adapter checks are stack-dispatched: `gate.GoChecks(cfg)` for the go
// stack, `gate.TSChecks(cfg)` for the ts stack, none for shell
// (custom-only); custom guards come from `gate.CustomChecks(cfg)`.
//
// **Success shape:** one Result per executed check
// {Name, Tier, OK, Skipped, Output, Err, Duration}; overall ok is true
// iff no non-skipped check failed. Absent optional tooling (e.g.
// golangci-lint) yields a Skipped result rather than a failure. A
// non-nil Run error signals an infrastructure failure (a check could
// not run its command), distinct from a check that ran and failed.
//
// **Non-goals:** owns no git or staging logic — it only orchestrates
// checks; mutation testing is report-only, never a gate; the shell stack
// has no adapter (custom checks only).
package gate
