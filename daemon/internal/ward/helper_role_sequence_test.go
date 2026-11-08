package ward

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// helperRoleSteps are the points TestHelperRoleSequence fails a role at: its
// create, its first (pre-start) inspect, its allowlist (the first inspect
// reports one extra mount), its start, and its first inspect after start,
// which is the stopped wait's first poll.
var helperRoleSteps = []string{"create", "inspect", "allowlist", "start", "stopped"}

// TestHelperRoleSequence characterizes the handoff's staged-volume,
// launch-state, and instruction helper roles from the outside, so moving
// them onto one runner shows every behavior change as a golden diff. It
// pins the runtime and journal calls of one successful handoff and, for
// each role and step, the error class, check, content-evidence mark, and
// whether teardown reaped everything.
func TestHelperRoleSequence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	out.WriteString("# Successful handoff: runtime and journal calls in order.\n")
	fx, hs := newHelperRoleSequenceFixture(t)
	result, err := fx.backend(t).Handoff(t.Context(), hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })
	for _, call := range fx.rt.calls {
		out.WriteString(normalizeHelperRoleCall(call) + "\n")
	}

	names := namesFor(hs.RunID)
	roles := []struct{ label, name string }{
		{"prompt-seeder", names.PromptSeeder},
		{"prompt-observer", names.PromptObserver},
		{"inputs-seeder", names.InputsSeeder},
		{"inputs-observer", names.InputsObserver},
		{"instruction-seeder", names.InstructionSeeder},
		{"instruction-observer", names.InstructionObserver},
		{"launch-state-seeder", names.ConfigRootSeeder},
		{"config-root-observer", names.ConfigRootObserver},
		{"continuity-observer", names.ContinuityObserver},
		{"scratch-observer", names.ScratchObserver},
	}
	rows := make([]string, 0, len(roles)*len(helperRoleSteps))
	for _, role := range roles {
		for _, step := range helperRoleSteps {
			rows = append(rows, role.label+" "+step)
		}
	}
	// The group returns once every parallel row has run, so rows is complete
	// before the golden comparison.
	t.Run("failures", func(t *testing.T) {
		for i, role := range roles {
			for j, step := range helperRoleSteps {
				row := &rows[i*len(helperRoleSteps)+j]
				t.Run(role.label+"/"+step, func(t *testing.T) {
					t.Parallel()
					*row += " " + helperRoleFailureRow(t, role.name, step)
				})
			}
		}
	})
	out.WriteString("\n# Injected failures: role, step, error class, content evidence, teardown.\n")
	for _, row := range rows {
		out.WriteString(row + "\n")
	}
	golden.Assert(t, "helper-role-sequence", out.Bytes())
}

// newHelperRoleSequenceFixture configures a handoff in which every moved
// role runs: a Claude clean launch state (the three state volumes and the
// composed instruction bundle), a prompt, and inputs. The journal records
// into the runtime's call list, so the golden pins journal writes against
// the role steps around them.
func newHelperRoleSequenceFixture(t *testing.T) (*handoffFixture, HandoffSpec) {
	t.Helper()
	fx := newHandoffFixture(t)
	journal := newFakeJournal()
	journal.rt = fx.rt
	fx.cfg.Journal = journal
	files, blobs := testInputs("task/shot.png", "pixels", "task/docs/notes.md", "notes")
	fx.cfg.InputBlobs = blobs
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("approved prompt"))
	hs.Agent.Inputs = files
	return fx, hs
}

// helperRoleFailureRow runs one handoff that fails the named role at step
// and renders the outcome. Each row is a fresh fixture.
func helperRoleFailureRow(t *testing.T, name, step string) string {
	t.Helper()
	fx, hs := newHelperRoleSequenceFixture(t)
	injected := errors.New("injected " + step + " failure")
	switch step {
	case "create":
		fx.rt.onCreateContainer = func(spec ContainerSpec) error {
			if spec.Name == name {
				return injected
			}
			return nil
		}
	case "start":
		fx.rt.onStart = func(id string) error {
			if id == name {
				return injected
			}
			return nil
		}
	case "inspect", "allowlist", "stopped":
		// The role's first inspect is its pre-start allowlist inspect; its
		// second is the stopped wait's first poll. The hook runs under the
		// fake's lock, so it only counts.
		inspects := 0
		fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
			if id != name {
				return rep, nil
			}
			inspects++
			switch {
			case step == "inspect" && inspects == 1, step == "stopped" && inspects == 2:
				return InspectReport{}, injected
			case step == "allowlist" && inspects == 1:
				rep.Mounts = append(rep.Mounts, Mount{
					Type: MountVolume, Source: "injected-extra", Target: "/injected-extra",
				})
			}
			return rep, nil
		}
	default:
		t.Fatalf("unknown helper-role step %q", step)
	}
	result, err := fx.backend(t).Handoff(t.Context(), hs)
	if result != nil {
		_ = os.RemoveAll(result.ExportDir)
	}
	if err == nil {
		t.Errorf("%s %s: Handoff succeeded, want an error", name, step)
		return "succeeded"
	}
	class := "plain"
	var cf *ConformanceFailure
	if errors.As(err, &cf) {
		class = "conformance:" + string(cf.Check)
	}
	evidence := "no-evidence"
	if errors.Is(err, errContentEvidence) {
		evidence = "evidence"
	}
	return class + " " + evidence + " " + fx.rt.leftovers()
}

// leftovers renders what teardown left in the fake runtime: "reaped" when
// nothing, otherwise the sorted names.
func (f *fakeRuntime) leftovers() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var left []string
	for name := range f.ctrs {
		left = append(left, "container:"+name)
	}
	for name := range f.vols {
		left = append(left, "volume:"+name)
	}
	for name := range f.nets {
		left = append(left, "network:"+name)
	}
	if len(left) == 0 {
		return "reaped"
	}
	slices.Sort(left)
	return "leftover=" + strings.Join(left, ",")
}

// normalizeHelperRoleCall replaces the host temp directory in a recorded
// copy, the one call field that differs between runs.
func normalizeHelperRoleCall(call string) string {
	fields := strings.Fields(call)
	if len(fields) == 4 && fields[0] == "copy" {
		fields[2] = "<host-dir>"
		return strings.Join(fields, " ")
	}
	return call
}
