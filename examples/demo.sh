#!/usr/bin/env bash
#
# corpos-gate demo: point it at throwaway apps and watch it align to each one,
# then catch a real problem and pass once it is fixed.
#
# Needs: go, and corpos-gate on PATH. Build the binary and prepend it to PATH:
#   go build -o /tmp/corpos-gate ./cmd/corpos-gate
#   PATH="/tmp:$PATH" ./examples/demo.sh
# or install it: go install github.com/sophdn/corpos-gate/cmd/corpos-gate@latest

set -u

if ! command -v corpos-gate >/dev/null 2>&1; then
    echo "corpos-gate is not on PATH. Build it first:" >&2
    echo "  go build -o /tmp/corpos-gate ./cmd/corpos-gate" >&2
    echo "  PATH=\"/tmp:\$PATH\" ./examples/demo.sh" >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
commit() { git -c user.email=demo@example.com -c user.name=demo commit -q "$@"; }
echo "demo workspace: $WORK"
echo

echo "=================================================================="
echo " 1. Go: init, catch a formatting error, then pass once it is fixed"
echo "=================================================================="
app="$WORK/go-app"
mkdir -p "$app"
cd "$app" || exit 1
git init -q
printf 'module example.com/demo\n\ngo 1.26\n' > go.mod
printf 'package main\n\nfunc main() {}\n' > main.go
git add -A
commit -m init

echo "--- corpos-gate init (note the detection summary) ---"
corpos-gate init
echo
echo "--- add a badly formatted, still-compiling file ---"
printf 'package main\n\nfunc   Helper()  int {  return  42  }\n' > messy.go
echo "\$ corpos-gate run --tier=pre-commit"
corpos-gate run --tier=pre-commit
echo ">>> exit $?  (non-zero: the gate caught the formatting)"
echo
echo "--- fix the formatting, run again ---"
gofmt -w messy.go
echo "\$ corpos-gate run --tier=pre-commit"
corpos-gate run --tier=pre-commit
echo ">>> exit $?  (zero: green)"
echo

echo "=================================================================="
echo " 2. TypeScript: init aligns to the app's own jest coverage config"
echo "=================================================================="
app="$WORK/ts-app"
mkdir -p "$app"
cd "$app" || exit 1
git init -q
cat > package.json <<'PKG'
{
  "name": "demo",
  "jest": {
    "coverageThreshold": {
      "global": { "statements": 80, "branches": 70, "functions": 85, "lines": 82 }
    }
  }
}
PKG
git add -A
commit -m init
echo "--- corpos-gate init (watch the coverage line) ---"
corpos-gate init
echo
echo "--- the generated gate.yml ---"
cat gate.yml
echo

echo "=================================================================="
echo " 3. Python: init aligns to the app's coverage.py fail_under"
echo "=================================================================="
app="$WORK/py-app"
mkdir -p "$app"
cd "$app" || exit 1
git init -q
printf '[tool.coverage.report]\nfail_under = 78\n' > pyproject.toml
git add -A
commit -m init
echo "--- corpos-gate init (watch the coverage line) ---"
corpos-gate init
echo
echo "--- the generated gate.yml ---"
cat gate.yml
echo
echo "done."
