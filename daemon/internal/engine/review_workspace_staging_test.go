package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type stagingTestTransport struct {
	retain func(ctx context.Context, dest, headSHA string) error
}

func (t stagingTestTransport) FetchBase(
	context.Context, string, string, string, string,
) (PublicationCheckout, error) {
	return nil, errors.New("unexpected fetch")
}

func (t stagingTestTransport) RetainWorktree(
	ctx context.Context, _ PublicationCheckout, dest, headSHA string,
) error {
	if t.retain != nil {
		return t.retain(ctx, dest, headSHA)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "head.txt"), []byte(headSHA), 0o600)
}

func (t stagingTestTransport) PushHead(
	context.Context, PublicationCheckout, publish.GatedHead,
) (publish.PushResult, error) {
	return publish.PushResult{}, errors.New("unexpected push")
}

func (t stagingTestTransport) FetchHead(
	context.Context, PublicationCheckout, string, string,
) (publish.HeadFetch, error) {
	return publish.HeadFetch{}, errors.New("unexpected head fetch")
}

func (t stagingTestTransport) ProspectiveMerge(
	context.Context, PublicationCheckout, string,
) (string, error) {
	return "", errors.New("unexpected prospective merge")
}

func newStagingTestWorkflow(t *testing.T, transport PublicationTransport) (*productionPublicationWorkflow, string) {
	t.Helper()
	workDir := t.TempDir()
	parent := filepath.Join(workDir, "review-workspaces")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("mkdir review-workspaces: %v", err)
	}
	return &productionPublicationWorkflow{workDir: workDir, transport: transport}, parent
}

func readStagingTestFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(filepath.Base(path))
}

type treeEntrySnapshot struct {
	mode       os.FileMode
	size       int64
	linkTarget string
	content    []byte
}

func snapshotTree(t *testing.T, root, excludeTop string) map[string]treeEntrySnapshot {
	t.Helper()
	snap := make(map[string]treeEntrySnapshot)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if excludeTop != "" && rel == excludeTop {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := treeEntrySnapshot{mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.linkTarget = target
		case info.Mode().IsRegular():
			entry.size = info.Size()
			if info.Mode().Perm()&0o400 != 0 {
				data, err := readStagingTestFile(path)
				if err != nil {
					return err
				}
				entry.content = data
			}
		}
		snap[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %q: %v", root, err)
	}
	return snap
}

func assertTreeSnapshotEqual(t *testing.T, label string, want, got map[string]treeEntrySnapshot) {
	t.Helper()
	for path, w := range want {
		g, ok := got[path]
		if !ok {
			t.Fatalf("%s: entry %q disappeared", label, path)
		}
		if w.mode != g.mode || w.size != g.size || w.linkTarget != g.linkTarget || !bytes.Equal(w.content, g.content) {
			t.Fatalf("%s: entry %q changed: got (%v, size=%d, link=%q, len=%d), want (%v, size=%d, link=%q, len=%d)",
				label, path, g.mode, g.size, g.linkTarget, len(g.content), w.mode, w.size, w.linkTarget, len(w.content))
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Fatalf("%s: unexpected entry %q appeared", label, path)
		}
	}
}

func seedAbandonedStaging(t *testing.T, parent string, id domain.InvocationID, payload string) string {
	t.Helper()
	staging, err := claimReviewWorkspaceStaging(parent, id)
	if err != nil {
		t.Fatalf("claim staging for %s: %v", id, err)
	}
	ws := filepath.Join(staging, reviewWorkspaceStagingWorkspace)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatalf("mkdir abandoned workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "partial.txt"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write abandoned workspace file: %v", err)
	}
	return staging
}

