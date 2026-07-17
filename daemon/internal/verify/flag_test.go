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
		// Swift/Xcode verification-control surfaces (app lane, #140).
		"Package.swift",
		"app/Package.swift",
		"Package@swift-6.0.swift",
		"app/Package@swift-5.swift",
		"Package.resolved",
		"app/Package.resolved",
		".swiftpm/configuration/mirrors.json",
		"app/.swiftpm/configuration/mirrors.json",
		".swiftpm/configuration/registries.json",
		"Config/Debug.xcconfig",
		"app/Base.xcconfig",
		"Freeside.xcodeproj/project.pbxproj",
		"app/Freeside.xcodeproj/xcshareddata/xcschemes/FreesideMac.xcscheme",
		"app/FreesideTests/App.xctestplan",
		"Freeside.xcworkspace/contents.xcworkspacedata",
		".gitattributes",
		"pkg/.gitattributes",
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
		// The Swift/Xcode class is name/extension-specific: a regular
		// Swift source, a manifest-lookalike suffix, and prose about the
		// project files must stay clean.
		"app/Sources/Freeside/App.swift",
		"Package.swift.md",
		"Package@swift-6.0.swift.md",
		"docs/pbxproj-notes.md",
		// The .swiftpm config patterns are specific to the two redirect
		// files, not the whole directory.
		".swiftpm/configuration/other.json",
		".swiftpm/xcode/package.xcworkspace/xcuserdata/x.plist",
		"Config/Debug.xcconfig.bak",
	}
	for _, p := range flagged {
		if got := flagControlPaths([]importer.Change{changed(p)}, nil, nil, testRecipePath); len(got) != 1 || got[0].Kind != FindingVerificationControlPath {
			t.Errorf("%q: findings = %v, want one verification_control_path", p, got)
		}
	}
	for _, p := range clean {
		if got := flagControlPaths([]importer.Change{changed(p)}, nil, nil, testRecipePath); len(got) != 0 {
			t.Errorf("%q: findings = %v, want none", p, got)
		}
	}
}

// TestFlagControlPathsDeletions pins that deletions are flagged:
// removing a Makefile or the recipe steers verification exactly as
// rewriting one does.
func TestFlagControlPathsDeletions(t *testing.T) {
	del := importer.Change{Path: "Makefile", Kind: importer.ChangeDeleted}
	got := flagControlPaths([]importer.Change{del}, nil, nil, testRecipePath)
	if len(got) != 1 || got[0].Detail != "deleted" {
		t.Fatalf("findings = %v, want one deleted flag", got)
	}
}

// TestFlagControlPathsNonUTF8 pins that a non-canonical path is flagged
// unconditionally: an unmatchable name must not bypass the class.
func TestFlagControlPathsNonUTF8(t *testing.T) {
	raw := importer.Change{PathHex: "676f2e6d6f64ff", Kind: importer.ChangeAdded}
	got := flagControlPaths([]importer.Change{raw}, nil, nil, testRecipePath)
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
	got := flagControlPaths([]importer.Change{changed("scripts/verify.sh"), changed("go.mod")}, []string{"**/scripts/verify.sh"}, nil, testRecipePath)
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
	findings := flagControlPaths(changes, nil, nil, testRecipePath)
	got, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, "flag_control_paths", append(got, '\n'))
}

// TestFlagControlPathsCommandEntrypoints is the Codex-review
// regression: a recipe that runs a repo-local script makes that script
// a verification-control surface, so a candidate change to it is
// flagged without the recipe author duplicating it in policy.
func TestFlagControlPathsCommandEntrypoints(t *testing.T) {
	commandPaths := Recipe{Commands: [][]string{{"bash", "scripts/verify.sh"}, {"./tools/lint"}}}.CommandPaths()
	flagged := flagControlPaths([]importer.Change{
		changed("scripts/verify.sh"),
		{Path: "tools/lint", Kind: importer.ChangeDeleted},
		changed("README.md"),
	}, nil, commandPaths, testRecipePath)
	if len(flagged) != 2 {
		t.Fatalf("findings = %v, want the script and the deleted entrypoint flagged", flagged)
	}
	// The unreferenced README stays clean.
	for _, f := range flagged {
		if f.Path == "README.md" {
			t.Errorf("unreferenced README flagged as a control path")
		}
	}
}

// TestCommandEntrypointFilenameEnumeration is the adversarial
// enumeration of the entrypoint-filename input space (run once as
// tests, not a widening of a cited pattern): a recipe entrypoint is
// executed by its literal name, so every character class that the
// glob/alias machinery would mangle must still match its own bytes and
// never over-match a pattern expansion. Each name is used as a recipe
// entrypoint; a candidate change to that exact name must be flagged.
func TestCommandEntrypointFilenameEnumeration(t *testing.T) {
	names := []string{
		"scripts/verify.sh",        // ordinary
		"scripts/check[fast].sh",   // glob char class
		"scripts/check*.sh",        // glob star
		"scripts/check?.sh",        // glob question
		"scripts/check...sh",       // three embedded dots (not the ... segment)
		"scripts/check:fast.sh",    // colon (alias/ADS separator on NTFS)
		"scripts/weird .sh",        // trailing space before extension
		"deep/nested/dir/build.py", // deeper path
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			cp := Recipe{Commands: [][]string{{"./" + name}}}.CommandPaths()
			if got := flagControlPaths([]importer.Change{changed(name)}, nil, cp, testRecipePath); len(got) != 1 || got[0].Path != name {
				t.Fatalf("entrypoint %q change not flagged: %v", name, got)
			}
		})
	}

	// Literal match, never a glob expansion: a char-class entrypoint
	// must not flag a name the class would have matched.
	cp := Recipe{Commands: [][]string{{"./scripts/check[fast].sh"}}}.CommandPaths()
	if got := flagControlPaths([]importer.Change{changed("scripts/checkf.sh")}, nil, cp, testRecipePath); len(got) != 0 {
		t.Errorf("literal entrypoint over-matched a glob expansion: %v", got)
	}

	// The Go recursive package pattern is not a file and is not an
	// entrypoint; a change to a literal `...` name it would clean to is
	// not flagged via the entrypoint path.
	if paths := (Recipe{Commands: [][]string{{"go", "test", "./..."}, {"go", "build", "./internal/..."}}}).CommandPaths(); len(paths) != 0 {
		t.Errorf("Go package patterns leaked into command paths: %v", paths)
	}
}
