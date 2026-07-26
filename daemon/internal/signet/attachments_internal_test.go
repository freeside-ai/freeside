package signet

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMakeBlobStoreDirectoryRetriesExistingParentSync(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "blobs")
	failedSync := errors.New("injected parent sync failure")
	calls := 0
	err := makeBlobStoreDirectoryWithSync(target, 0o750, func(string) error {
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
	if err := makeBlobStoreDirectoryWithSync(target, 0o750, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatalf("retry existing directory: %v", err)
	}
	if len(synced) != 1 || synced[0] != root {
		t.Fatalf("retry synced %v, want the existing target's parent %s", synced, root)
	}
}

func TestMakeBlobStoreDirectoryConvergesConcurrentCreators(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "attachments", "blobs")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errs <- makeBlobStoreDirectory(root, 0o750)
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent blob-root creation: %v", err)
		}
	}
}