func TestReviewWorkspaceStagingCrashRecovery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := domain.InvocationID("review-aaaaaaaaaaaaaaaaaaaaaaaa")

	t.Run("ensure reclaims abandoned staging and retains clean workspace", func(t *testing.T) {
		w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
		abandoned1 := seedAbandonedStaging(t, parent, id, "crash-1")
		abandoned2 := seedAbandonedStaging(t, parent, id, "crash-2")

		target, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-current")
		if err != nil {
			t.Fatalf("ensureReviewWorkspace: %v", err)
		}
		for _, dir := range []string{abandoned1, abandoned2} {
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected abandoned staging %q removed, got err=%v", dir, err)
			}
		}
		got, err := readStagingTestFile(filepath.Join(target, "head.txt"))
		if err != nil {
			t.Fatalf("read retained workspace: %v", err)
		}
		if string(got) != "sha-current" {
			t.Fatalf("retained workspace head.txt = %q, want %q", got, "sha-current")
		}
		if _, err := os.Lstat(filepath.Join(target, reviewWorkspaceStagingRecordName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ownership record leaked into retained workspace: err=%v", err)
		}
	})

	t.Run("remove reclaims abandoned staging with or without retained workspace", func(t *testing.T) {
		w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
		abandoned := seedAbandonedStaging(t, parent, id, "crash-before-rename")

		if err := w.removeReviewWorkspace(id); err != nil {
			t.Fatalf("removeReviewWorkspace without target: %v", err)
		}
		if _, err := os.Lstat(abandoned); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected abandoned staging %q removed, got err=%v", abandoned, err)
		}

		target, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-retained")
		if err != nil {
			t.Fatalf("ensureReviewWorkspace: %v", err)
		}
		abandonedAfter := seedAbandonedStaging(t, parent, id, "crash-after-retained")
		if err := w.removeReviewWorkspace(id); err != nil {
			t.Fatalf("removeReviewWorkspace with target: %v", err)
		}
		for _, p := range []string{target, abandonedAfter} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected %q removed, got err=%v", p, err)
			}
		}
	})

	t.Run("remove succeeds when review-workspaces does not exist", func(t *testing.T) {
		w := &productionPublicationWorkflow{workDir: t.TempDir(), transport: stagingTestTransport{}}
		if err := w.removeReviewWorkspace(id); err != nil {
			t.Fatalf("removeReviewWorkspace on absent parent: %v", err)
		}
	})
}

func TestReviewWorkspaceStagingPreservesOtherInvocation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	idA := domain.InvocationID("review-111111111111111111111111")
	idB := domain.InvocationID("review-222222222222222222222222")

	w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
	otherStaging := seedAbandonedStaging(t, parent, idB, "other-invocation-bytes")
	wantOther := snapshotTree(t, otherStaging, "")

	if _, err := w.ensureReviewWorkspace(ctx, idA, nil, "sha-a"); err != nil {
		t.Fatalf("ensureReviewWorkspace(%s): %v", idA, err)
	}
	assertTreeSnapshotEqual(t, "other staging after ensure", wantOther, snapshotTree(t, otherStaging, ""))

	if err := w.removeReviewWorkspace(idA); err != nil {
		t.Fatalf("removeReviewWorkspace(%s): %v", idA, err)
	}
	assertTreeSnapshotEqual(t, "other staging after remove", wantOther, snapshotTree(t, otherStaging, ""))
}

