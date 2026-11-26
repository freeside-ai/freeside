package cordon

import "strings"

// HostFacts is what the program that starts runs holds on its host: the
// two TCC approvals macOS charges to it rather than to the tools a run
// uses, and whether its session has the window server.
//
// macOS charges an approval to a LaunchAgent's program, and every process
// a launch starts inherits that program's responsibility, so an approval
// it holds reaches every cordoned run, which the rule file can't deny
// (#3596 items 4 and 5). A backend therefore refuses every launch while
// either is held (docs/plan.md §5.7, Execution Hosts, Host states), and
// its suite's preconditions fail the same way.
type HostFacts struct {
	ScreenRecording bool
	Accessibility   bool
	DesktopSession  bool
}

// hostFactsReader is the reader New gives every backend. Only this
// package's tests replace it, before any backend is built.
var hostFactsReader = readHostFacts

// checkApprovals names the refusal both a launch and the suite's
// preconditions report.
const checkApprovals = "launcher approvals"

// HostFacts reads the facts the backend's approval check reads, so what a
// host launcher reports is what its launches are held to.
func (b *Backend) HostFacts() (HostFacts, error) { return b.facts() }

// approvalRefusal is the refusal of a launch, or of a pass, while the
// program that starts runs holds either approval. A reader that fails
// refuses too: an approval that can't be read can't be shown absent.
func (b *Backend) approvalRefusal() error {
	facts, err := b.facts()
	if err != nil {
		return failure(checkApprovals, "read the approvals the program that starts runs holds: %v", err)
	}
	var held []string
	if facts.ScreenRecording {
		held = append(held, "screen recording")
	}
	if facts.Accessibility {
		held = append(held, "accessibility")
	}
	if len(held) > 0 {
		return failure(checkApprovals, "the program that starts runs holds the %s approval, which would reach every run",
			strings.Join(held, " and "))
	}
	return nil
}
