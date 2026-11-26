package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// composeCordon gives the fixture's admitter a cordon declaring caps, as
// WithCordonedWriters does.
func (f inputFilesFixture) composeCordon(caps ...exec.Capability) {
	cordon := execfake.RunnerBackend{
		BackendName: string(domain.BackendCordonedHostProcess), Caps: exec.NewCapabilitySet(caps...),
	}
	f.engine.admission.cordon, f.engine.admission.cordonConfigurationDigest = cordon, cordon.ConfigurationDigest()
}

// setProjectWriterClass stands in for the project's writer_class setting.
func (f inputFilesFixture) setProjectWriterClass(class *domain.RunnerBackendClass) {
	setWriterClass(f.engine, class)
}

func (f inputFilesFixture) requireTaskWarded(t *testing.T) {
	t.Helper()
	if err := f.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.SetTaskWriterClass(t.Context(), domain.TaskWriterClass{
			TaskID: f.taskID, Class: domain.BackendFreshVMReadOnlyVolumeHandoff,
			SetBy: "cli:operator", SetAt: agentTestAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// record stores admission as the engine does, under a cordoned launch cap
// the store requires of a cordoned one.
func (f inputFilesFixture) record(t *testing.T, admission domain.ExecutionAdmission) {
	t.Helper()
	recordCordonedCap(t, f.store, 1, agentTestAt)
	f.agentAdmissionFixture.record(t, admission)
}

// recordFirstWriter stores admission as the run's first writer admission
// and binds the fixture's run to the stage the store now holds, as the
// engine's next pass reads it.
func (f inputFilesFixture) recordFirstWriter(t *testing.T, admission domain.ExecutionAdmission) inputFilesFixture {
	t.Helper()
	f.record(t, admission)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), f.run.ID)
		f.binding.run = run
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// An implementer resolves its class from the task's requirement, then the
// project's setting, then the cordon. A cordoned admission records the
// host's open network and the selected image, and the store accepts it; a
// warded one is placed on the ward as before (#3472).
func TestWriterAdmissionResolvesTheWriterClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		project    *domain.RunnerBackendClass
		taskWarded bool
		cordoned   bool
	}{
		{name: "no setting", cordoned: true},
		{name: "cordoned project", project: new(domain.BackendCordonedHostProcess), cordoned: true},
		{name: "warded project", project: new(domain.BackendFreshVMReadOnlyVolumeHandoff)},
		{name: "warded task", project: new(domain.BackendCordonedHostProcess), taskWarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInputFilesFixture(t)
			f.composeCordon()
			f.setProjectWriterClass(tc.project)
			if tc.taskWarded {
				f.requireTaskWarded(t)
			}
			admission, admitted, err := f.admitAs(t, domain.RoleImplementer)
			if err != nil || !admitted {
				t.Fatalf("admit = %t, %v", admitted, err)
			}
			wantBackend, wantEgress := stageInputBackend{}.Name(), domain.EgressProviderOnly
			if tc.cordoned {
				wantBackend, wantEgress = string(domain.BackendCordonedHostProcess), domain.EgressHostOpen
			}
			if admission.Backend != wantBackend || admission.EgressProfile != wantEgress {
				t.Fatalf("admission placed on %s with %s, want %s with %s",
					admission.Backend, admission.EgressProfile, wantBackend, wantEgress)
			}
			if admission.ImageRef != f.engine.admission.environment.ImageRef {
				t.Fatalf("admission image = %s, want the selected %s",
					admission.ImageRef, f.engine.admission.environment.ImageRef)
			}
			f.record(t, admission)
		})
	}
}

