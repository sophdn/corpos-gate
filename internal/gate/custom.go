package gate

import (
	"context"
	"time"
)

// CustomChecks builds a Check per custom entry in cfg. Each runs its
// command via `sh -c "<cmd>"` in the repo root and FAILS on a non-zero
// exit, capturing the combined output. This is the stack-agnostic
// escape hatch for repo-specific guards the adapters don't model.
func CustomChecks(cfg *Config) []Check {
	var checks []Check
	for _, cu := range cfg.Custom {
		cu := cu
		tier := tierOf(cu.Tier)
		checks = append(checks, funcCheck{cu.Name, tier, func(ctx context.Context, env RunEnv) Result {
			start := time.Now()
			out, code, err := env.runner()(ctx, env.RepoRoot, "sh", "-c", cu.Cmd)
			res := Result{Name: cu.Name, Tier: tier, Duration: time.Since(start), Output: out, Err: err}
			res.OK = err == nil && code == 0
			return res
		}})
	}
	return checks
}
