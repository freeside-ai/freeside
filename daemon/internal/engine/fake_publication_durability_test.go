package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestFakePublicationDurabilityHelpers(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := makeFakePublicationDirectory(nested, 0o700); err != nil {
		t.Fatalf("make durable directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "candidate.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncFakePublicationTree(filepath.Join(root, "one")); err != nil {
		t.Fatalf("sync tree: %v", err)
	}
	if err := syncFakePublicationDirectory(root); err != nil {
		t.Fatalf("sync directory: %v", err)
	}
}

func TestFakePublicationDurabilityHelpersRejectNonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeFakePublicationDirectory(filepath.Join(notDirectory, "child"), 0o700); err == nil {
		t.Fatal("created directory beneath a regular file")
	}
}

func TestMakeFakePublicationDirectoryRetriesExistingParentSync(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "candidate")
	failedSync := errors.New("injected parent sync failure")
	calls := 0
	err := makeFakePublicationDirectoryWithSync(target, 0o700, func(string) error {
		calls++
		if calls == 2 {
			return failedSync
		}
		return nil
	})
	if !errors.Is(err, failedSync) {
		t.Fatalf("first creation error = %v, want injected sync failure", err)
	}
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		t.Fatalf("failed sync did not leave the created directory for retry: %v", statErr)
	}

	var synced []string
	if err := makeFakePublicationDirectoryWithSync(target, 0o700, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatalf("retry existing directory: %v", err)
	}
	if len(synced) != 1 || synced[0] != root {
		t.Fatalf("retry synced %v, want the existing target's parent %s", synced, root)
	}
}

func TestInstallFakePublicationCheckpointDoesNotReplaceConcurrentWinner(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "candidate.json")
	sources := []string{
		filepath.Join(root, "candidate-one"),
		filepath.Join(root, "candidate-two"),
	}
	for i, source := range sources {
		if err := os.WriteFile(source, []byte{byte('1' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	results := make(chan bool, len(sources))
	errs := make(chan error, len(sources))
	var workers sync.WaitGroup
	for _, source := range sources {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			installed, err := installFakePublicationCheckpoint(source, destination)
			results <- installed
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)

	installed := 0
	for result := range results {
		if result {
			installed++
		}
	}
	if installed != 1 {
		t.Fatalf("installed = %d, want exactly one winner", installed)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("install checkpoint: %v", err)
		}
	}
	body, err := os.ReadFile(destination) //nolint:gosec // test-owned path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "1" && string(body) != "2" {
		t.Fatalf("checkpoint body = %q, want one complete contender", body)
	}
}

func TestValidateFakePublicationRecipePath(t *testing.T) {
	for _, path := range []string{
		"",
		"/verify.json",
		"../verify.json",
		"./verify.json",
		"recipes//verify.json",
		"recipes/*.json",
		"recipes\\verify.json",
		"recipes:verify.json",
	} {
		t.Run(path, func(t *testing.T) {
			if err := validateFakePublicationRecipePath(path); err == nil {
				t.Fatalf("accepted invalid recipe path %q", path)
			}
		})
	}
	if err := validateFakePublicationRecipePath(".freeside/verify.json"); err != nil {
		t.Fatalf("rejected valid recipe path: %v", err)
	}
}

func TestPutTerminalItemAcceptsCompatibleLifecycleAdvance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attention := signet.NewService(st)
	runID := domain.RunID("run-terminal")
	expected, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "ready-run-terminal", ProjectID: "project-terminal",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "owner/repo#7 is published and ready for final review.",
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		PRHeadSHA:   "0123456789012345678901234567890123456789",
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attention.PutItem(ctx, expected); err != nil {
		t.Fatal(err)
	}
	advanced := expected
	advanced.ItemVersion = 2
	advanced.Status = domain.StatusDismissed
	if err := attention.PutItem(ctx, advanced); err != nil {
		t.Fatal(err)
	}

	workflow := &fakePublicationWorkflow{store: st, attention: attention}
	if err := workflow.putTerminalItem(ctx, expected); err != nil {
		t.Fatalf("compatible terminal replay: %v", err)
	}
}