// A project with no project image pins its recipe binding on each
// admission, by value, and its writer resolves to the cordon; a warded
// project setting is refused at admission, naming the missing image (#3620).
func TestWriterAdmissionPinsTheRecipeBinding(t *testing.T) {
	t.Parallel()
	binding := domain.RecipeBinding{
		Repository: "owner/repo", RepositoryID: 1, CommitSHA: strings.Repeat("c", 40),
		RecipeDigest: domain.Digest("sha256:" + strings.Repeat("e", 64)),
	}
	// noImageProject stands in for the authority of a project that binds
	// its recipe with binding and has no image.
	noImageProject := func(e *Engine, class *domain.RunnerBackendClass) {
		base := e.admission.environment.Base
		e.projectAuthoritySource = FixedProjectAuthority(ProjectAuthority{
			Repo: base.Repo, RepositoryID: base.RepositoryID, BaseRef: base.BaseRef,
			WriterClass: class, RecipeBinding: &binding,
		})
	}
	f := newInputFilesFixture(t)
	f.composeCordon()
	noImageProject(f.engine, nil)
	admission, admitted, err := f.admitAs(t, domain.RoleImplementer)
	if err != nil || !admitted {
		t.Fatalf("admit = %t, %v", admitted, err)
	}
	if admission.RecipeBinding == nil || *admission.RecipeBinding != binding || admission.RecipeBinding == &binding {
		t.Fatalf("admission recipe binding = %+v, want a copy of %+v", admission.RecipeBinding, binding)
	}
	if admission.Backend != string(domain.BackendCordonedHostProcess) {
		t.Fatalf("admission placed on %s, want the cordon", admission.Backend)
	}
	f.record(t, admission)

	warded := newInputFilesFixture(t)
	warded.composeCordon()
	noImageProject(warded.engine, new(domain.BackendFreshVMReadOnlyVolumeHandoff))
	if _, _, err := warded.admitAs(t, domain.RoleImplementer); !errors.Is(err, domain.ErrWardedWithoutProjectImage) {
		t.Fatalf("warded project with no image: admit err = %v, want ErrWardedWithoutProjectImage", err)
	}
}

// The specifier has no writer class: it runs on the ward even where a
// writer would resolve to a cordon this daemon lacks.
func TestSpecifierAdmissionResolvesNoWriterClass(t *testing.T) {
	t.Parallel()
	f := newInputFilesFixture(t)
	f.setProjectWriterClass(nil)
	f.bindConversation(t, 1, inputMessage(t, 1, domain.AuthorUser))
	admission, admitted, err := f.admitAs(t, domain.RoleSpecifier)
	if err != nil || !admitted {
		t.Fatalf("admit = %t, %v", admitted, err)
	}
	if admission.Backend != (stageInputBackend{}).Name() {
		t.Fatalf("specifier placed on %s", admission.Backend)
	}
}

// A writer resolved to the cordon holds only its own invocation under
// backend_not_conformant when the daemon composed no cordon, or one with no
// current proof under unattended. It never falls back to the ward.
func TestCordonedWriterHoldsWithoutAProvenCordon(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		compose bool
		mode    domain.OperatingMode
	}{
		{name: "no cordon", mode: domain.ModeAttendedDev},
		{name: "unproven cordon", compose: true, mode: domain.ModeUnattended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInputFilesFixture(t)
			f.setProjectWriterClass(nil)
			if tc.compose {
				f.composeCordon()
			}
			f.engine.admission.environment.OperatingMode = tc.mode
			_, admitted, err := f.admitAs(t, domain.RoleImplementer)
			if admitted || !errors.Is(err, ErrCordonNotConformant) {
				t.Fatalf("admit = %t, %v; want ErrCordonNotConformant", admitted, err)
			}
			if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldBackendNotConformant {
				t.Fatalf("hold reason = %q, %t; want backend_not_conformant", reason, ok)
			}
			// Dispatch checks the invocation hold before the pass refusal
			// store.ErrBackendNotConformant also matches.
			if !invocationDispatchHold(err) {
				t.Fatal("the refusal does not hold only its own invocation")
			}
		})
	}
}

