// Package gitenv is the ONE shared definition of the git environment scrubbing
// this repo's git-invoking code needs.
//
// git exports its configuration-injection channels into every child process a
// HOOK runs, and those channels OVERRIDE `git -C <dir>` and exec.Cmd.Dir. This
// repo's pre-commit gate IS a git hook and it runs the test suite, so a helper
// that runs `git init` + `git config user.email <fixture>` in a temp dir does not
// create a repo in that temp dir at all when the suite runs under the gate: it
// RE-INITIALIZES the repository being committed to and writes the fixture
// identity into ITS config.
//
// ## Intended use
//
// **Workflow served:** any code that shells out to git — production helpers and,
// especially, test helpers that build throwaway repos — needs certainty that the
// directory it named is the directory git operates on. Setting cmd.Dir or passing
// `-C dir` does NOT provide that on its own. This package is what does.
//
// **Invocation pattern:** assign the result to the command's environment before
// running it: cmd := exec.Command("git", …); cmd.Dir = dir; cmd.Env =
// gitenv.Clean(). When the caller needs to ADD variables (a hermetic author
// identity, say), append to it — append(gitenv.Clean(), "GIT_AUTHOR_NAME=…") —
// rather than to os.Environ(), which would carry the redirection channels back in.
//
// **Success shape:** Clean returns the process environment minus the context
// channels, leaving everything else untouched. The scrub is surgical on purpose:
// stripping more would break commands for unrelated reasons, and stripping less
// leaves the redirection open.
//
// **Non-goals:** does not neutralize the GLOBAL or SYSTEM git config (a caller
// wanting that sets GIT_CONFIG_GLOBAL=/dev/null itself), does not choose an
// author identity, does not run git, and does not detect a repo that has ALREADY
// been polluted — that is the corpos-gate `repo-identity` check, declared in
// gate.yml.
//
// # Why this exists as a package rather than a helper
//
// A private copy inside one test file leaves every OTHER git-invoking helper in
// the repo exposed, and the leak is silent: a repo-local config beats global and
// git warns about nothing at commit time, so a fixture identity written into
// .git/config misattributes every commit until someone reads
// `git log --format=%ae`. One shared definition is the only way to keep all the
// callers in step.
//
// Two channels the scrub covers:
//   - GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE / GIT_COMMON_DIR / GIT_PREFIX:
//     set when the suite runs inside a worktree checkout's pre-commit hook. These
//     are what redirect a command at the wrong repository.
//   - GIT_CONFIG_PARAMETERS / GIT_CONFIG_COUNT / GIT_CONFIG_KEY_<n> /
//     GIT_CONFIG_VALUE_<n>: exported by a gate-only worktree commit path's
//     `git -c core.hooksPath=… commit`. An inherited core.hooksPath makes a child
//     commit exec a hook that does not exist under the temp repo.
package gitenv
