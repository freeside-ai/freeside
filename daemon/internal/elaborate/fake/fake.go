// Package fake scripts elaborator results through the shared exec fake. It
// emits the same canonical output bytes the engine accepts from a real agent,
// so workflow tests cannot drift onto a fixture-only contract.
package fake

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// Script registers one successful elaborator invocation with deterministic
// inspect progression and a canonical contract result.
func Script(
	driver *execfake.StageDriver,
	id domain.InvocationID,
	pendingInspects int,
	runningInspects int,
	output elaborate.Output,
) error {
	if driver == nil {
		return fmt.Errorf("script elaborator %q: nil stage driver", id)
	}
	transcript, err := elaborate.EncodeTranscript(output)
	if err != nil {
		return fmt.Errorf("script elaborator %q: %w", id, err)
	}
	driver.Script(id, execfake.StageScript{
		PendingInspects: pendingInspects,
		RunningInspects: runningInspects,
		Outcome:         execfake.OutcomeComplete,
		Result:          exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Elaborator returned structured output."},
		Transcript:      transcript,
	})
	return nil
}
