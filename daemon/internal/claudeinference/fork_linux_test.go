package claudeinference

import (
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func TestCompleteSurvivesParallelForks(t *testing.T) {
	b, _ := json.Marshal(completion())
	// Padding makes copying overlap more forks without making the shell parse
	// it. testDriver writes the source through execfixture.Write.
	d := testDriver(t, "cat >/dev/null\nprintf '%s' '"+string(b)+"'\nexit 0\n#"+strings.Repeat("x", 256<<10)+"\n")
	var stop atomic.Bool
	var forks, ready sync.WaitGroup
	ready.Add(16)
	for range 16 {
		forks.Go(func() {
			ready.Done()
			for !stop.Load() {
				if err := exec.CommandContext(t.Context(), "/bin/true").Run(); err != nil {
					t.Errorf("parallel fork: %v", err)
					return
				}
			}
		})
	}
	defer func() {
		stop.Store(true)
		forks.Wait()
	}()
	ready.Wait()
	for i := range 100 {
		got, err := d.Complete(t.Context(), classifierRequest(), "synthetic")
		if err != nil {
			t.Fatalf("completion %d: %v", i, err)
		}
		if string(got.Output) != completion()["result"] || got.ComputeUnits != 12 ||
			got.Observed != (inference.Observed{ModelID: "test-model", OutputTokens: 12}) {
			t.Fatalf("completion %d: unexpected response %+v", i, got)
		}
	}
}
