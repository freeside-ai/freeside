package publish

import (
	"bufio"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// authorityLockHolderEnv makes this test binary a lock holder: it acquires the
// writer lock on the named directory, reports it held, and waits to be killed.
const authorityLockHolderEnv = "FREESIDE_TEST_AUTHORITY_LOCK_HOLDER"

// authorityLockHolderChildEnv makes the holder also start a long-lived child
// after it takes the lock, as a publish starts git, and report its pid.
const authorityLockHolderChildEnv = "FREESIDE_TEST_AUTHORITY_LOCK_HOLDER_CHILD"

func TestAuthorityWriterLockHolderProcess(t *testing.T) {
	t.Parallel()
	dir := os.Getenv(authorityLockHolderEnv)
	if dir == "" {
		t.Skip("lock holder subprocess only")
	}
	lock, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		_, _ = os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	heldLock = lock // keep the file reachable so no finalizer closes it
	if os.Getenv(authorityLockHolderChildEnv) != "" {
		child := osexec.Command("sleep", "3600")
		if err := child.Start(); err != nil {
			_, _ = os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("held " + strconv.Itoa(child.Process.Pid) + "\n")
		time.Sleep(time.Hour)
	}
	_, _ = os.Stdout.WriteString("held\n")
	time.Sleep(time.Hour) // hold until killed; the lock is never closed
}

var heldLock *AuthorityWriterLock

func TestAuthorityWriterLockExcludesASecondAcquisition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireAuthorityWriterLock(dir); !errors.Is(err, ErrAuthorityWriterLockHeld) {
		t.Fatalf("second acquire in the same process = %v, want ErrAuthorityWriterLockHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	again, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	info, err := os.Lstat(filepath.Join(dir, authorityWriterLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestAuthorityWriterLockRefusesUnusableDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := AcquireAuthorityWriterLock(" "); err == nil {
		t.Fatal("empty directory accepted")
	}
	if _, err := AcquireAuthorityWriterLock(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireAuthorityWriterLock(file); err == nil {
		t.Fatal("regular file accepted as the state directory")
	}
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(state, authorityWriterLockFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireAuthorityWriterLock(state); err == nil {
		t.Fatal("symlinked lock file accepted")
	}
}

// TestAuthorityWriterLockIsSharedAcrossProcessesAndDroppedOnKill proves the two
// properties the start order rests on: a lock another process holds refuses
// this one, and a holder killed without closing anything leaves no stale lock.
func TestAuthorityWriterLockIsSharedAcrossProcessesAndDroppedOnKill(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestAuthorityWriterLockHolderProcess$") //nolint:gosec // G204: this test binary with a fixed argument
	cmd.Env = append(os.Environ(), authorityLockHolderEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read holder status: %v", err)
	}
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("holder reported %q", line)
	}

	if _, err := AcquireAuthorityWriterLock(dir); !errors.Is(err, ErrAuthorityWriterLockHeld) {
		t.Fatalf("acquire beside another process = %v, want ErrAuthorityWriterLockHeld", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatalf("acquire after the holder was killed: %v", err)
	}
	_ = lock.Close()
}

func TestAuthorityWriterLockCovers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	other := filepath.Join(root, "other")
	for _, dir := range []string{state, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireAuthorityWriterLock(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := lock.Covers(state); err != nil {
		t.Fatalf("lock on a link to the state directory does not cover it: %v", err)
	}
	if err := lock.Covers(other); err == nil {
		t.Fatal("lock covers another directory")
	}
	_ = lock.Close()
	if err := lock.Covers(state); err == nil {
		t.Fatal("released lock covers its directory")
	}
	var nilLock *AuthorityWriterLock
	if err := nilLock.Covers(state); err == nil {
		t.Fatal("nil lock covers a directory")
	}
}

// TestAuthorityWriterLockCloseReleasesASharedDescription: a child forked but
// not yet exec'd shares the lock's open file description. A dup shares it
// the same way, so the lock must be free once Close returns even while the
// dup stays open.
func TestAuthorityWriterLockCloseReleasesASharedDescription(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := syscall.Dup(int(lock.file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(shared) }()
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatalf("lock after Close beside a shared description: %v", err)
	}
	_ = again.Close()
}

// TestAuthorityWriterLockDropsWhileTheHoldersChildRuns: a holder killed
// mid-publish leaves its git child running. The child never inherited the
// lock, so the lock is free once the holder is dead, while the child lives.
func TestAuthorityWriterLockDropsWhileTheHoldersChildRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestAuthorityWriterLockHolderProcess$") //nolint:gosec // G204: this test binary with a fixed argument
	cmd.Env = append(os.Environ(), authorityLockHolderEnv+"="+dir, authorityLockHolderChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read holder status: %v", err)
	}
	pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "held ")
	if !ok {
		t.Fatalf("holder reported %q", line)
	}
	childPID, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("the holder's child is gone (%v); the test proves nothing", err)
	}
	lock, err := AcquireAuthorityWriterLock(dir)
	if err != nil {
		t.Fatalf("acquire while the dead holder's child runs: %v", err)
	}
	_ = lock.Close()
}