// The run's first writer admission fixes its class: a remediation round
// and an operator-feedback turn follow it, and a later admission whose
// settings now resolve another class is refused with one notice.
// TestReadmissionKeepsACordonedAdmissionsClass drives the same refusal
// through re-admission.
func TestLaterWriterAdmissionsFollowTheRunsFirstWriterClass(t *testing.T) {
	t.Parallel()
	for _, first := range []domain.RunnerBackendClass{
		domain.BackendCordonedHostProcess, domain.BackendFreshVMReadOnlyVolumeHandoff,
	} {
		t.Run(string(first), func(t *testing.T) {
			t.Parallel()
			f := newInputFilesFixture(t)
			f.composeCordon()
			f.setProjectWriterClass(new(first))
			admission, _, err := f.admitAs(t, domain.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			f = f.recordFirstWriter(t, admission)

			remediation, _, err := f.admitAs(t, domain.RoleRemediator)
			if err != nil || remediation.Backend != admission.Backend {
				t.Fatalf("remediation admitted on %s, %v; want the first writer's %s",
					remediation.Backend, err, admission.Backend)
			}
			feedback := f.binding
			feedbackID := domain.InvocationID("inv-feedback-1")
			feedback.invocation.ID = feedbackID
			stage := domain.Stage{ID: operatorFeedbackStageID(feedbackID), RunID: f.run.ID, Name: productionStageName}
			turn, _, _, err := f.engine.admitAttempt(t.Context(), feedback, stage, feedbackID)
			if err != nil || turn.Backend != admission.Backend {
				t.Fatalf("feedback turn admitted on %s, %v; want the first writer's %s",
					turn.Backend, err, admission.Backend)
			}

			other := domain.BackendFreshVMReadOnlyVolumeHandoff
			if first == other {
				other = domain.BackendCordonedHostProcess
			}
			f.setProjectWriterClass(new(other))
			for range 2 {
				_, admitted, err := f.admitAs(t, domain.RoleRemediator)
				if admitted || !errors.Is(err, ErrWriterClassChanged) {
					t.Fatalf("admit after the change = %t, %v; want ErrWriterClassChanged", admitted, err)
				}
				if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldBackendNotConformant ||
					!invocationDispatchHold(err) {
					t.Fatalf("hold = %q, %t; want an invocation hold under backend_not_conformant", reason, ok)
				}
			}
			for occurrence, want := range []bool{true, false} {
				_, found, err := readProductionQuarantineItem(t.Context(), f.store,
					productionQuarantineOccurrenceID(writerClassChangedPrefix, f.run.ID, occurrence+1))
				if err != nil || found != want {
					t.Fatalf("notice %d found %t, %v; want %t", occurrence+1, found, err, want)
				}
			}

			f.setProjectWriterClass(new(first))
			if _, _, err := f.admitAs(t, domain.RoleRemediator); err != nil {
				t.Fatalf("admit after the setting was restored: %v", err)
			}
		})
	}
}

// A cordoned writer can't receive attached files, so its admission is
// refused as undeliverable, naming the warded setting, before anything is
// recorded; a warded writer receives them as before.
func TestCordonedWriterRefusesAttachments(t *testing.T) {
	t.Parallel()
	f := newInputFilesFixture(t)
	f.composeCordon()
	f.setProjectWriterClass(nil)
	f.putTaskAttachments(t, []domain.TaskAttachment{
		taskAttachment("notes.txt", domain.TaskAttachmentMediaOctetStream, []byte("notes")),
	})
	_, admitted, err := f.admitAs(t, domain.RoleImplementer)
	if admitted || !errors.Is(err, errOperatorInputsUndeliverable) {
		t.Fatalf("admit = %t, %v; want the attachments refused", admitted, err)
	}
	f.setProjectWriterClass(new(domain.BackendFreshVMReadOnlyVolumeHandoff))
	if _, _, err := f.admitAs(t, domain.RoleImplementer); err != nil {
		t.Fatalf("warded admit: %v", err)
	}
}

// unboundCordon is a cordon whose configuration names no topology.
type unboundCordon struct{ execfake.RunnerBackend }

func (unboundCordon) ConfigurationDigest() domain.Digest {
	return domain.UnboundBackendConfigurationDigest
}

// WithCordonedWriters composes only a cordon by its class name, after
// WithAdmission, and under unattended only one whose configuration digest
// binds, since that digest is the proof admission records.
func TestWithCordonedWritersRefusesAnUnusableCordon(t *testing.T) {
	t.Parallel()
	cordon := execfake.RunnerBackend{BackendName: string(domain.BackendCordonedHostProcess)}
	admitterIn := func(mode domain.OperatingMode) *admitter {
		return &admitter{environment: AdmissionEnvironment{OperatingMode: mode}}
	}
	for _, tc := range []struct {
		name      string
		cordon    exec.ConfigurationBoundBackend
		admission *admitter
	}{
		{name: "nil backend", admission: admitterIn(domain.ModeAttendedDev)},
		{name: "ward backend", cordon: execfake.RunnerBackend{
			BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		}, admission: admitterIn(domain.ModeAttendedDev)},
		{name: "admission not configured", cordon: cordon},
		{
			name: "unbound digest under unattended", cordon: unboundCordon{cordon},
			admission: admitterIn(domain.ModeUnattended),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{admission: tc.admission}
			if err := WithCordonedWriters(tc.cordon)(e); err == nil {
				t.Fatal("WithCordonedWriters accepted the cordon")
			}
			if tc.admission != nil && tc.admission.cordon != nil {
				t.Fatal("a refused cordon was composed")
			}
		})
	}

	for _, tc := range []struct {
		cordon exec.ConfigurationBoundBackend
		mode   domain.OperatingMode
	}{
		{cordon: cordon, mode: domain.ModeUnattended},
		{cordon: unboundCordon{cordon}, mode: domain.ModeAttendedDev},
	} {
		e := &Engine{admission: admitterIn(tc.mode)}
		if err := WithCordonedWriters(tc.cordon)(e); err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		if e.admission.cordon == nil ||
			e.admission.cordonConfigurationDigest != tc.cordon.ConfigurationDigest() {
			t.Fatalf("%s: cordon not composed with its digest", tc.mode)
		}
	}
}

