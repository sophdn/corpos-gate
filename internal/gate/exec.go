package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner runs a command in a working directory and returns its combined
// stdout+stderr, its process exit code, and an error that is non-nil
// ONLY when the command could not be started (e.g. binary not found).
// A command that starts and exits non-zero returns (output, code, nil)
// so checks can distinguish "tool missing" from "tool ran and failed".
//
// It is injected via RunEnv so a check's command CONSTRUCTION can be
// unit-tested with a fake Runner without executing anything (sans-IO).
type Runner func(ctx context.Context, dir, name string, args ...string) (output string, code int, err error)

// OSRunner is the real os/exec implementation of Runner. stdout and
// stderr are merged into one buffer so a check's reported Output holds
// everything the tool printed.
func OSRunner(ctx context.Context, dir, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// The command ran and exited non-zero — a check-level
			// failure, not an exec failure. Surface the code, clear err.
			return buf.String(), exit.ExitCode(), nil
		}
		// Could not start the command at all (binary missing, dir gone).
		return buf.String(), -1, err
	}
	return buf.String(), 0, nil
}

// DefaultLookupTool resolves an auxiliary tool binary (e.g.
// golangci-lint) to an absolute path, probing PATH first, then $GOBIN,
// then $(go env GOPATH)/bin, then ~/go/bin. Returns (path, true) on the
// first hit, ("", false) if none.
func DefaultLookupTool(name string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	var candidates []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		candidates = append(candidates, filepath.Join(gobin, name))
	}
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if gopath := strings.TrimSpace(string(out)); gopath != "" {
			candidates = append(candidates, filepath.Join(gopath, "bin", name))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "go", "bin", name))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, true
		}
	}
	return "", false
}
