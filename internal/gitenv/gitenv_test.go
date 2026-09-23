package gitenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanStripsEveryContextChannel(t *testing.T) {
	for _, name := range ContextVars {
		t.Setenv(name, "/some/value")
	}
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/some/hooks")
	t.Setenv("GIT_AUTHOR_NAME", "kept") // NOT a context channel — must survive
	t.Setenv("EXAMPLE_KEEP_ME", "yes")

	got := Clean()
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		for _, bad := range ContextVars {
			if name == bad {
				t.Errorf("%s survived Clean()", bad)
			}
		}
		for _, p := range IndexedPrefixes {
			if strings.HasPrefix(name, p) {
				t.Errorf("%s survived Clean()", name)
			}
		}
	}

	// The scrub must be surgical: unrelated variables, and git variables that
	// are NOT redirection channels, have to survive or the command runs in a
	// stripped-down environment that breaks for unrelated reasons.
	joined := strings.Join(got, "\n")
	for _, want := range []string{"GIT_AUTHOR_NAME=kept", "EXAMPLE_KEEP_ME=yes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Clean() dropped %q, which is not a context channel", want)
		}
	}
}

// TestCleanIsWhatMakesCmdDirAuthoritative is the regression test for the actual
// damage: with GIT_DIR/GIT_WORK_TREE exported, `git init` in a temp dir does not
// create a repo there — it re-initializes the pointed-at repo — and a subsequent
// `git config` writes into THAT repo's config.
//
// The test builds a canary repo, points the context channels at it, and proves
// both halves: unscrubbed leaks into the canary, scrubbed does not.
func TestCleanIsWhatMakesCmdDirAuthoritative(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// SETUP IS SCRUBBED, deliberately. This suite itself runs under the repo's
	// pre-commit hook, so an unscrubbed setup call here would write into the
	// repository being committed to — the very damage under test. Only the
	// demonstration below runs unscrubbed, and only AFTER t.Setenv has pointed
	// the context channels at this test's own canary, so the worst it can do is
	// modify a temp dir.
	canary := t.TempDir()
	mustGit(t, canary, Clean(), "init", "-q")
	mustGit(t, canary, Clean(), "config", "user.email", "canary@example.invalid")

	// Point the context channels at the canary, exactly as a pre-commit hook
	// would for the repository it is committing to. From here on, "inherited"
	// means inherited from THIS test.
	t.Setenv("GIT_DIR", filepath.Join(canary, ".git"))
	t.Setenv("GIT_WORK_TREE", canary)

	t.Run("UNSCRUBBED writes land in the canary, not the target", func(t *testing.T) {
		target := t.TempDir()
		// Deliberately no cmd.Env: this is the bug shape.
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = target
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
			t.Skip("this git honours cmd.Dir over GIT_DIR; the hazard does not reproduce here")
		}

		cmd = exec.Command("git", "config", "user.email", "leaked@example.invalid")
		cmd.Dir = target
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
		if got := gitConfigEmail(t, canary); got != "leaked@example.invalid" {
			t.Fatalf("expected the unscrubbed write to hit the canary; canary email = %q", got)
		}
	})

	t.Run("SCRUBBED writes land in the target", func(t *testing.T) {
		// Reset the canary so this subtest is independent of the one above.
		mustGit(t, canary, Clean(), "config", "user.email", "canary@example.invalid")

		target := t.TempDir()
		mustGit(t, target, Clean(), "init", "-q")
		if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
			t.Fatalf("scrubbed git init must create a repo in the target dir: %v", err)
		}
		mustGit(t, target, Clean(), "config", "user.email", "target@example.invalid")

		if got := gitConfigEmail(t, canary); got != "canary@example.invalid" {
			t.Errorf("the canary was modified despite the scrub: %q", got)
		}
		if got := gitConfigEmail(t, target); got != "target@example.invalid" {
			t.Errorf("target email = %q, want the value we set", got)
		}
	})
}

func mustGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gitConfigEmail(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--get", "user.email")
	cmd.Dir = dir
	cmd.Env = Clean()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
