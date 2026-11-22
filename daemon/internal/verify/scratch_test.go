package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchRoom records each workdir it is handed and the mode of the
// workdir's scratch directory while the command runs, then leaves a
// read-only directory in the workspace, as candidate code can.
type scratchRoom struct {
	workdirs     []string
	scratchModes []os.FileMode
	err          error
}

func (r *scratchRoom) Run(_ context.Context, workdir string, _ []string) (StepResult, error) {
	r.workdirs = append(r.workdirs, workdir)
	info, err := os.Stat(filepath.Dir(workdir))
	if err != nil {
		r.err = err
	} else {
		r.scratchModes = append(r.scratchModes, info.Mode().Perm())
	}
	if err := leaveReadOnlyTree(filepath.Join(workdir, "readonly")); err != nil {
		r.err = err
	}
	return StepResult{Output: []byte("ok\n")}, nil
}

func (r *scratchRoom) RunEnv(ctx context.Context, workdir string, argv, _ []string) (StepResult, error) {
	return r.Run(ctx, workdir, argv)
}

var _ EnvRoom = (*scratchRoom)(nil)

// leaveReadOnlyTree makes dir hold a file under a 0555 directory and a
// 0000 directory, which os.RemoveAll alone can't remove.
func leaveReadOnlyTree(dir string) error {
	for _, sub := range []string{"locked", "sealed"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "f"), []byte("f\n"), 0o600); err != nil {
			return err
		}
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0o555); err != nil { //nolint:gosec // G302: a read-only directory is the fixture
		return err
	}
	return os.Chmod(filepath.Join(dir, "sealed"), 0)
}

// TestVerifyStagesOnlyUnderScratchParent pins where verification stages:
// every workspace, a history command's included, sits in a 0700 scratch
// directly under ScratchParent, which is empty again once Verify returns,
// and the temporary directory gets nothing, even when a command leaves a
// read-only directory in its workspace. A cordoned writer can write
// the temporary directory, so a workspace there could change between its
// exact-tree check and the room's copy. Not parallel: it sets TMPDIR.
func TestVerifyStagesOnlyUnderScratchParent(t *testing.T) {
	checkout, base := historyBase(t, nil)
	head := commitChain(t, checkout, base, []candidateCommit{{"Add feature one", map[string]string{"feature.txt": "one\n"}}})[0]
	recipe := `{"commands": [["go", "vet", "./..."], ` +
		`{"argv": ["bash", "history-probe.sh"], "room": "project_image", "history": true}], "capture": "none"}`
	room := &scratchRoom{}
	opts := historyOptions(t, head, base, recipe, room)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	if _, err := Verify(context.Background(), checkout, opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if room.err != nil {
		t.Fatalf("stat scratch: %v", room.err)
	}
	if len(room.workdirs) != 2 {
		t.Fatalf("workdirs = %q, want one per command", room.workdirs)
	}
	for i, workdir := range room.workdirs {
		scratch := filepath.Dir(workdir)
		if filepath.Dir(scratch) != opts.ScratchParent || !strings.HasPrefix(filepath.Base(scratch), ScratchPrefix) {
			t.Errorf("workdir %q is not in a %s* scratch under %q", workdir, ScratchPrefix, opts.ScratchParent)
		}
		if room.scratchModes[i] != 0o700 {
			t.Errorf("scratch of %q has mode %v, want 0700", workdir, room.scratchModes[i])
		}
	}
	for dir, what := range map[string]string{opts.ScratchParent: "scratch parent", tmp: "temporary directory"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s holds %d entries after Verify, want none: %v", what, len(entries), entries)
		}
	}
}

// TestPrepareScratchParentRemovesOnlyScratch pins the startup sweep: a
// prefixed directory, read-only contents included, and a prefixed symlink
// go, the symlink's target is not followed, and an unrelated entry stays.
func TestPrepareScratchParentRemovesOnlyScratch(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "verify-scratch")
	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	for _, dir := range []string{filepath.Join(parent, ScratchPrefix+"x", "workspace-0"), target} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "kept"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := leaveReadOnlyTree(filepath.Join(parent, ScratchPrefix+"x", "workspace-0", "readonly")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, ScratchPrefix+"y")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "keep"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if unremoved, err := PrepareScratchParent(parent); err != nil || len(unremoved) != 0 {
		t.Fatalf("PrepareScratchParent = %v, %v", unremoved, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep" {
		t.Errorf("parent holds %v, want only keep", entries)
	}
	if _, err := os.Stat(filepath.Join(target, "kept")); err != nil {
		t.Errorf("symlink target was followed: %v", err)
	}
}

// TestPrepareScratchParentCreatesPrivateDirectory pins that a missing
// parent is created 0700.
func TestPrepareScratchParentCreatesPrivateDirectory(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "verify-scratch")
	if _, err := PrepareScratchParent(parent); err != nil {
		t.Fatalf("PrepareScratchParent: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("parent mode = %v, want a 0700 directory", info.Mode())
	}
}

// TestPrepareScratchParentRefusals enumerates the parents the sweep
// refuses before deleting anything.
func TestPrepareScratchParentRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(real, ScratchPrefix+"x")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, parent := range map[string]string{
		"empty":    "",
		"relative": "verify-scratch",
		"unclean":  real + "/../real",
		"symlink":  link,
		"file":     file,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareScratchParent(parent); err == nil {
				t.Fatalf("PrepareScratchParent(%q) = nil, want a refusal", parent)
			}
		})
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("a refused sweep through the symlink removed %q: %v", stale, err)
	}
}
