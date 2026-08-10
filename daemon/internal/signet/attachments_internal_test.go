package signet

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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

func TestBlobStorePutRetriesExistingBlobDirectorySync(t *testing.T) {
	store, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "durable artifact"
	digest := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))))
	failedSync := errors.New("injected blob directory sync failure")
	syncCalls := 0
	syncDir := func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return failedSync
		}
		return nil
	}

	if _, err := store.put(digest, strings.NewReader(body), syncDir); !errors.Is(err, failedSync) {
		t.Fatalf("first put error = %v, want injected sync failure", err)
	}
	created, err := store.put(digest, strings.NewReader(body), syncDir)
	if err != nil {
		t.Fatalf("retry existing blob: %v", err)
	}
	if created {
		t.Fatal("retry reported existing blob as newly created")
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want 2", syncCalls)
	}
}

func TestBlobStoreVerifyHashesStoredBytes(t *testing.T) {
	store, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "durable artifact"
	digest := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))))
	if _, err := store.Put(digest, strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	verified, err := store.Verify(digest)
	if err != nil || !verified {
		t.Fatalf("Verify stored bytes = %t, %v; want true, nil", verified, err)
	}

	path, err := store.blobPath(digest)
	if err != nil {
		t.Fatalf("blobPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	verified, err = store.Verify(digest)
	if err != nil || verified {
		t.Fatalf("Verify corrupted bytes = %t, %v; want false, nil", verified, err)
	}
}
