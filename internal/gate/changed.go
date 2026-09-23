package gate

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
)

// changedGoPackages maps the repo's currently-changed Go files to the set
// of `./`-relative package import patterns under a unit, deduped and
// sorted. It runs `git diff --name-only HEAD` (unstaged + committed drift
// vs HEAD) and `git diff --name-only --cached` (staged) in the repo root,
// keeps the `*.go` paths that fall under unit.Dir, and collapses each to
// its package directory as a `./…` pattern (the unit root maps to ".").
//
// It is used to SCOPE a pre-commit race run to only what changed, so the
// data-race detector (which roughly doubles suite runtime) stays off the
// hot pre-commit path for the whole tree. An empty result means nothing
// Go changed → the caller SKIPs the race run.
//
// All IO flows through env.runner(), so the mapping is hermetically
// testable with a fake Runner (fake `git diff` output → expected package
// set) — this is why it takes (ctx, env, unit) rather than bare path
// strings.
func changedGoPackages(ctx context.Context, env RunEnv, unit Unit) ([]string, error) {
	run := env.runner()
	var files []string
	for _, args := range [][]string{
		{"diff", "--name-only", "HEAD"},
		{"diff", "--name-only", "--cached"},
	} {
		out, _, err := run(ctx, env.RepoRoot, "git", args...)
		if err != nil {
			return nil, err
		}
		files = append(files, splitLines(out)...)
	}

	// Prefix that a repo-root-relative path must carry to belong to this
	// unit. A "." (or empty) unit dir spans the whole repo, so no prefix.
	prefix := ""
	if unit.Dir != "." && unit.Dir != "" {
		prefix = strings.TrimSuffix(unit.Dir, "/") + "/"
	}

	seen := map[string]bool{}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		rel := f
		if prefix != "" {
			if !strings.HasPrefix(f, prefix) {
				continue
			}
			rel = strings.TrimPrefix(f, prefix)
		}
		dir := filepath.Dir(rel)
		pat := "."
		if dir != "." && dir != "" {
			pat = "./" + dir
		}
		seen[pat] = true
	}

	pkgs := make([]string, 0, len(seen))
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// splitLines splits git output into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}
