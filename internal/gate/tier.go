package gate

import "fmt"

// Tier names the gate stage a check belongs to. The ordering is a
// strict superset relationship: ci is a SUPERSET of pre-push, which is a
// SUPERSET of pre-commit.
type Tier int

const (
	// TierPreCommit checks are the fast gate — format, vet, lint, build,
	// plus repo-specific custom guards. Run on every commit. Keep this
	// tier fast (target: well under ~20s); slow checks belong at
	// pre-push or ci.
	TierPreCommit Tier = iota
	// TierPrePush checks are the slow gate — the full test suite,
	// coverage floor, vuln scan. A pre-push run ALSO runs every
	// pre-commit check (the superset property).
	TierPrePush
	// TierCI checks are the slowest gate — reserved for checks too slow
	// even for pre-push (e.g. mutation testing). A ci run runs EVERY
	// check: ci ⊃ pre-push ⊃ pre-commit. Nothing runs a ci-tier check on
	// pre-commit or pre-push.
	TierCI
)

// String renders the canonical gate.yml spelling of a tier.
func (t Tier) String() string {
	switch t {
	case TierPreCommit:
		return "pre-commit"
	case TierPrePush:
		return "pre-push"
	case TierCI:
		return "ci"
	default:
		return fmt.Sprintf("tier(%d)", int(t))
	}
}

// Includes reports whether a run at tier `run` should execute a check
// tiered at `check`. A ci run includes pre-push AND pre-commit; a
// pre-push run includes pre-commit and pre-push; a pre-commit run
// includes only pre-commit. Because TierPreCommit < TierPrePush < TierCI,
// the relation is exactly `run >= check`.
func (run Tier) Includes(check Tier) bool {
	return run >= check
}

// ParseTier maps a gate.yml tier string to a Tier. Unknown strings are
// an error so a typo in the config fails loudly at Load time rather
// than silently mis-tiering a check.
func ParseTier(s string) (Tier, error) {
	switch s {
	case "pre-commit":
		return TierPreCommit, nil
	case "pre-push":
		return TierPrePush, nil
	case "ci":
		return TierCI, nil
	default:
		return TierPreCommit, fmt.Errorf("unknown tier %q (want pre-commit, pre-push, or ci)", s)
	}
}

// tierOf parses a tier string that has already passed Load validation,
// so an unknown value here is unreachable; it defaults to pre-commit
// rather than panicking.
func tierOf(s string) Tier {
	t, err := ParseTier(s)
	if err != nil {
		return TierPreCommit
	}
	return t
}