func TestReviewWorkspaceStagingAdversarialEntriesLeftIntact(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := domain.InvocationID("review-333333333333333333333333")

	mustRecordBytes := func(t *testing.T, rec reviewWorkspaceStagingRecord) []byte {
		t.Helper()
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		return data
	}

	cases := []struct {
		name  string
		setup func(t *testing.T, parent, outside string)
	}{
		{
			name: "directory with content and no record",
			setup: func(t *testing.T, parent, _ string) {
				file := filepath.Join(parent, "staging-norecord", "workspace", "keep.txt")
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(file, []byte("pre-upgrade-content"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		{
			name: "record naming different directory",
			setup: func(t *testing.T, parent, _ string) {
				dir := filepath.Join(parent, "staging-renamed")
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-original",
				})
				if err := os.WriteFile(filepath.Join(dir, reviewWorkspaceStagingRecordName), recBytes, 0o600); err != nil {
					t.Fatalf("write record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "workspace", "data.txt"), []byte("do-not-delete"), 0o600); err != nil {
					t.Fatalf("write data: %v", err)
				}
			},
		},
		{
			name: "record symlink to valid outside record",
			setup: func(t *testing.T, parent, outside string) {
				dir := filepath.Join(parent, "staging-rec-symlink")
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				outsideRec := filepath.Join(outside, "valid-record.json")
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-rec-symlink",
				})
				if err := os.WriteFile(outsideRec, recBytes, 0o600); err != nil {
					t.Fatalf("write outside record: %v", err)
				}
				if err := os.Symlink(outsideRec, filepath.Join(dir, reviewWorkspaceStagingRecordName)); err != nil {
					t.Fatalf("symlink record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "workspace", "keep.txt"), []byte("keep-through-record-symlink"), 0o600); err != nil {
					t.Fatalf("write data: %v", err)
				}
			},
		},
		{
			name: "record symlink to valid in-directory record",
			setup: func(t *testing.T, parent, _ string) {
				dir := filepath.Join(parent, "staging-rec-inroot-symlink")
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-rec-inroot-symlink",
				})
				if err := os.WriteFile(filepath.Join(dir, "workspace", "forged.json"), recBytes, 0o600); err != nil {
					t.Fatalf("write forged record: %v", err)
				}
				if err := os.Symlink(filepath.Join("workspace", "forged.json"), filepath.Join(dir, reviewWorkspaceStagingRecordName)); err != nil {
					t.Fatalf("symlink record: %v", err)
				}
			},
		},
		{
			name: "malformed record",
			setup: func(t *testing.T, parent, _ string) {
				dir := filepath.Join(parent, "staging-malformed")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				raw := []byte(`{"version":1,"invocation_id":"` + string(id) + `","dir_name":"staging-malformed","extra":true}`)
				if err := os.WriteFile(filepath.Join(dir, reviewWorkspaceStagingRecordName), raw, 0o600); err != nil {
					t.Fatalf("write malformed record: %v", err)
				}
			},
		},
		{
			name: "oversized record",
			setup: func(t *testing.T, parent, _ string) {
				dir := filepath.Join(parent, "staging-oversized")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-oversized",
				})
				padded := append(bytes.Repeat([]byte(" "), int(reviewWorkspaceStagingRecordMax)+1), recBytes...)
				if err := os.WriteFile(filepath.Join(dir, reviewWorkspaceStagingRecordName), padded, 0o600); err != nil {
					t.Fatalf("write oversized record: %v", err)
				}
			},
		},
		{
			name: "unreadable record",
			setup: func(t *testing.T, parent, _ string) {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses file read permissions")
				}
				dir := filepath.Join(parent, "staging-unreadable")
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-unreadable",
				})
				recPath := filepath.Join(dir, reviewWorkspaceStagingRecordName)
				if err := os.WriteFile(recPath, recBytes, 0o600); err != nil {
					t.Fatalf("write record: %v", err)
				}
				if err := os.Chmod(recPath, 0o000); err != nil {
					t.Fatalf("chmod record: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(recPath, 0o600) })
			},
		},
		{
			name: "wrong-version record",
			setup: func(t *testing.T, parent, _ string) {
				dir := filepath.Join(parent, "staging-wrong-version")
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion + 1,
					InvocationID: id,
					DirName:      "staging-wrong-version",
				})
				if err := os.WriteFile(filepath.Join(dir, reviewWorkspaceStagingRecordName), recBytes, 0o600); err != nil {
					t.Fatalf("write wrong-version record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "workspace", "data.txt"), []byte("future-version-content"), 0o600); err != nil {
					t.Fatalf("write data: %v", err)
				}
			},
		},
		{
			name: "staging symlink to outside directory with valid record",
			setup: func(t *testing.T, parent, outside string) {
				targetDir := filepath.Join(outside, "victim-dir")
				if err := os.MkdirAll(filepath.Join(targetDir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir outside dir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-dir-symlink",
				})
				if err := os.WriteFile(filepath.Join(targetDir, reviewWorkspaceStagingRecordName), recBytes, 0o600); err != nil {
					t.Fatalf("write outside record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(targetDir, "workspace", "precious.txt"), []byte("outside-target-must-survive"), 0o600); err != nil {
					t.Fatalf("write outside file: %v", err)
				}
				if err := os.Symlink(targetDir, filepath.Join(parent, "staging-dir-symlink")); err != nil {
					t.Fatalf("symlink staging dir: %v", err)
				}
			},
		},
		{
			name: "staging symlink to in-root directory with valid record",
			setup: func(t *testing.T, parent, _ string) {
				targetDir := filepath.Join(parent, "staging-real")
				if err := os.MkdirAll(filepath.Join(targetDir, "workspace"), 0o700); err != nil {
					t.Fatalf("mkdir in-root target dir: %v", err)
				}
				recBytes := mustRecordBytes(t, reviewWorkspaceStagingRecord{
					Version:      reviewWorkspaceStagingRecordVersion,
					InvocationID: id,
					DirName:      "staging-link",
				})
				if err := os.WriteFile(filepath.Join(targetDir, reviewWorkspaceStagingRecordName), recBytes, 0o600); err != nil {
					t.Fatalf("write in-root target record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(targetDir, "workspace", "precious.txt"), []byte("in-root-target-must-survive"), 0o600); err != nil {
					t.Fatalf("write in-root target file: %v", err)
				}
				if err := os.Symlink("staging-real", filepath.Join(parent, "staging-link")); err != nil {
					t.Fatalf("symlink in-root staging dir: %v", err)
				}
			},
		},
		{
			name: "staging regular file",
			setup: func(t *testing.T, parent, _ string) {
				if err := os.WriteFile(filepath.Join(parent, "staging-regular-file"), []byte("plain-regular-file"), 0o600); err != nil {
					t.Fatalf("write staging regular file: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
			outside := t.TempDir()
			tc.setup(t, parent, outside)

			wantParent := snapshotTree(t, parent, string(id))
			wantOutside := snapshotTree(t, outside, "")

			target, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-ok")
			if err != nil {
				t.Fatalf("ensureReviewWorkspace: %v", err)
			}
			got, err := readStagingTestFile(filepath.Join(target, "head.txt"))
			if err != nil || string(got) != "sha-ok" {
				t.Fatalf("retained workspace = %q, err=%v", got, err)
			}
			assertTreeSnapshotEqual(t, "review-workspaces", wantParent, snapshotTree(t, parent, string(id)))
			assertTreeSnapshotEqual(t, "outside", wantOutside, snapshotTree(t, outside, ""))
		})
	}
}

func TestReviewWorkspaceStagingSymlinkInsideAuthenticatedTree(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := domain.InvocationID("review-444444444444444444444444")

	w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
	outside := t.TempDir()
	outsideSub := filepath.Join(outside, "dir")
	if err := os.MkdirAll(outsideSub, 0o700); err != nil {
		t.Fatalf("mkdir outsideSub: %v", err)
	}
	outsideFile := filepath.Join(outsideSub, "keep.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-survives-reclaim"), 0o600); err != nil {
		t.Fatalf("write outsideFile: %v", err)
	}
	wantOutside := snapshotTree(t, outside, "")

	staging := seedAbandonedStaging(t, parent, id, "abandoned")
	if err := os.Symlink(outsideSub, filepath.Join(staging, reviewWorkspaceStagingWorkspace, "dir-link")); err != nil {
		t.Fatalf("symlink dir inside workspace: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(staging, reviewWorkspaceStagingWorkspace, "file-link")); err != nil {
		t.Fatalf("symlink file inside workspace: %v", err)
	}
	if err := os.Symlink(outsideSub, filepath.Join(staging, "top-link")); err != nil {
		t.Fatalf("symlink beside workspace: %v", err)
	}

	if _, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-clean"); err != nil {
		t.Fatalf("ensureReviewWorkspace: %v", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected authenticated staging %q removed, got err=%v", staging, err)
	}
	assertTreeSnapshotEqual(t, "outside", wantOutside, snapshotTree(t, outside, ""))
}

func TestReviewWorkspaceStagingFailedMaterializationPreservesRetainedAndCleansStaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := domain.InvocationID("review-555555555555555555555555")

	w, parent := newStagingTestWorkflow(t, stagingTestTransport{})
	target, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-v1")
	if err != nil {
		t.Fatalf("initial ensureReviewWorkspace: %v", err)
	}

	failErr := errors.New("simulated materialization failure")
	w.transport = stagingTestTransport{
		retain: func(_ context.Context, dest, _ string) error {
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dest, "partial.txt"), []byte("incomplete"), 0o600); err != nil {
				return err
			}
			return failErr
		},
	}

	if _, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-v2"); !errors.Is(err, failErr) {
		t.Fatalf("expected materialization error %v, got %v", failErr, err)
	}
	got, err := readStagingTestFile(filepath.Join(target, "head.txt"))
	if err != nil || string(got) != "sha-v1" {
		t.Fatalf("existing retained workspace altered: got=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir review-workspaces: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), reviewWorkspaceStagingPrefix) {
			t.Fatalf("failed call left staging directory behind: %q", entry.Name())
		}
	}
}

