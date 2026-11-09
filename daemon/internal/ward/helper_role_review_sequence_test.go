package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// codexReviewHelperRoleSteps adds the journal step, the fingerprint mark
// every Codex role writes before start, to the steps TestHelperRoleSequence
// fails a role at.
var codexReviewHelperRoleSteps = append(slices.Clone(helperRoleSteps), "journal")

// TestCodexReviewHelperRoleSequence characterizes the Codex review's helper
// roles from the outside, so moving them onto the ward runner shows every
// behavior change as a golden diff. It pins the runtime calls and journal
// marks of one successful review launch with inputs and, for each role and
// step, the error class, check, whether the error is operational, the
// content-evidence mark, the role's own journal state, and what the
// runtime holds afterward.
func TestCodexReviewHelperRoleSequence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	out.WriteString("# Successful Codex review launch with inputs: runtime calls and journal marks in order.\n")
	lifecycle, rt, cfg, launchSpec, _ := newCodexReviewHelperRoleFixture(t)
	launch, err := lifecycle.CodexReview(t.Context(), cfg, launchSpec)
	if err != nil {
		t.Fatalf("CodexReview = %v, want success", err)
	}
	rt.mu.Lock()
	calls := slices.Clone(rt.calls)
	rt.mu.Unlock()
	if err := launch.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	for _, call := range calls {
		out.WriteString(normalizeHelperRoleCall(call) + "\n")
	}

	names := codexReviewNames(launchSpec.RunID)
	roles := []struct{ label, name string }{
		{"workspace-observer", names.workspaceObserver},
		{"shadow-initializer", names.shadowInitializer},
		{"shadow-observer", names.shadowObserver},
		{"snapshot-seeder", names.snapshotSeeder},
		{"snapshot-observer", names.snapshotObserver},
		{"inputs-seeder", names.inputsSeeder},
		{"inputs-observer", names.inputsObserver},
	}
	rows := make([]string, 0, len(roles)*len(codexReviewHelperRoleSteps))
	for _, role := range roles {
		for _, step := range codexReviewHelperRoleSteps {
			rows = append(rows, role.label+" "+step)
		}
	}
	// The group returns once every parallel row has run, so rows is complete
	// before the golden comparison.
	t.Run("failures", func(t *testing.T) {
		for i, role := range roles {
			for j, step := range codexReviewHelperRoleSteps {
				row := &rows[i*len(codexReviewHelperRoleSteps)+j]
				t.Run(role.label+"/"+step, func(t *testing.T) {
					t.Parallel()
					*row += " " + codexReviewHelperRoleFailureRow(t, role.name, step)
				})
			}
		}
	})
	out.WriteString("\n# Injected failures: role, step, error class, operational, content evidence, journal, runtime.\n")
	for _, row := range rows {
		out.WriteString(row + "\n")
	}
	golden.Assert(t, "codex-review-helper-role-sequence", out.Bytes())
}

// recordingCodexReviewJournal records each accepted resource mark into the
// runtime's call list, so the golden pins marks against the role steps
// around them, and can fail the fingerprinted mark of one resource.
type recordingCodexReviewJournal struct {
	*fakeCodexReviewJournal
	rt *fakeRuntime
	// failFingerprintMark names the resource whose fingerprinted mark fails.
	failFingerprintMark string
	// marked is each resource's last accepted mark: "fingerprint" when it
	// carried one, "token" otherwise.
	marked map[string]string
}

func (j *recordingCodexReviewJournal) MarkCodexReviewIntentResource(
	ctx context.Context, runID string, resource CodexReviewIntentResource,
) error {
	if resource.Fingerprint != "" && resource.Name == j.failFingerprintMark {
		return errors.New("injected journal failure")
	}
	if err := j.fakeCodexReviewJournal.MarkCodexReviewIntentResource(ctx, runID, resource); err != nil {
		return err
	}
	j.rt.mu.Lock()
	defer j.rt.mu.Unlock()
	j.rt.record("mark %s token=%s fingerprint=%s",
		resource.Name, setOrEmpty(resource.OwnershipToken), setOrEmpty(resource.Fingerprint))
	j.marked[resource.Name] = "token"
	if resource.Fingerprint != "" {
		j.marked[resource.Name] = "fingerprint"
	}
	return nil
}

