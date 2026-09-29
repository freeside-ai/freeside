package ward

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"testing"
	"time"
)

// TestLiveDeclaredSizeAndMemoryLimitKill proves on the reference runtime that
// a declared size is the realized size, and that a memory-limit kill is named
// from the real boot log, including a kill the container's main process
// survived. Opt-in:
//
//	FREESIDE_WARD_LIVE_TEST=1 go test ./internal/ward -run TestLiveDeclaredSizeAndMemoryLimitKill -v
func TestLiveDeclaredSizeAndMemoryLimitKill(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live size test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, and the pinned alpine:3.22 image)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, err := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); err != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", err, out)
	}
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	ops := runtimeOps{rt: rt}
	size := ContainerSize{CPUs: 1, MemoryMiB: 256}
	suffix := time.Now().Unix()

	for _, tc := range []struct {
		name, script string
		wantKill     bool
	}{
		// sh survives its child's kill and exits 0: only the boot log shows it.
		{"survived child kill", "tail /dev/zero; exit 0", true},
		{"no kill", "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("freeside-ward-live-size-%d-%t", suffix, tc.wantKill)
			t.Cleanup(func() {
				_ = rt.StopContainer(ctx, id)
				_ = rt.DeleteContainer(ctx, id)
			})
			if err := rt.CreateContainer(ctx, ContainerSpec{
				Name: id, Image: liveImage, Command: []string{"sh", "-c", tc.script},
				NetworkDisabled: true, Size: size,
			}); err != nil {
				t.Fatalf("create: %v", err)
			}
			rep, err := rt.Inspect(ctx, id)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if !realizedSize(rep, size) {
				t.Fatalf("realized %d CPUs and %d bytes (observed %t), want %s",
					rep.CPUs, rep.MemoryBytes, rep.ResourcesObserved, size)
			}
			if err := rt.StartContainer(ctx, id); err != nil {
				t.Fatalf("start: %v", err)
			}
			deadline := time.Now().Add(2 * time.Minute)
			for {
				rep, err := rt.Inspect(ctx, id)
				if err == nil && rep.State == StateStopped {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("container did not stop: %+v, %v", rep, err)
				}
				time.Sleep(time.Second)
			}
			observed := ops.memoryLimitKilled(ctx, id)
			if observed.bootLogErr != nil {
				t.Fatal(observed.bootLogErr)
			}
			if observed.memoryLimitKill != tc.wantKill {
				t.Fatalf("memory-limit kill = %t, want %t", observed.memoryLimitKill, tc.wantKill)
			}
		})
	}
}
