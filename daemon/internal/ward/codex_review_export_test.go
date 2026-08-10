package ward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// CodexReviewLifecycleTestFixture exposes only the fixture plumbing needed by
// external-package tests that bind the Codex review lifecycle to the real wardstore
// adapter without creating an import cycle in package ward.
type CodexReviewLifecycleTestFixture struct {
	Lifecycle *CodexReviewLifecycle
	Config    CodexReviewConfig
	Launch    CodexReviewLaunchSpec

	runtime *fakeRuntime
}

func NewCodexReviewLifecycleTestFixture(
	t *testing.T, journal CodexReviewJournal,
) *CodexReviewLifecycleTestFixture {
	t.Helper()
	lifecycle, runtime, cfg, launch, fakeJournal := testCodexReviewLifecycle(t)
	cfg.Journal = journal
	if err := journal.PutCodexReviewWorkspaceBinding(
		context.Background(), fakeJournal.workspaceBinding,
	); err != nil {
		t.Fatalf("persist real-adapter workspace fixture: %v", err)
	}
	return &CodexReviewLifecycleTestFixture{
		Lifecycle: lifecycle, Config: cfg, Launch: launch, runtime: runtime,
	}
}

func (f *CodexReviewLifecycleTestFixture) BlockRuntimeCleanup() {
	err := errors.New("simulated process loss before cleanup")
	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	f.runtime.onDeleteContainer = func(string) (bool, error) { return true, err }
	f.runtime.onDeleteVolume = func(string) (bool, error) { return true, err }
	f.runtime.onDeleteNetwork = func(string) (bool, error) { return true, err }
}

func (f *CodexReviewLifecycleTestFixture) UnblockRuntimeCleanup() {
	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	f.runtime.onDeleteContainer = nil
	f.runtime.onDeleteVolume = nil
	f.runtime.onDeleteNetwork = nil
}

func (f *CodexReviewLifecycleTestFixture) RestartVolumeLifecycleLeaser(t *testing.T) {
	t.Helper()
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.Config.VolumeLifecycleLeaser = leaser
}

func (f *CodexReviewLifecycleTestFixture) SeedSnapshotStageResidue(t *testing.T) string {
	t.Helper()
	path := codexReviewSnapshotStagePath(f.Lifecycle.cfg.ExportRoot, f.Launch.RunID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, codexReviewSnapshotAuthName), []byte("residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *CodexReviewLifecycleTestFixture) AssertNoLaunchRuntimeResidue(t *testing.T) {
	t.Helper()
	names := codexReviewNames(f.Launch.RunID)
	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	for _, name := range []string{
		names.workspaceObserver, names.shadowInitializer, names.shadowObserver,
		names.snapshotSeeder, names.snapshotObserver, names.reviewContainer,
	} {
		if _, exists := f.runtime.ctrs[name]; exists {
			t.Errorf("container %q survived recovery", name)
		}
	}
	for _, name := range []string{names.shadowVolume, names.snapshotVolume} {
		if _, exists := f.runtime.vols[name]; exists {
			t.Errorf("volume %q survived recovery", name)
		}
	}
	if _, exists := f.runtime.nets[names.network]; exists {
		t.Errorf("network %q survived recovery", names.network)
	}
}
