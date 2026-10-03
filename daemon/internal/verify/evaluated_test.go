package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// mergeFixture is a base-advance re-entry: a candidate head built on the
// old base, a base that has since advanced, and the prospective merge of
// the head into the advanced base. Options name the advanced base, the
// unchanged head, and the merge as the evaluated commit.
type mergeFixture struct {
	checkout string
	opts     Options
	room     *recordingRoom
	oldBase  string
}

func newMergeFixture(t *testing.T) mergeFixture {
	t.Helper()
	checkout, opts, room := verifyFixture(
		t,
		map[string]string{"main.go": "package main\n"},
		[]importer.Change{{Path: "main.go", Kind: importer.ChangeAdded, Mode: "100644", Digest: "sha256:bb"}},
	)
	oldBase := opts.BaseSHA
	writeFiles(t, checkout, map[string]string{"landed.txt": "landed on the base after the head was built\n"})
	runGit(t, checkout, "add", "-A")
	runGit(t, checkout, "commit", "-q", "-m", "base advance")
	opts.BaseSHA = runGit(t, checkout, "rev-parse", "HEAD")
	opts.EvaluatedSHA = commitWithParents(t, checkout, mergedTree(t, checkout, opts.BaseSHA, opts.HeadSHA), opts.BaseSHA, opts.HeadSHA)
	return mergeFixture{checkout: checkout, opts: opts, room: room, oldBase: oldBase}
}

// mergedTree returns the tree of merging head into base, leaving the
// checkout's HEAD at base.
func mergedTree(t *testing.T, dir, base, head string) string {
	t.Helper()
	runGit(t, dir, "reset", "-q", "--hard", base)
	runGit(t, dir, "merge", "-q", "--no-ff", "--no-commit", head)
	tree := runGit(t, dir, "write-tree")
	runGit(t, dir, "reset", "-q", "--hard", base)
	return tree
}

// commitWithParents builds a commit of tree with exactly the given
// parents, in order, and anchors it against gc.
func commitWithParents(t *testing.T, dir, tree string, parents ...string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", "prospective merge"}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	sha := runGit(t, dir, args...)
	runGit(t, dir, "update-ref", "refs/freeside/evaluated/"+sha, sha)
	return sha
}

