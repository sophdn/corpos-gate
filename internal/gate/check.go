package gate

import (
	"context"
	"io"
	"os"
	"time"
)

// Result is the outcome of running a single Check.
type Result struct {
	Name     string        // check name, e.g. "format", "coverage", a custom name
	Tier     Tier          // the tier the check is registered at
	OK       bool          // true if the check passed (a skipped check is also OK)
	Skipped  bool          // true if the check was skipped (e.g. tool absent)
	Output   string        // captured combined stdout+stderr / explanatory text
	Report   bool          // true if Output is a report to surface even when OK (e.g. mutation)
	Err      error         // non-nil only on an infrastructure failure to run
	Duration time.Duration // wall-clock time the check took
}

// Check is one gate step. Run must be deterministic in its command
// construction — all IO flows through the injected RunEnv.Run so the
// construction can be exercised with a fake Runner.
type Check interface {
	Name() string
	Tier() Tier
	Run(ctx context.Context, env RunEnv) Result
}

// RunEnv carries the ambient inputs a Check needs: the repo root, the
// command Runner (injected so tests can fake it), an auxiliary-tool
// resolver, the progress-output sink, and the fail-fast toggle. Nil
// function/writer fields fall back to real defaults, so a caller can
// construct `RunEnv{RepoRoot: root}` and get production behavior.
type RunEnv struct {
	// RepoRoot is the absolute path to the repository root (the dir
	// holding gate.yml). Unit dirs are resolved relative to it; custom
	// checks run their shell command here.
	RepoRoot string
	// Run executes commands. nil → OSRunner.
	Run Runner
	// LookupTool resolves auxiliary binaries (e.g. golangci-lint). nil →
	// DefaultLookupTool.
	LookupTool func(name string) (string, bool)
	// Out receives the per-check progress lines. nil → os.Stdout.
	Out io.Writer
	// KeepGoing disables fail-fast: when true, Run executes every
	// selected check instead of stopping at the first failure. The
	// zero value (false) is the default fail-fast behavior.
	KeepGoing bool
}

func (e RunEnv) runner() Runner {
	if e.Run != nil {
		return e.Run
	}
	return OSRunner
}

func (e RunEnv) lookup(name string) (string, bool) {
	if e.LookupTool != nil {
		return e.LookupTool(name)
	}
	return DefaultLookupTool(name)
}

func (e RunEnv) out() io.Writer {
	if e.Out != nil {
		return e.Out
	}
	return os.Stdout
}

// funcCheck adapts a name, tier, and closure into a Check. It keeps the
// adapters declarative — each check is a closure over its own command
// construction — while sharing one interface implementation.
type funcCheck struct {
	name string
	tier Tier
	run  func(ctx context.Context, env RunEnv) Result
}

func (c funcCheck) Name() string { return c.name }
func (c funcCheck) Tier() Tier   { return c.tier }
func (c funcCheck) Run(ctx context.Context, env RunEnv) Result {
	return c.run(ctx, env)
}