// journalState renders the named resource's last accepted mark.
func (j *recordingCodexReviewJournal) journalState(name string) string {
	j.rt.mu.Lock()
	defer j.rt.mu.Unlock()
	if state, ok := j.marked[name]; ok {
		return "journal=" + state
	}
	return "journal=none"
}

func setOrEmpty(value string) string {
	if value == "" {
		return "empty"
	}
	return "set"
}

// newCodexReviewHelperRoleFixture configures a Codex review launch in which
// every helper role runs: the workspace observer, the shadow and snapshot
// seeders and observers, and, because the launch delivers inputs, the input
// seeder and observer.
func newCodexReviewHelperRoleFixture(t *testing.T) (
	*CodexReviewLifecycle, *fakeRuntime, CodexReviewConfig, CodexReviewLaunchSpec, *recordingCodexReviewJournal,
) {
	t.Helper()
	lifecycle, rt, cfg, launchSpec, fake := testCodexReviewLifecycle(t)
	journal := &recordingCodexReviewJournal{fakeCodexReviewJournal: fake, rt: rt, marked: map[string]string{}}
	cfg.Journal = journal
	files, blobs := testInputs("task/shot.png", "pixels", "task/docs/notes.md", "notes")
	lifecycle.cfg.InputBlobs = blobs
	launchSpec.Inputs = files
	return lifecycle, rt, cfg, launchSpec, journal
}

// codexReviewHelperRoleFailureRow runs one review launch that fails the
// named role at step and renders the outcome. Each row is a fresh fixture.
// The observers run again in the pre-start re-observation, so every
// injection targets the role's first run.
func codexReviewHelperRoleFailureRow(t *testing.T, name, step string) string {
	t.Helper()
	lifecycle, rt, cfg, launchSpec, journal := newCodexReviewHelperRoleFixture(t)
	if step == "journal" {
		journal.failFingerprintMark = name
	} else {
		injectHelperRoleFailure(t, rt, name, step, func(id string) bool { return id == name })
	}
	launch, err := lifecycle.CodexReview(t.Context(), cfg, launchSpec)
	if err == nil {
		_ = launch.Close()
		t.Errorf("%s %s: CodexReview succeeded, want an error", name, step)
		return "succeeded"
	}
	operational := "not-operational"
	if errors.Is(err, ErrCodexReviewOperational) {
		operational = "operational"
	}
	return helperRoleErrorClass(err) + " " + operational + " " + helperRoleEvidence(err) + " " +
		journal.journalState(name) + " " + rt.leftovers()
}

// TestClaudeCredentialSeederSequence characterizes the Claude enrollment
// credential seeder from the outside: the runtime calls of one successful
// enrollment and, for each step, the error class, check, content-evidence
// mark, and what the runtime holds afterward.
func TestClaudeCredentialSeederSequence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	out.WriteString("# Successful Claude enrollment: runtime calls in order.\n")
	fx := newClaudeEnrollmentFixture(t)
	if _, err := enrollClaudeInBubble(t, fx.cfg); err != nil {
		t.Fatalf("EnrollClaudeSetupToken = %v, want success", err)
	}
	normalize := claudeEnrollmentCallNormalizer()
	for _, call := range fx.rt.calls {
		out.WriteString(normalize(normalizeHelperRoleCall(call)) + "\n")
	}
	rows := make([]string, len(helperRoleSteps))
	t.Run("failures", func(t *testing.T) {
		for i, step := range helperRoleSteps {
			t.Run(step, func(t *testing.T) {
				t.Parallel()
				rows[i] = "credential-seeder " + step + " " + claudeCredentialSeederFailureRow(t, step)
			})
		}
	})
	out.WriteString("\n# Injected failures: role, step, error class, content evidence, runtime.\n")
	for _, row := range rows {
		out.WriteString(row + "\n")
	}
	golden.Assert(t, "claude-credential-seeder-sequence", out.Bytes())
}