// A stale cordoned admission is checked against the cordon's running
// digest, never the ward's, and re-admitted only through the run's pin:
// when the cordon is reconfigured and the project now sets warded, the
// re-admission is refused and nothing is superseded; once both are
// restored, the attempt starts under its own admission (#3472).
func TestReadmissionKeepsACordonedAdmissionsClass(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newCordonedImplementer(t)
	c.engine.driver = &failFirstStart{StageDriver: c.engine.driver}
	recordCordonedCap(t, c.store, 1, *c.now)
	if _, err := c.engine.Reconcile(ctx); err == nil {
		t.Fatal("the injected start failure did not surface")
	}
	storedAdmission := func() (domain.ExecutionAdmission, int) {
		t.Helper()
		var (
			admission  domain.ExecutionAdmission
			superseded []domain.ExecutionAdmission
		)
		if err := c.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			if admission, err = tx.GetExecutionAdmissionRecord(ctx, c.id); err != nil {
				return err
			}
			superseded, err = tx.ListSupersededExecutionAdmissions(ctx, c.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return admission, len(superseded)
	}
	stale, _ := storedAdmission()
	if stale.Backend != string(domain.BackendCordonedHostProcess) {
		t.Fatalf("the stale admission is placed on %s, want the cordon", stale.Backend)
	}

	reconfigured := domain.Digest("sha256:" + strings.Repeat("7", 64))
	proof, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendCordonedHostProcess, Outcome: domain.ConformancePassed,
		ConfigurationDigest: reconfigured, Capabilities: domain.CordonedCapabilities(),
		ProvedAt: unattendedExportEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, domain.DaemonHostID, proof)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c.engine.admission.cordonConfigurationDigest = reconfigured
	setWriterClass(c.engine, new(domain.BackendFreshVMReadOnlyVolumeHandoff))
	if started, err := c.engine.dispatchPendingInvocations(ctx); err != nil || started != 0 {
		t.Fatalf("dispatch = %d, %v; want the pass to continue with nothing started", started, err)
	}
	if _, started := c.driver.StartSpec(c.id); started {
		t.Fatal("a re-admission that changes the run's writer class started")
	}
	if hold := storedRunHold(t, c.store, c.runID); hold == nil || hold.Reason != domain.HoldBackendNotConformant {
		t.Fatalf("hold = %+v, want backend_not_conformant", hold)
	}
	if got, superseded := storedAdmission(); got.ID != stale.ID || superseded != 0 {
		t.Fatalf("stored admission %s with %d superseded, want the stale %s alone", got.ID, superseded, stale.ID)
	}

	c.engine.admission.cordonConfigurationDigest = c.cordon.ConfigurationDigest()
	setWriterClass(c.engine, nil)
	if started, err := c.engine.dispatchPendingInvocations(ctx); err != nil || started != 1 {
		t.Fatalf("dispatch = %d, %v; want the attempt started", started, err)
	}
	spec, started := c.driver.StartSpec(c.id)
	if !started || spec.AdmissionID != stale.ID {
		t.Fatalf("started %t under admission %s, want the stale %s", started, spec.AdmissionID, stale.ID)
	}
	if _, superseded := storedAdmission(); superseded != 0 {
		t.Fatalf("an admission bound to the running cordon was superseded %d times", superseded)
	}
}