// TestVerifyEvaluatedMergeVerifiesMergedTree is the base-advance
// re-entry's verification identity: the recipe runs against the merge's
// tree (the head's change and the base's later commit together), the
// account names the base, the head, and the merge, and the evidence
// still binds the head the forge shows.
func TestVerifyEvaluatedMergeVerifiesMergedTree(t *testing.T) {
	f := newMergeFixture(t)
	f.room.results = map[string]StepResult{
		"go test ./...": {Output: []byte("ok  \texample.test\t0.01s\n")},
		"go vet ./...":  {Output: []byte("")},
	}
	f.room.inspect = func(workdir string) {
		for _, name := range []string{"main.go", "landed.txt"} {
			if _, err := os.Stat(filepath.Join(workdir, name)); err != nil {
				t.Errorf("merged workspace lacks %s: %v", name, err)
			}
		}
	}
	res, err := Verify(context.Background(), f.checkout, f.opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(f.room.runs) == 0 {
		t.Fatal("no recipe command ran")
	}
	if res.HeadSHA != f.opts.HeadSHA || res.EvaluatedSHA != f.opts.EvaluatedSHA || res.Outcome != OutcomePassed {
		t.Fatalf("result binds head %s evaluated %s outcome %s", res.HeadSHA, res.EvaluatedSHA, res.Outcome)
	}
	rep, err := ParseReport(res.Evidence[0].Content)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if rep.HeadSHA != f.opts.HeadSHA || rep.BaseSHA != f.opts.BaseSHA || rep.EvaluatedSHA != f.opts.EvaluatedSHA {
		t.Fatalf("report binds head %s base %s evaluated %s", rep.HeadSHA, rep.BaseSHA, rep.EvaluatedSHA)
	}
	for _, evidence := range res.Evidence {
		if got := evidence.Artifact.Provenance.SourceHeadSHA; got != f.opts.HeadSHA {
			t.Errorf("%s provenance head = %s, want the pull request head %s", evidence.Artifact.Type, got, f.opts.HeadSHA)
		}
	}
	golden.Assert(t, "verify_report_prospective_merge", res.Evidence[0].Content)

	// The head's own tree, verified without the merge, lacks the base's
	// later commit: the two verifications are of different trees.
	headOnly := f.opts
	headOnly.EvaluatedSHA = ""
	sawLanded := false
	f.room.inspect = func(workdir string) {
		if _, err := os.Stat(filepath.Join(workdir, "landed.txt")); err == nil {
			sawLanded = true
		}
	}
	plain, err := Verify(context.Background(), f.checkout, headOnly)
	if err != nil {
		t.Fatalf("Verify head: %v", err)
	}
	if sawLanded || plain.EvaluatedSHA != "" {
		t.Fatalf("head verification saw the merged tree (landed=%v evaluated=%q)", sawLanded, plain.EvaluatedSHA)
	}
	if bytes.Contains(plain.Evidence[0].Content, []byte("evaluated_sha")) {
		t.Fatal("head verification's report names an evaluated commit")
	}
}

// TestVerifyEvaluatedMergeInspectsTheMergedTree covers the two checks
// that read a tree before any command runs. Both must read the merge's:
// the head predates the base's later commit, so its tree answers each
// check differently from the tree the commands run in.
func TestVerifyEvaluatedMergeInspectsTheMergedTree(t *testing.T) {
	divergent := func(res Result) bool {
		for _, f := range res.Findings {
			if f.Kind == FindingRecipeDivergence {
				return true
			}
		}
		return false
	}

	t.Run("recipe divergence", func(t *testing.T) {
		// The base advance rewrites the trusted recipe. The merge carries
		// the rewrite, so it agrees with the base; the head still carries
		// the recipe of the old base.
		checkout, opts, _ := verifyFixture(
			t,
			map[string]string{"main.go": "package main\n"},
			[]importer.Change{{Path: "main.go", Kind: importer.ChangeAdded, Mode: "100644", Digest: "sha256:bb"}},
		)
		writeFiles(t, checkout, map[string]string{testRecipePath: `{"commands": [["go", "vet", "./..."]], "capture": "none"}`})
		runGit(t, checkout, "add", "-A")
		runGit(t, checkout, "commit", "-q", "-m", "base advance rewrites the recipe")
		opts.BaseSHA = runGit(t, checkout, "rev-parse", "HEAD")
		opts.EvaluatedSHA = commitWithParents(t, checkout, mergedTree(t, checkout, opts.BaseSHA, opts.HeadSHA), opts.BaseSHA, opts.HeadSHA)

		merged, err := Verify(context.Background(), checkout, opts)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if divergent(merged) {
			t.Fatalf("merge verification compared the head's recipe: %+v", merged.Findings)
		}
		headOnly := opts
		headOnly.EvaluatedSHA = ""
		plain, err := Verify(context.Background(), checkout, headOnly)
		if err != nil {
			t.Fatalf("Verify head: %v", err)
		}
		if !divergent(plain) {
			t.Fatal("fixture does not distinguish the trees: the head's recipe matches the advanced base's")
		}
	})

	t.Run("symlink entrypoint", func(t *testing.T) {
		// The base advance turns the command entrypoint into a symlink.
		// The commands would run in the merge's tree, so the merge's
		// entrypoint is the one that must be a regular file.
		dir, base := initRepo(t, map[string]string{
			"scripts/verify.sh": "#!/bin/sh\ntrue\n",
			"run-check":         "#!/bin/sh\ntrue\n",
		})
		head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
		runGit(t, dir, "rm", "-q", "--", "run-check")
		if err := os.Symlink("scripts/verify.sh", filepath.Join(dir, "run-check")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-q", "-m", "base advance links the entrypoint")
		advanced := runGit(t, dir, "rev-parse", "HEAD")
		room := &recordingRoom{}
		opts := Options{
			HeadSHA: head, BaseSHA: advanced,
			EvaluatedSHA: commitWithParents(t, dir, mergedTree(t, dir, advanced, head), advanced, head),
			InvocationID: domain.InvocationID("inv-1"),
			RecipeSource: ConfigRecipe([]byte(`{"commands": [["./run-check"]], "capture": "none"}`)),
			Room:         room,
		}
		if _, err := Verify(context.Background(), dir, opts); !errors.Is(err, ErrSymlinkEntrypoint) {
			t.Fatalf("err = %v, want ErrSymlinkEntrypoint for the merge's entrypoint", err)
		}
		if len(room.runs) != 0 {
			t.Fatalf("commands ran despite the merge's symlinked entrypoint: %v", room.runs)
		}
		headOnly := opts
		headOnly.EvaluatedSHA = ""
		if _, err := Verify(context.Background(), dir, headOnly); err != nil {
			t.Fatalf("fixture does not distinguish the trees: head verification = %v", err)
		}
	})
}

// TestVerifyEvaluatedMustBeTheMergeOfHeadIntoBase enumerates the commits
// a caller could name in place of the prospective merge. Each fails
// closed before any command runs: the report would otherwise claim the
// named tree's result for this head on this base.
func TestVerifyEvaluatedMustBeTheMergeOfHeadIntoBase(t *testing.T) {
	f := newMergeFixture(t)
	base, head, merge := f.opts.BaseSHA, f.opts.HeadSHA, f.opts.EvaluatedSHA
	tree := runGit(t, f.checkout, "rev-parse", merge+"^{tree}")
	other := commitWithParents(t, f.checkout, runGit(t, f.checkout, "rev-parse", f.oldBase+"^{tree}"), f.oldBase)
	for _, tc := range []struct {
		name      string
		evaluated string
	}{
		{"swapped parents", commitWithParents(t, f.checkout, tree, head, base)},
		{"a commit on the base, not a merge", commitWithParents(t, f.checkout, tree, base)},
		{"a commit on the head, not a merge", commitWithParents(t, f.checkout, tree, head)},
		{"a root commit", commitWithParents(t, f.checkout, tree)},
		{"a merge into the old base", commitWithParents(t, f.checkout, tree, f.oldBase, head)},
		{"a merge of another commit", commitWithParents(t, f.checkout, tree, base, other)},
		{"a third parent", commitWithParents(t, f.checkout, tree, base, head, other)},
		{"a tree object", tree},
		{"a commit the checkout does not hold", "0123456789abcdef0123456789abcdef01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.opts
			room := &recordingRoom{}
			opts.Room, opts.EvaluatedSHA = room, tc.evaluated
			res, err := Verify(context.Background(), f.checkout, opts)
			if !errors.Is(err, ErrEvaluatedMismatch) {
				t.Fatalf("err = %v, want ErrEvaluatedMismatch", err)
			}
			if len(room.runs) != 0 {
				t.Errorf("commands ran despite the evaluated mismatch: %v", room.runs)
			}
			if len(res.Evidence) != 0 || res.Outcome != "" {
				t.Errorf("result %+v carries state despite the failure", res)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		evaluated string
	}{
		{"the head itself", head},
		{"the base itself", base},
		{"short", "abc123"},
		{"uppercase", strings.ToUpper(merge)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.opts
			room := &recordingRoom{}
			opts.Room, opts.EvaluatedSHA = room, tc.evaluated
			if _, err := Verify(context.Background(), f.checkout, opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("err = %v, want ErrInvalidOptions", err)
			}
			if len(room.runs) != 0 {
				t.Errorf("commands ran despite invalid options: %v", room.runs)
			}
		})
	}
}

// TestParseReportEvaluatedCommit pins the merge report's round trip and
// the decode-side refusal of an evaluated commit that repeats the head
// or the base, or arrives without a base.
func TestParseReportEvaluatedCommit(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/verify_report_prospective_merge.golden")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EvaluatedSHA == "" {
		t.Fatal("merge report parsed without its evaluated commit")
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil || !bytes.Equal(append(encoded, '\n'), raw) {
		t.Fatalf("round trip changed the verifier artifact: %v", err)
	}
	for name, mutate := range map[string]func(*Report){
		"evaluated is the head": func(r *Report) { r.EvaluatedSHA = r.HeadSHA },
		"evaluated is the base": func(r *Report) { r.EvaluatedSHA = r.BaseSHA },
		"no base":               func(r *Report) { r.BaseSHA = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := rep
			mutate(&invalid)
			body, err := json.MarshalIndent(invalid, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseReport(append(body, '\n')); err == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
}
