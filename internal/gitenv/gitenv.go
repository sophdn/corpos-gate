package gitenv

import (
	"os"
	"strings"
)

// ContextVars are the exact environment variable names stripped before a git
// command runs. The indexed GIT_CONFIG_KEY_<n>/VALUE_<n> forms are handled by
// prefix in Clean, since their names carry an arbitrary index.
var ContextVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_PREFIX",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
}

// IndexedPrefixes are the variable-name prefixes stripped by prefix match.
// GIT_CONFIG_COUNT gates the indexed array form, so stripping the count alone
// neutralizes it — the indexed vars are dropped too so nothing lingers for a
// tool that reads them directly.
var IndexedPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

// Clean returns the process environment minus the git context channels — the
// environment a git command must run in to be certain it operates on the
// directory it was pointed at, and nothing else.
//
// Use it for EVERY exec of git in a test helper. Setting cmd.Dir or passing
// `-C <dir>` is NOT sufficient on its own: the stripped variables outrank both.
func Clean() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if stripped(kv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stripped reports whether an environment entry is one of the git context
// channels.
func stripped(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	for _, v := range ContextVars {
		if name == v {
			return true
		}
	}
	for _, p := range IndexedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
