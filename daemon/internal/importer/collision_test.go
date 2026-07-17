package importer

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestDetectCollisions(t *testing.T) {
	base := map[string]treeEntry{
		"README.md":  {mode: "100644", oid: "aaa"},
		"keep/x.txt": {mode: "100644", oid: "bbb"},
	}
	cases := []struct {
		name    string
		changes []plannedChange
		want    []string // changed paths expected to be flagged
	}{
		{
			name: "case collision among adds",
			changes: []plannedChange{
				{path: "Config.txt", kind: ChangeAdded, mode: "100644"},
				{path: "config.txt", kind: ChangeAdded, mode: "100644"},
			},
			want: []string{"Config.txt", "config.txt"},
		},
		{
			name: "add collides with untouched base",
			changes: []plannedChange{
				{path: "readme.md", kind: ChangeAdded, mode: "100644"},
			},
			want: []string{"readme.md"},
		},
		{
			name: "nfc/nfd collision",
			changes: []plannedChange{
				{path: "café.txt", kind: ChangeAdded, mode: "100644"}, // e + combining acute (NFD)
				{path: "café.txt", kind: ChangeAdded, mode: "100644"},  // precomposed é (NFC)
			},
			want: []string{"café.txt", "café.txt"},
		},
		{
			name: "no collision",
			changes: []plannedChange{
				{path: "a.txt", kind: ChangeAdded, mode: "100644"},
				{path: "b.txt", kind: ChangeAdded, mode: "100644"},
			},
			want: nil,
		},
		{
			name: "deletion frees the name",
			changes: []plannedChange{
				{path: "README.md", kind: ChangeDeleted},
				{path: "readme.md", kind: ChangeAdded, mode: "100644"},
			},
			want: nil,
		},
		{
			name: "directory-only case divergence is not a data-losing collision",
			changes: []plannedChange{
				// Keep/y.txt and base keep/x.txt merge into one directory
				// on a case-insensitive FS and both files survive, so no
				// finding: distinct leaf names do not fold together.
				{path: "Keep/y.txt", kind: ChangeAdded, mode: "100644"},
			},
			want: nil,
		},
		{
			name: "full-path fold through a directory component",
			changes: []plannedChange{
				// Keep/x.txt folds onto base keep/x.txt: same file, which
				// content wins is filesystem-defined.
				{path: "Keep/x.txt", kind: ChangeAdded, mode: "100644"},
			},
			want: []string{"Keep/x.txt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := detectCollisions(tc.changes, base)
			got := make(map[string]bool)
			for _, f := range findings {
				if f.Kind != FindingPathCollision {
					t.Errorf("finding kind = %s, want %s", f.Kind, FindingPathCollision)
				}
				got[f.Path] = true
			}
			if len(got) != len(tc.want) {
				t.Fatalf("flagged %v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("expected a collision finding for %q", w)
				}
			}
		})
	}
}

// TestDetectCollisionsFileDirectory is the Codex round-5 regression:
// a file and a directory whose folded names coincide cannot coexist on
// APFS even though their full folded paths differ.
func TestDetectCollisionsFileDirectory(t *testing.T) {
	t.Run("added dir collides with base file", func(t *testing.T) {
		base := map[string]treeEntry{"README": {mode: "100644", oid: "a"}}
		f := detectCollisions([]plannedChange{{path: "readme/config.yml", kind: ChangeAdded, mode: "100644"}}, base)
		if len(f) != 1 || f[0].Path != "readme/config.yml" {
			t.Fatalf("dir/file collision not flagged: %+v", f)
		}
	})
	t.Run("added file collides with base directory", func(t *testing.T) {
		base := map[string]treeEntry{"foo/bar": {mode: "100644", oid: "a"}}
		f := detectCollisions([]plannedChange{{path: "FOO", kind: ChangeAdded, mode: "100644"}}, base)
		if len(f) != 1 || f[0].Path != "FOO" {
			t.Fatalf("file/dir collision not flagged: %+v", f)
		}
	})
	t.Run("deep folded-ancestor file", func(t *testing.T) {
		base := map[string]treeEntry{"a/B": {mode: "100644", oid: "x"}}
		// candidate adds a/b/c: folded ancestor a/b matches base file a/B.
		f := detectCollisions([]plannedChange{{path: "a/b/c", kind: ChangeAdded, mode: "100644"}}, base)
		if len(f) != 1 {
			t.Fatalf("nested file/dir collision not flagged: %+v", f)
		}
	})
	t.Run("distinct siblings under a shared dir do not collide", func(t *testing.T) {
		base := map[string]treeEntry{"dir/a.txt": {mode: "100644", oid: "x"}}
		f := detectCollisions([]plannedChange{{path: "dir/b.txt", kind: ChangeAdded, mode: "100644"}}, base)
		if len(f) != 0 {
			t.Fatalf("sibling files under one dir must not collide: %+v", f)
		}
	})
}

