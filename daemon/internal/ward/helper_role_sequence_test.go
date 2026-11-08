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

// TestHelperRoleSequence characterizes the handoff's helper roles from the
// outside, so moving them onto one runner shows every behavior change as a
// golden diff. It pins the runtime and journal calls of one successful
// handoff and, for each role and step, the error class, check,
// content-evidence mark, and whether teardown reaped everything.
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
	roles := []struct {
		label, name string
		// setup, when set, makes a failure row's handoff run the role; the
		// successful handoff above runs without it.
		setup func(*testing.T, *handoffFixture, *HandoffSpec)
	}{
		{"prompt-seeder", names.PromptSeeder, nil},
		{"prompt-observer", names.PromptObserver, nil},
		{"inputs-seeder", names.InputsSeeder, nil},
		{"inputs-observer", names.InputsObserver, nil},
		{"instruction-seeder", names.InstructionSeeder, nil},
		{"instruction-observer", names.InstructionObserver, nil},
		{"launch-state-seeder", names.ConfigRootSeeder, nil},
		{"config-root-observer", names.ConfigRootObserver, nil},
		{"continuity-observer", names.ContinuityObserver, nil},
		{"scratch-observer", names.ScratchObserver, nil},
		{"workspace-seeder", names.Seeder, nil},
		{"base-observer", names.Observer, nil},
		{"credential-pre-observer", names.CredObsPre, leaseHelperRoleSequence},
		{"writer-outcome-observer", names.WriterObserver, markHelperRoleSequenceOutcome},
		{"credential-post-observer", names.CredObsPost, leaseHelperRoleSequence},
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
					*row += " " + helperRoleFailureRow(t, role.name, step, role.setup)
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

// leaseHelperRoleSequence gives a failure row's handoff the auth-store lease,
// so the pre- and post-writer credential observers run.
func leaseHelperRoleSequence(t *testing.T, fx *handoffFixture, hs *HandoffSpec) {
	t.Helper()
	leased, _ := fx.leased(t)
	hs.Agent.CredentialMounts = leased.Agent.CredentialMounts
	hs.AuthStoreLease = leased.AuthStoreLease
}

// markHelperRoleSequenceOutcome gives a failure row's writer an outcome
// marker carrying status 0, so the writer-outcome observer runs.
func markHelperRoleSequenceOutcome(t *testing.T, fx *handoffFixture, hs *HandoffSpec) {
	t.Helper()
	hs.Agent.OutcomeMarkerPath = fx.cfg.WorkspaceTarget + "/.freeside/evidence/writer-outcome"
	hs.Agent.Command = []string{"sh", "-c", "printf '%s 0\\n' " + WriterNoncePlaceholder + " > " + hs.Agent.OutcomeMarkerPath}
}

// helperRoleFailureRow runs one handoff that fails the named role at step
// and renders the outcome. Each row is a fresh fixture, prepared by setup
// when the role needs one to run.
func helperRoleFailureRow(
	t *testing.T, name, step string, setup func(*testing.T, *handoffFixture, *HandoffSpec),
) string {
	t.Helper()
	fx, hs := newHelperRoleSequenceFixture(t)
	if setup != nil {
		setup(t, fx, &hs)
	}
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
