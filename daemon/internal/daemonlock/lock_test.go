package daemonlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExcludesCanonicalDatabaseAliasesAndReleases(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := Acquire(db); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock error = %v, want ErrAlreadyRunning", err)
	}
	link := filepath.Join(dir, "state-link.db")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("symlink lock error = %v, want ErrAlreadyRunning", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := Acquire(db); err != nil {
		t.Fatalf("reacquire after close: %v", err)
	} else if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockExcludesDanglingDatabaseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := Acquire(target); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("target lock error = %v, want ErrAlreadyRunning", err)
	}
}

func TestLockRejectsHardLinkedDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(db, filepath.Join(dir, "alias.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(db); !errors.Is(err, ErrAmbiguousDatabasePath) {
		t.Fatalf("hard-linked database error = %v, want ErrAmbiguousDatabasePath", err)
	}
}
