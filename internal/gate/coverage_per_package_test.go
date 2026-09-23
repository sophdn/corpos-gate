package gate

import (
	"fmt"
	"strings"
	"testing"
)

// prof builds a coverage profile body. Each entry is
// (file, numStmt, count) — count>0 means the block was executed.
func prof(entries ...[3]any) string {
	var b strings.Builder
	b.WriteString("mode: atomic\n")
	for i, e := range entries {
		fmt.Fprintf(&b, "%s:%d.1,%d.2 %d %d\n", e[0], i+1, i+1, e[1], e[2])
	}
	return b.String()
}

// THE POINT OF THE CHECK: an aggregate above the floor while one package
// sits below it, so the aggregate stays green while a package rots.
func TestPackagesBelowFloor_CatchesPackageMaskedByAggregate(t *testing.T) {
	// good: 990/1000 = 99%. bad: 5/10 = 50%. Aggregate 995/1010 = 98.5%.
	p := prof(
		[3]any{"m/internal/good/a.go", 990, 1},
		[3]any{"m/internal/good/b.go", 10, 0},
		[3]any{"m/internal/bad/a.go", 5, 1},
		[3]any{"m/internal/bad/b.go", 5, 0},
	)
	below, err := packagesBelowFloor(p, 95, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 1 {
		t.Fatalf("expected exactly the one bad package, got %+v", below)
	}
	if below[0].Package != "m/internal/bad" {
		t.Errorf("wrong package named: %+v", below[0])
	}
	if got := below[0].Percent; got < 49.9 || got > 50.1 {
		t.Errorf("expected ~50%%, got %.2f", got)
	}
}

// Statement-weighted, NOT the mean of per-function percentages. This is the
// case where the prototype's approach (averaging -func rows) would be wrong:
// a tiny fully-uncovered helper beside a large fully-covered file. Function
// averaging says ~50%; real statement coverage is 99%.
func TestPackagesBelowFloor_IsStatementWeightedNotFunctionAveraged(t *testing.T) {
	p := prof(
		[3]any{"m/internal/pkg/big.go", 297, 1}, // large, covered
		[3]any{"m/internal/pkg/tiny.go", 3, 0},  // small, uncovered
	)
	below, err := packagesBelowFloor(p, 95, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("99%% statement coverage must pass a 95%% floor; a function-averaged "+
			"reading would have wrongly failed it: %+v", below)
	}
}

func TestPackagesBelowFloor_AllPassing(t *testing.T) {
	p := prof(
		[3]any{"m/internal/a/x.go", 100, 1},
		[3]any{"m/internal/b/y.go", 100, 1},
	)
	below, err := packagesBelowFloor(p, 95, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("expected none below floor, got %+v", below)
	}
}

func TestPackagesBelowFloor_SortsWorstFirst(t *testing.T) {
	p := prof(
		[3]any{"m/internal/mid/a.go", 5, 1}, [3]any{"m/internal/mid/b.go", 5, 0}, // 50%
		[3]any{"m/internal/worst/a.go", 1, 1}, [3]any{"m/internal/worst/b.go", 9, 0}, // 10%
	)
	below, err := packagesBelowFloor(p, 95, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 2 || below[0].Package != "m/internal/worst" {
		t.Fatalf("expected worst-first ordering, got %+v", below)
	}
}

// An exemption lets a named package sit lower, and it is judged against ITS
// floor — including still failing if it drops below even that.
func TestPackagesBelowFloor_ExemptionAppliesItsOwnFloor(t *testing.T) {
	p := prof(
		[3]any{"m/internal/thin/a.go", 8, 1}, [3]any{"m/internal/thin/b.go", 2, 0}, // 80%
	)
	ex := []PackageExemption{{Package: "m/internal/thin", Floor: 75, Reason: "thin wrapper, integration-tested"}}
	below, err := packagesBelowFloor(p, 95, ex)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("80%% should pass an exempted floor of 75: %+v", below)
	}

	// Below even the exempted floor -> still reported, against that floor.
	ex[0].Floor = 90
	below, err = packagesBelowFloor(p, 95, ex)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 1 || below[0].Floor != 90 {
		t.Fatalf("expected a failure judged against the exempted floor 90, got %+v", below)
	}
}

func TestPackagesBelowFloor_ExemptionSupportsPrefixAndPrefersSpecific(t *testing.T) {
	p := prof(
		[3]any{"m/internal/gen/sub/a.go", 1, 1}, [3]any{"m/internal/gen/sub/b.go", 9, 0}, // 10%
	)
	ex := []PackageExemption{
		{Package: "m/internal/gen/...", Floor: 50, Reason: "generated"},
		{Package: "m/internal/gen/sub", Floor: 5, Reason: "generated, fully untested"},
	}
	below, err := packagesBelowFloor(p, 95, ex)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("the exact-match exemption (floor 5) should win over the prefix (50): %+v", below)
	}
}

// A reasonless exemption is a config error, not a quiet pass — otherwise the
// exemption list becomes the new place coverage silently erodes.
func TestPackagesBelowFloor_RejectsExemptionWithNoReason(t *testing.T) {
	p := prof([3]any{"m/internal/a/x.go", 1, 1})
	_, err := packagesBelowFloor(p, 95, []PackageExemption{{Package: "m/internal/a", Floor: 10}})
	if err == nil {
		t.Fatal("expected an error for a reasonless exemption")
	}
	if !strings.Contains(err.Error(), "no reason") {
		t.Errorf("error should say the reason is missing, got: %v", err)
	}
}

// A package with zero statements must not read as 0%.
func TestPackagesBelowFloor_IgnoresZeroStatementPackages(t *testing.T) {
	p := prof([3]any{"m/internal/empty/doc.go", 0, 0})
	below, err := packagesBelowFloor(p, 95, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("a statement-free package must not be reported as 0%%: %+v", below)
	}
}

// Absent config (floor 0) is aggregate-only — the prior behavior.
func TestPackagesBelowFloor_ZeroFloorReportsNothing(t *testing.T) {
	p := prof([3]any{"m/internal/a/x.go", 1, 0})
	below, err := packagesBelowFloor(p, 0, nil)
	if err != nil {
		t.Fatalf("packagesBelowFloor: %v", err)
	}
	if len(below) != 0 {
		t.Errorf("floor 0 gates nothing, got %+v", below)
	}
}
