package main

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

// The defaults name the same host state the README's hand setup made, so an
// install over it adopts rather than duplicates it.
func TestParseMacverifySpecDefaults(t *testing.T) {
	t.Parallel()
	spec, err := parseMacverifySpec("install", nil, "benja")
	if err != nil {
		t.Fatal(err)
	}
	want := macverify.InstallSpec{
		Root: "/Library/Freeside/macverify", SlotPrefix: "fvslot", SlotCount: 2, Group: "fvslots", FirstID: 460,
		Operator: "benja", Anchor: "freeside-macverify", Label: "ai.freeside.macverify.pf",
		SudoersName: "freeside-slot-run", PortBase: macverify.DefaultPortBase,
	}
	if spec != want {
		t.Fatalf("spec = %+v, want %+v", spec, want)
	}
}

func TestRunMacverifyCommandUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"bogus"}, {"check", "extra"}, {"install", "-slots", "x"}} {
		if err := runMacverifyCommand(args, nil, nil, "benja"); !errors.Is(err, errMacverifyUsage) {
			t.Errorf("runMacverifyCommand(%q) = %v, want a usage error", args, err)
		}
	}
}
