package verify

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func changed(path string) importer.Change {
	return importer.Change{Path: path, Kind: importer.ChangeModified, Mode: "100644", Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
}

// TestFlagControlPathsDefaults exercises every default pattern plus the
// adversarial alias space (case, Unicode folding, trailing dot/space,
// ADS suffix, HFS-ignorables): the class must fire on the name a
// downstream checkout would materialize.
func TestFlagControlPathsDefaults(t *testing.T) {
	flagged := []string{
		"go.mod",
		"go.sum",
		"go.work",
		"go.work.sum",
		"daemon/go.mod",
		"deep/nested/module/go.sum",
		"Makefile",
		"GNUmakefile",
		"makefile",
		"build/rules.mk",
		"justfile",
		"Taskfile.yml",
		"Taskfile.yaml",
		".golangci.yml",
		"daemon/.golangci.yml",
		".golangci.yaml",
		".golangci.json",
		".golangci.toml",
		testRecipePath,
		// Alias space: each must fold or normalize onto a class member.
		"GO.MOD",
		"MAKEFILE",
		"Makefile.",
		"Makefile ",
		"Makefile::$DATA",
		".golangci.yml\u200d",
		"daemon/.GOLANGCI.YML",
		"\ufeffgo.mod",
	}
	clean := []string{
		"README.md",
		"main.go",
		"docs/plan.md",
		"go.mod.md",
		"ago.mod",
		"Makefile.md",
		"internal/verify/verify.go",
		".golangci.yml.bak",
	}
	for _, p := range flagged {
		if got := flagControlPaths([]importer.Change{changed(p)}, nil, testRecipePath); len(got) != 1 || got[0].Kind != FindingVerificationControlPath {
			t.Errorf("%q: findings = %v, want one verification_control_path", p, got)
		}
	}
	for _, p := range clean {
		if got := flagControlPaths([]importer.Change{changed(p)}, nil, testRecipePath); len(got) != 0 {
			t.Errorf("%q: findings = %v, want none", p, got)
		}
	}
}

// TestFlagControlPathsDeletions pins that deletions are flagged:
// removing a Makefile or the recipe steers verification exactly as
// rewriting one does.
func TestFlagControlPathsDeletions(t *testing.T) {
	del := importer.Change{Path: "Makefile", Kind: importer.ChangeDeleted}
	got := flagControlPaths([]importer.Change{del}, nil, testRecipePath)
	if len(got) != 1 || got[0].Detail != "deleted" {
		t.Fatalf("findings = %v, want one deleted flag", got)
	}
}

// TestFlagControlPathsNonUTF8 pins that a non-canonical path is flagged
// unconditionally: an unmatchable name must not bypass the class.
func TestFlagControlPathsNonUTF8(t *testing.T) {
	raw := importer.Change{PathHex: "676f2e6d6f64ff", Kind: importer.ChangeAdded}
	got := flagControlPaths([]importer.Change{raw}, nil, testRecipePath)
	if len(got) != 1 || got[0].PathHex != raw.PathHex || got[0].Path != "" {
		t.Fatalf("findings = %v, want one path_hex flag", got)
	}
}

// TestVerificationControlWidenOnly is the immutability rule: caller
// widening is added, and no extra set can drop a default or the recipe
// path from the class.
func TestVerificationControlWidenOnly(t *testing.T) {
	for _, extra := range [][]string{nil, {}, {"**/scripts/verify.sh"}} {
		patterns := verificationControl(extra, testRecipePath)
		for _, want := range DefaultVerificationControlPatterns {
			if !slices.Contains(patterns, want) {
				t.Errorf("extra %v: class dropped default %q", extra, want)
			}
		}
		if !slices.Contains(patterns, testRecipePath) {
			t.Errorf("extra %v: class dropped the recipe path", extra)
		}
	}
	got := flagControlPaths([]importer.Change{changed("scripts/verify.sh"), changed("go.mod")}, []string{"**/scripts/verify.sh"}, testRecipePath)
	if len(got) != 2 {
		t.Fatalf("findings = %v, want the widened path and the default both flagged", got)
	}
}

func TestFlagControlPathsGolden(t *testing.T) {
	changes := []importer.Change{
		changed("go.mod"),
		{Path: "Makefile", Kind: importer.ChangeDeleted},
		changed(testRecipePath),
		changed("README.md"),
		{PathHex: "676f2e6d6f64ff", Kind: importer.ChangeAdded},
	}
	findings := flagControlPaths(changes, nil, testRecipePath)
	got, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "flag_control_paths", append(got, '\n'))
}
