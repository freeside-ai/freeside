package claudeinference

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCompleteAndConfirmSeparatesOutcomeFromProcessExit(t *testing.T) {
	body, err := json.Marshal(completion())
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"printf '%s' '" + string(body) + "'", "exit 7"} {
		driver := testDriver(t, script)
		_, quiescent, _ := driver.CompleteAndConfirm(t.Context(), classifierRequest(), "synthetic")
		if !quiescent {
			t.Fatal("joined CLI group did not produce quiescence proof")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, quiescent, err := driver.CompleteAndConfirm(ctx, classifierRequest(), "synthetic"); err == nil || !quiescent {
			t.Fatalf("no-launch refusal: quiescent=%v, err=%v", quiescent, err)
		}
	}
}

func TestCompleteAndConfirmCancellationJoinsNativeGroup(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	if err := syscall.Mkfifo(readyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600) // #nosec G304 -- private test FIFO.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ready.Close() }()
	quote := "'" + strings.ReplaceAll(readyPath, "'", "'\\''") + "'"
	driver := testDriver(t, "printf 'ready\\n' > "+quote+"\nexec /bin/sleep 600")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proof := make(chan bool, 1)
	go func() {
		_, quiescent, err := driver.CompleteAndConfirm(ctx, classifierRequest(), "synthetic")
		proof <- quiescent && err != nil
	}()
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !<-proof {
		t.Fatal("cancelled native call lacked joined process-group evidence")
	}
}
