package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// This opt-in check executes the pinned native CLI against a local provider
// that never completes its response. A test-only exec shim selects that
// endpoint; both the shim and the native executable are verified private
// copies. Production gains no endpoint override or credential exposure.
func TestTaskCancellationPinnedNativeCLI(t *testing.T) {
	if os.Getenv("FREESIDE_TASK_CANCELLATION_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_TASK_CANCELLATION_LIVE_TEST=1 with FREESIDE_JUDGMENT_CLI and FREESIDE_JUDGMENT_CLI_SHA256 for the no-spend native cancellation check")
	}
	root := t.TempDir()
	native := filepath.Join(root, "native-cli")
	input, err := os.Open(os.Getenv("FREESIDE_JUDGMENT_CLI")) // #nosec G304 G703 -- intentional operator-selected executable, never a request path; bytes must match its explicit pin.
	if err != nil {
		t.Fatal("pinned native CLI is unavailable")
	}
	output, err := os.OpenFile(native, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500) // #nosec G302 G304 -- fixed basename in a private test directory; verified executable needs owner execute permission.
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	if err := errors.Join(copyErr, input.Close(), output.Close()); err != nil {
		t.Fatal("could not copy pinned native CLI")
	}
	if hex.EncodeToString(hash.Sum(nil)) != os.Getenv("FREESIDE_JUDGMENT_CLI_SHA256") {
		t.Fatal("native CLI content does not match the explicit pin")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Model string `json:"model"`
			Tools []any  `json:"tools"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&request); err != nil || request.Model != "claude-sonnet-4-6" || len(request.Tools) != 0 {
			t.Error("native CLI changed the no-tool model contract")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		first.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	shim := []byte("#!/bin/sh\nexport ANTHROPIC_BASE_URL=" + quote(server.URL) + "\nexec " + quote(native) + " \"$@\"\n")
	shimPath := filepath.Join(root, "localhost-cli")
	if err := os.WriteFile(shimPath, shim, 0o500); err != nil { // #nosec G306 -- executable test shim in a private directory.
		t.Fatal(err)
	}
	shimHash := sha256.Sum256(shim)
	driver, err := claudeinference.New(claudeinference.Config{Binary: shimPath, SHA256: hex.EncodeToString(shimHash[:]), Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "judgments")
	owned, err := ward.NewTaskJudgments(journal, driver)
	if err != nil {
		t.Fatal(err)
	}
	site := inference.ClassifierSite(inference.Budget{})
	fields := make(map[string]string)
	for _, field := range site.Fields {
		fields[field.Name] = "synthetic"
	}
	request := inference.Request{SiteID: site.ID, Fields: fields, MaxOutput: site.MaxOutputBytes, MaxComputeUnits: 1024}
	ctx, cancel := context.WithCancel(ward.WithTaskOwner(t.Context(), "live-task", "live-run"))
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := owned.Complete(ctx, request, "synthetic-test-only"); finished <- err }()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("native CLI ended before provider entry: %v", err)
	}
	if err := owned.ConfirmRun(t.Context(), "live-task", "live-run"); err == nil {
		t.Fatal("active native call was reported quiescent")
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("cancelled provider call reported success")
	}
	reopened, err := ward.NewTaskJudgments(journal, driver)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ConfirmRun(t.Context(), "live-task", "live-run"); err != nil {
		t.Fatalf("native process-group exit proof was not retained: %v", err)
	}
	t.Log("Pinned native CLI entered the local provider, received task cancellation, joined its exact process group, and retained quiescence proof across journal reopen; no real provider credential or call was used.")
}