// claudeCredentialSeederPrefix is the seeder's name before the per-enrollment
// ownership suffix.
const claudeCredentialSeederPrefix = "freeside-enroll-credential-"

// enrollmentNameSuffix matches the random 12-hex-digit suffix every
// enrollment helper's name carries.
var enrollmentNameSuffix = regexp.MustCompile(`-[0-9a-f]{12}\b`)

// claudeEnrollmentCallNormalizer replaces each helper name's random suffix
// with a placeholder numbered by first appearance, so the trace still shows
// which calls share a container.
func claudeEnrollmentCallNormalizer() func(string) string {
	seen := map[string]string{}
	return func(call string) string {
		return enrollmentNameSuffix.ReplaceAllStringFunc(call, func(suffix string) string {
			if _, ok := seen[suffix]; !ok {
				seen[suffix] = fmt.Sprintf("-<id%d>", len(seen)+1)
			}
			return seen[suffix]
		})
	}
}

// claudeCredentialSeederFailureRow runs one enrollment that fails the
// credential seeder at step and renders the outcome. The fixture's hooks
// stay in place: injectHelperRoleFailure chains to them.
func claudeCredentialSeederFailureRow(t *testing.T, step string) string {
	t.Helper()
	fx := newClaudeEnrollmentFixture(t)
	injectHelperRoleFailure(t, fx.rt.fakeRuntime, "", step, func(id string) bool {
		return strings.HasPrefix(id, claudeCredentialSeederPrefix)
	})
	_, err := enrollClaudeInBubble(t, fx.cfg)
	if err == nil {
		t.Errorf("credential seeder %s: enrollment succeeded, want an error", step)
		return "succeeded"
	}
	return helperRoleErrorClass(err) + " " + helperRoleEvidence(err) + " " + fx.rt.leftovers()
}

// injectHelperRoleFailure fails the role matched by isRole at step, the way
// helperRoleFailureRow does. A create or start hook already set runs only
// for calls the injection passes; an inspect hook already set runs first,
// and the injection counts and edits its report. name labels the failure
// only.
func injectHelperRoleFailure(t *testing.T, rt *fakeRuntime, name, step string, isRole func(string) bool) {
	t.Helper()
	injected := errors.New("injected " + step + " failure " + name)
	switch step {
	case "create":
		previous := rt.onCreateContainer
		rt.onCreateContainer = func(spec ContainerSpec) error {
			if isRole(spec.Name) {
				return injected
			}
			if previous != nil {
				return previous(spec)
			}
			return nil
		}
	case "start":
		previous := rt.onStart
		rt.onStart = func(id string) error {
			if isRole(id) {
				return injected
			}
			if previous != nil {
				return previous(id)
			}
			return nil
		}
	case "inspect", "allowlist", "stopped":
		// The role's first inspect is its pre-start allowlist inspect; its
		// second is the stopped wait's first poll. The hook runs under the
		// fake's lock, so it only counts.
		previous := rt.onInspect
		inspects := 0
		rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
			if previous != nil {
				var err error
				if rep, err = previous(id, rep); err != nil {
					return rep, err
				}
			}
			if !isRole(id) {
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
}

func helperRoleErrorClass(err error) string {
	var cf *ConformanceFailure
	if errors.As(err, &cf) {
		return "conformance:" + string(cf.Check)
	}
	return "plain"
}

func helperRoleEvidence(err error) string {
	if errors.Is(err, errContentEvidence) {
		return "evidence"
	}
	return "no-evidence"
}