func TestReviewWorkspaceStagingDeleteFailureIsLoudAndRetainsRecord(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	ctx := t.Context()
	id := domain.InvocationID("review-666666666666666666666666")

	retainCalled := false
	w, parent := newStagingTestWorkflow(t, stagingTestTransport{
		retain: func(_ context.Context, dest, headSHA string) error {
			retainCalled = true
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dest, "head.txt"), []byte(headSHA), 0o600)
		},
	})

	staging := seedAbandonedStaging(t, parent, id, "locked-child")
	lockedDir := filepath.Join(staging, reviewWorkspaceStagingWorkspace, "locked")
	if err := os.MkdirAll(lockedDir, 0o700); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "file.txt"), []byte("stuck"), 0o600); err != nil {
		t.Fatalf("write locked file: %v", err)
	}
	lockedRoot, err := os.OpenRoot(filepath.Join(staging, reviewWorkspaceStagingWorkspace))
	if err != nil {
		t.Fatalf("open workspace root: %v", err)
	}
	t.Cleanup(func() {
		_ = lockedRoot.Chmod("locked", 0o700)
		_ = lockedRoot.Close()
	})
	if err := lockedRoot.Chmod("locked", 0o500); err != nil {
		t.Fatalf("chmod locked dir: %v", err)
	}

	if _, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-blocked"); err == nil {
		t.Fatal("expected error deleting authenticated staging with unremovable child, got nil")
	}
	if retainCalled {
		t.Fatal("ensureReviewWorkspace continued to RetainWorktree after reclaim failure")
	}
	if _, err := os.Lstat(filepath.Join(staging, reviewWorkspaceStagingRecordName)); err != nil {
		t.Fatalf("ownership record must remain until children are deleted: %v", err)
	}

	if err := lockedRoot.Chmod("locked", 0o700); err != nil {
		t.Fatalf("restore locked dir perms: %v", err)
	}
	if err := lockedRoot.Close(); err != nil {
		t.Fatalf("close workspace root: %v", err)
	}
	if _, err := w.ensureReviewWorkspace(ctx, id, nil, "sha-recovered"); err != nil {
		t.Fatalf("ensureReviewWorkspace after restoring perms: %v", err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected staging %q reclaimed on retry, got err=%v", staging, err)
	}
}