// TestFoldedComponentsFullCaseFold pins the APFS-matching fold: full
// Unicode case folding, not simple lowercasing. ß folds to ss and the
// ﬁ ligature to fi (both the same file on APFS, which simple ToLower
// misses), while İ (U+0130) stays distinct from i (which ToLower wrongly
// merges).
func TestFoldedComponentsFullCaseFold(t *testing.T) {
	same := func(a, b string) bool {
		return strings.Join(foldedComponents(a), "/") == strings.Join(foldedComponents(b), "/")
	}
	if !same("straße", "STRASSE") {
		t.Error("ß must fold to ss (APFS treats straße and STRASSE as one file)")
	}
	if !same("ﬁle", "file") {
		t.Error("ﬁ ligature must fold to fi")
	}
	if same("İ", "i") {
		t.Error("İ (U+0130) must stay distinct from i (APFS keeps them apart)")
	}
	if !same("café", "café") {
		t.Error("NFC and NFD forms must fold equal")
	}
}

// TestDetectCollisionsFullFold exercises the fold through the collision
// path: distinct-content adds that fold together on APFS are flagged.
func TestDetectCollisionsFullFold(t *testing.T) {
	f := detectCollisions([]plannedChange{
		{path: "straße", kind: ChangeAdded, mode: "100644"},
		{path: "STRASSE", kind: ChangeAdded, mode: "100644"},
	}, map[string]treeEntry{})
	if len(f) != 2 {
		t.Fatalf("ß/ss fold collision not flagged on both members: %+v", f)
	}
	// İ vs i must NOT be flagged (they coexist on APFS).
	f = detectCollisions([]plannedChange{
		{path: "İ", kind: ChangeAdded, mode: "100644"},
		{path: "i", kind: ChangeAdded, mode: "100644"},
	}, map[string]treeEntry{})
	if len(f) != 0 {
		t.Fatalf("İ and i coexist on APFS and must not be flagged: %+v", f)
	}
}

// TestDetectCollisionsIgnoresModifies confirms that modifying a path
// which already fold-collides with a base sibling is not flagged: the
// collision pre-existed in the (case-sensitive) base, so a content
// modify or a fromBase chmod does not introduce it.
func TestDetectCollisionsIgnoresModifies(t *testing.T) {
	base := map[string]treeEntry{
		"README.md": {mode: "100644", oid: "aaa"},
		"readme.md": {mode: "100644", oid: "bbb"}, // pre-existing fold-collision
	}
	for _, c := range []plannedChange{
		{path: "README.md", kind: ChangeModified},
		{path: "README.md", kind: ChangeModified, fromBase: true},
	} {
		if f := detectCollisions([]plannedChange{c}, base); len(f) != 0 {
			t.Errorf("modify %+v flagged a pre-existing base collision: %+v", c, f)
		}
	}
	// But newly adding the colliding member is the candidate's doing.
	base2 := map[string]treeEntry{"README.md": {mode: "100644", oid: "aaa"}}
	if f := detectCollisions([]plannedChange{{path: "readme.md", kind: ChangeAdded, mode: "100644"}}, base2); len(f) != 1 {
		t.Fatalf("adding a colliding path must flag: %+v", f)
	}
}

// TestDetectCollisionsDeterministicPartner is the round-17 regression:
// two base paths and one add can share a fold, but the evidence detail
// must not depend on randomized map iteration.
func TestDetectCollisionsDeterministicPartner(t *testing.T) {
	base := map[string]treeEntry{
		"README": {mode: "100644", oid: "aaa"},
		"readme": {mode: "100644", oid: "bbb"},
	}
	changes := []plannedChange{{path: "ReadMe", kind: ChangeAdded, mode: "100644"}}
	const want = "collides with README under case/normalization folding"
	for i := 0; i < 128; i++ {
		f := detectCollisions(changes, base)
		if len(f) != 1 || f[0].Detail != want {
			t.Fatalf("iteration %d findings = %+v, want detail %q", i, f, want)
		}
	}
}

func TestImportCaseCollisionBlocksNothingButFlags(t *testing.T) {
	// Two case-distinct paths a case-insensitive checkout cannot keep
	// apart: the tree can hold both (git is case-sensitive), so the
	// commit exists, but the ambiguity is flagged for the human gate.
	checkout, base := initBaseRepo(t, map[string]string{"keep.txt": "k\n"})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("Data.txt", "one\n", false),
		regularEntryFor("data.txt", "two\n", false),
	}, "one\n", "two\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("a case collision is a policy finding, not a construction block")
	}
	flagged := 0
	for _, f := range res.Findings {
		if f.Kind == FindingPathCollision {
			flagged++
		}
	}
	if flagged != 2 {
		t.Fatalf("got %d collision findings, want 2 (both members)", flagged)
	}
}
