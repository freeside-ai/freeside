package verify

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// TestRecipeSkips is the evaluation table: which of rulesRecipeBytes'
// commands (docs always, go test on daemon/ or api/openapi.yaml, app test
// on app/) each change set runs.
func TestRecipeSkips(t *testing.T) {
	t.Parallel()
	r, err := ParseRecipe([]byte(rulesRecipeBytes))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	changed := func(kind importer.ChangeKind, paths ...string) []importer.Change {
		out := make([]importer.Change, 0, len(paths))
		for _, p := range paths {
			out = append(out, importer.Change{Path: p, Kind: kind})
		}
		return out
	}
	cases := []struct {
		name    string
		changes []importer.Change
		runs    []bool
	}{
		{"daemon only", changed(importer.ChangeModified, "daemon/main.go"), []bool{true, true, false}},
		{"app only", changed(importer.ChangeAdded, "app/Sources/View.swift"), []bool{true, false, true}},
		{"docs only", changed(importer.ChangeModified, "docs/plan.md", "README.md"), []bool{true, false, false}},
		{"run_all_on file", changed(importer.ChangeModified, "docs/plan.md", ".freeside/verify.json"), []bool{true, true, true}},
		{"run_all_on prefix", changed(importer.ChangeAdded, ".github/workflows/new.yml"), []bool{true, true, true}},
		// Without rename detection a move is a delete on one side and an
		// add on the other, so both sides' commands run.
		{"rename across components", append(
			changed(importer.ChangeDeleted, "daemon/internal/x.go"),
			changed(importer.ChangeAdded, "app/x.go")...), []bool{true, true, true}},
		{"empty change set", nil, []bool{true, true, true}},
		{"path not UTF-8", []importer.Change{
			{Path: "docs/plan.md", Kind: importer.ChangeModified},
			{PathHex: "646f63732fff", Kind: importer.ChangeAdded},
		}, []bool{true, true, true}},
		{"sibling of a prefix", changed(importer.ChangeAdded, "daemon-tools/x", "application/y"), []bool{true, false, false}},
		{"prefix names a directory not a file", changed(importer.ChangeAdded, "daemon"), []bool{true, false, false}},
		{"exact file", changed(importer.ChangeModified, "api/openapi.yaml"), []bool{true, true, false}},
		// A case- or normalization-insensitive checkout puts these under
		// daemon/, api/openapi.yaml, and app/, so they run those commands.
		{"case-folded prefix", changed(importer.ChangeAdded, "DAEMON/internal/x_test.go"), []bool{true, true, false}},
		{"case-folded exact file", changed(importer.ChangeModified, "API/OpenAPI.yaml"), []bool{true, true, false}},
		{"alias-normalized prefix", changed(importer.ChangeAdded, "app\u200c/x.swift"), []bool{true, false, true}},
		{"exact file sibling", changed(importer.ChangeModified, "api/openapi.yaml.bak", "api/other.yaml"), []bool{true, false, false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			skips := r.Skips(tc.changes)
			for i, want := range tc.runs {
				if got := skips[i] == nil; got != want {
					t.Errorf("command %d %v runs = %v, want %v", i, r.Commands[i].Argv, got, want)
				}
				if skips[i] != nil && !skips[i].valid() {
					t.Errorf("command %d proof %+v is malformed", i, *skips[i])
				}
			}
			if got := r.RunsClass(ClassMacOSHost, tc.changes); got != tc.runs[2] {
				t.Errorf("RunsClass(macos_host) = %v, want %v", got, tc.runs[2])
			}
		})
	}
}

// TestRecipeSkipProof pins what the proof binds: the changes digest is a
// function of the set of changed paths alone, and the rule digest tells
// commands with different rules apart and changes with run_all_on.
func TestRecipeSkipProof(t *testing.T) {
	t.Parallel()
	r, err := ParseRecipe([]byte(rulesRecipeBytes))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	docs := []importer.Change{{Path: "docs/b.md"}, {Path: "docs/a.md"}}
	reordered := []importer.Change{{Path: "docs/a.md"}, {Path: "docs/b.md"}, {Path: "docs/a.md"}}
	skips, again := r.Skips(docs), r.Skips(reordered)
	if skips[1] == nil || skips[2] == nil {
		t.Fatalf("docs change ran a limited command: %v", skips)
	}
	if *skips[1] != *again[1] {
		t.Errorf("proof changed with the order or repetition of the same paths: %+v, %+v", *skips[1], *again[1])
	}
	if skips[1].ChangesDigest != skips[2].ChangesDigest {
		t.Error("two skips of one change set name different change sets")
	}
	if skips[1].RuleDigest == skips[2].RuleDigest {
		t.Error("commands with different paths share a rule digest")
	}
	other := r.Skips([]importer.Change{{Path: "docs/a.md"}})
	if other[1].ChangesDigest == skips[1].ChangesDigest {
		t.Error("different change sets share a changes digest")
	}
	widened := r
	widened.RunAllOn = append(slices.Clone(r.RunAllOn), "scripts/check.sh")
	if widened.Skips(docs)[1].RuleDigest == skips[1].RuleDigest {
		t.Error("the rule digest does not cover run_all_on")
	}
}
