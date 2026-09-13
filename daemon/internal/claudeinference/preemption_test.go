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
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func siteRequest(site inference.Site) inference.Request {
	fields := make(map[string]string)
	for _, field := range site.Fields {
		fields[field.Name] = "synthetic"
	}
	return inference.Request{SiteID: site.ID, Fields: fields, MaxOutput: site.MaxOutputBytes, MaxComputeUnits: site.MaxComputeUnits}
}

// A real CLI namer blocks on a FIFO. Every later workflow CLI checks that the
// namer's process is gone and its private scratch was removed before it started.
func blockedNamer(t *testing.T, workflowOutput string) (*Driver, <-chan error, *os.File) {
	t.Helper()
	root := t.TempDir()
	readyPath, releasePath := filepath.Join(root, "ready"), filepath.Join(root, "release")
	for _, path := range []string{readyPath, releasePath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	openFIFO := func(path string) *os.File {
		f, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // G304: fixed FIFO in the test's private directory.
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	ready, release := openFIFO(readyPath), openFIFO(releasePath)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	packet := completion()
	packet["result"] = `{"name":"Name the task"}`
	nameBody, _ := json.Marshal(packet)
	packet["result"] = workflowOutput
	workflowBody, _ := json.Marshal(packet)
	statePath := quote(filepath.Join(root, "previous"))
	script := `input=$(cat)
case "$input" in
  *source_text*)
    printf '%s\n%s\n' "$$" "$PWD" > ` + statePath + `
    printf 'ready\n' > ` + quote(readyPath) + `
    read -r done < ` + quote(releasePath) + `
    printf '%s' ` + quote(string(nameBody)) + `
    ;;
  *)
    previous_pid=$(sed -n '1p' ` + statePath + `)
    previous_root=$(sed -n '2p' ` + statePath + `)
    kill -0 "$previous_pid" 2>/dev/null && exit 21
    test ! -e "$previous_root" || exit 22
    printf '%s' ` + quote(string(workflowBody)) + `
    ;;
esac
`
	d := testDriver(t, script)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	finished := make(chan error, 1)
	go func() {
		_, err := d.Complete(ctx, siteRequest(inference.TaskNamerSite(inference.Budget{})), "token")
		finished <- err
	}()
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return d, finished, release
}

func TestCompletePreemptsNamerAfterProcessAndScratchCleanup(t *testing.T) {
	for _, site := range []inference.Site{inference.ClassifierSite(inference.Budget{}), inference.AdjudicatorSite(inference.Budget{})} {
		t.Run(site.ID, func(t *testing.T) {
			output := completion()["result"].(string)
			if site.ID == inference.AdjudicatorSiteID {
				output = `{"entries":[]}`
			}
			d, finished, _ := blockedNamer(t, output)
			if _, err := d.Complete(t.Context(), siteRequest(site), "token"); err != nil {
				t.Fatal("workflow judgment did not replace namer after cleanup:", err)
			}
			if err := <-finished; err == nil {
				t.Fatal("preempted namer succeeded")
			}
		})
	}
}

func TestCompleteRefusedRequestsDoNotPreemptNamer(t *testing.T) {
	d, finished, release := blockedNamer(t, completion()["result"].(string))
	for _, tc := range []struct {
		name   string
		change func(*inference.Request)
	}{
		{"unsupported", func(r *inference.Request) { r.SiteID = inference.DiagnosticSiteID }},
		{"missing field", func(r *inference.Request) { delete(r.Fields, "message") }},
		{"extra field", func(r *inference.Request) { r.Fields["extra"] = "synthetic" }},
		{"oversized input", func(r *inference.Request) { r.Fields["message"] = strings.Repeat("x", 1<<20) }},
		{"invalid compute", func(r *inference.Request) { r.MaxComputeUnits = 0 }},
		{"invalid output", func(r *inference.Request) { r.MaxOutput = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := classifierRequest()
			tc.change(&req)
			if _, err := d.Complete(t.Context(), req, "token"); err == nil {
				t.Fatal("invalid call succeeded")
			}
		})
	}
	if _, err := d.Complete(t.Context(), classifierRequest(), ""); err == nil {
		t.Fatal("credential-free call succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Complete(ctx, classifierRequest(), "token"); err == nil {
		t.Fatal("canceled call succeeded")
	}
	if _, err := d.Complete(t.Context(), siteRequest(inference.TaskNamerSite(inference.Budget{})), "token"); err == nil {
		t.Fatal("overlapping namer succeeded")
	}
	if _, err := release.WriteString("done\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal("a refused call preempted the original namer:", err)
	}
}

func TestPreemptionWaitKeepsSlotUntilCleanup(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "handoff"
		if canceled {
			name = "waiter deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := &Driver{}
				nameCtx, cancelName := context.WithCancel(t.Context())
				defer cancelName()
				active, err := d.acquire(nameCtx, inference.TaskNamerSiteID, cancelName)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				type acquired struct {
					call *nativeCall
					err  error
				}
				finished := make(chan acquired, 1)
				go func() {
					call, err := d.acquire(ctx, inference.ClassifierSiteID, cancel)
					finished <- acquired{call, err}
				}()
				synctest.Wait()
				if nameCtx.Err() == nil {
					t.Fatal("namer was not canceled")
				}
				for _, site := range []string{inference.TaskNamerSiteID, inference.AdjudicatorSiteID} {
					if _, err := d.acquire(t.Context(), site, func() {}); err == nil {
						t.Fatal("another call bypassed the reserved handoff")
					}
				}
				select {
				case <-finished:
					t.Fatal("waiter returned before cleanup or cancellation")
				default:
				}
				if canceled {
					time.Sleep(time.Second)
					synctest.Wait()
					if got := <-finished; got.err == nil || got.call != nil {
						t.Fatal("expired waiter acquired a call")
					}
					if d.active != active {
						t.Fatal("expired waiter released a still-cleaning call")
					}
					if _, err := d.acquire(t.Context(), inference.TaskNamerSiteID, func() {}); err == nil {
						t.Fatal("namer reused the slot before cleanup")
					}
					// A fresh workflow call may reserve the same retiring call,
					// but it too must wait until cleanup completes.
					go func() {
						call, err := d.acquire(t.Context(), inference.AdjudicatorSiteID, func() {})
						finished <- acquired{call, err}
					}()
					synctest.Wait()
				}
				d.release(active)
				got := <-finished
				if got.err != nil || got.call == nil {
					t.Fatal("waiter did not acquire after cleanup:", got.err)
				}
				if _, err := d.acquire(t.Context(), inference.TaskNamerSiteID, func() {}); err == nil {
					t.Fatal("namer displaced the workflow call")
				}
				d.release(got.call)
			})
		})
	}
}
