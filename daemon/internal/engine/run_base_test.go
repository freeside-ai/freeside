package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testTip is a tip the composition would derive: the agent admission
// fixture's repository and ref at sha.
func testTip(sha string) domain.BaseRevision {
	return domain.BaseRevision{Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: sha}
}

// deriveTip makes e derive *tip for every attempt, as the composition does
// from the stored tip observation.
func deriveTip(e *Engine, tip *domain.BaseRevision) {
	e.derive = func(_ context.Context, id domain.InvocationID) (string, domain.BaseRevision, error) {
		return "workspace-" + string(id), *tip, nil
	}
}

func getRun(t *testing.T, st *store.Store, id domain.RunID) domain.Run {
	t.Helper()
	var run domain.Run
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// seedWriterRun stores a run with a production stage and no attempts, bound
// to the fixture's policy and specification.
func seedWriterRun(t *testing.T, f *agentAdmissionFixture, id domain.RunID, base *domain.BaseRevision) domain.Run {
	t.Helper()
	return seedRunPolicy(t, f.store, domain.Run{
		ID: id, ProjectID: f.run.ProjectID, SpecDigest: f.run.SpecDigest, Base: base,
		Stages: []domain.Stage{{ID: productionStageID(id), RunID: id, Name: productionStageName}},
	}, nil)
}

func writerBinding(t *testing.T, run domain.Run, id domain.InvocationID) invocationBinding {
	t.Helper()
	invocation, err := domain.NewAgentInvocation(id, []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return invocationBinding{run: run, invocation: invocation}
}

// A run's first admission records its base, and every later writer
// admission of the run starts there after the tip has moved: a remediation
// round and an operator-feedback turn. TestARetryAttemptStartsAtTheRunBase
// covers a retry, which needs a failed attempt.
func TestLaterAdmissionsStartAtTheRunBase(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.engine.productionPublication = &productionPublicationWorkflow{
		remediationPromptPackage: agentTestDigest("8"),
	}
	tip := testTip("1111111111111111111111111111111111111111")
	deriveTip(f.engine, &tip)
	run := seedWriterRun(t, f, "run-base", nil)
	first := productionInvocationID(run.ID)
	admission, base, admitted, err := f.engine.admitAttempt(
		t.Context(), writerBinding(t, run, first), run.Stages[0], first)
	if err != nil || !admitted {
		t.Fatalf("first admission: admitted %t, %v", admitted, err)
	}
	if base != tip || admission.Base != tip {
		t.Fatalf("first admission at %+v with run base %+v, want the tip %+v", admission.Base, base, tip)
	}
	if _, _, _, err := f.engine.recordAttempt(
		t.Context(), run.ID, run.Stages[0].ID, first, nil, "pending", &admission, base,
	); err != nil {
		t.Fatal(err)
	}
	run = getRun(t, f.store, run.ID)
	if run.Base == nil || *run.Base != tip {
		t.Fatalf("recorded run base = %+v, want %+v", run.Base, tip)
	}
	recorded := tip

	tip = testTip("2222222222222222222222222222222222222222")
	feedbackID := operatorFeedbackInvocationID("command-1")
	for name, tc := range map[string]struct {
		stage domain.StageID
		id    domain.InvocationID
	}{
		"remediation":       {remediationStageID(run.ID, 1), remediationInvocationID(run.ID, 1)},
		"operator feedback": {operatorFeedbackStageID(feedbackID), feedbackID},
	} {
		t.Run(name, func(t *testing.T) {
			stage := domain.Stage{ID: tc.stage, RunID: run.ID, Name: productionStageName}
			admission, base, admitted, err := f.engine.admitAttempt(
				t.Context(), writerBinding(t, run, tc.id), stage, tc.id)
			if err != nil || !admitted {
				t.Fatalf("admitted %t, %v", admitted, err)
			}
			if base != recorded || admission.Base != recorded {
				t.Fatalf("admitted at %+v with run base %+v after the tip moved, want %+v",
					admission.Base, base, recorded)
			}
			if admission.Workspace != "workspace-"+string(tc.id) {
				t.Fatalf("workspace = %q, want the derived one", admission.Workspace)
			}
		})
	}
}

// A run stored before #3203 has admissions and no base. Its next admission
// adopts the base of its earliest recorded admission, and recording it
// stores that base on the run. A run with no admissions takes the tip.
func TestARunStoredWithoutABaseAdoptsItsEarliestAdmission(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	tip := testTip("1111111111111111111111111111111111111111")
	deriveTip(f.engine, &tip)
	admission, _, admitted, err := f.engine.admitAttempt(t.Context(), f.binding, f.stage, f.binding.invocation.ID)
	if err != nil || !admitted {
		t.Fatalf("admitted %t, %v", admitted, err)
	}
	// The record the daemon wrote before the run carried a base.
	f.record(t, admission)
	legacy := getRun(t, f.store, f.run.ID)
	if legacy.Base != nil {
		t.Fatalf("legacy run has base %+v", *legacy.Base)
	}

	tip = testTip("2222222222222222222222222222222222222222")
	feedbackID := operatorFeedbackInvocationID("command-1")
	f.engine.productionPublication = &productionPublicationWorkflow{remediationPromptPackage: agentTestDigest("8")}
	stage := domain.Stage{ID: operatorFeedbackStageID(feedbackID), RunID: legacy.ID, Name: productionStageName}
	next, base, admitted, err := f.engine.admitAttempt(t.Context(), writerBinding(t, legacy, feedbackID), stage, feedbackID)
	if err != nil || !admitted {
		t.Fatalf("admitted %t, %v", admitted, err)
	}
	if want := testTip("1111111111111111111111111111111111111111"); base != want || next.Base != want {
		t.Fatalf("legacy run admitted at %+v with base %+v, want its earliest admission's %+v", next.Base, base, want)
	}

	// recordAttempt needs the stage on the run.
	legacy.Stages = append(legacy.Stages, stage)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutRun(t.Context(), legacy) }); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.engine.recordAttempt(
		t.Context(), legacy.ID, stage.ID, feedbackID, nil, "pending", &next, base,
	); err != nil {
		t.Fatal(err)
	}
	if got := getRun(t, f.store, legacy.ID).Base; got == nil || *got != base {
		t.Fatalf("legacy run base after its next admission = %+v, want %+v", got, base)
	}

	unadmitted := seedWriterRun(t, f, "run-unadmitted", nil)
	id := productionInvocationID(unadmitted.ID)
	if _, base, _, err := f.engine.admitAttempt(
		t.Context(), writerBinding(t, unadmitted, id), unadmitted.Stages[0], id,
	); err != nil || base != tip {
		t.Fatalf("unadmitted run base = %+v, %v; want the tip %+v", base, err, tip)
	}
}

// A first admission that finds the run recorded another base since it read
// the run is refused and holds; the next pass admits at the recorded base.
func TestAFirstAdmissionThatLostTheRaceHolds(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	tip := testTip("2222222222222222222222222222222222222222")
	deriveTip(f.engine, &tip)
	run := seedWriterRun(t, f, "run-race", nil)
	id := productionInvocationID(run.ID)
	admission, base, _, err := f.engine.admitAttempt(t.Context(), writerBinding(t, run, id), run.Stages[0], id)
	if err != nil {
		t.Fatal(err)
	}
	won := testTip("1111111111111111111111111111111111111111")
	raced := run
	raced.Base = &won
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutRun(t.Context(), raced) }); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = f.engine.recordAttempt(t.Context(), run.ID, run.Stages[0].ID, id, nil, "pending", &admission, base)
	if !errors.Is(err, errRunBaseRecorded) || !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("recordAttempt = %v, want errRunBaseRecorded joined with ErrInputUnavailable", err)
	}
	if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldInputUnavailable || !invocationDispatchHold(err) {
		t.Fatalf("refusal holds as %q, %t; want the one invocation held as %q", reason, ok, domain.HoldInputUnavailable)
	}
	if stored := getRun(t, f.store, run.ID); attemptRecorded(stored, id) {
		t.Fatal("the refused admission appended an attempt")
	}

	run = getRun(t, f.store, run.ID)
	admission, base, _, err = f.engine.admitAttempt(t.Context(), writerBinding(t, run, id), run.Stages[0], id)
	if err != nil || base != won || admission.Base != won {
		t.Fatalf("next pass admitted at %+v with base %+v, %v; want %+v", admission.Base, base, err, won)
	}
	if _, _, _, err := f.engine.recordAttempt(
		t.Context(), run.ID, run.Stages[0].ID, id, nil, "pending", &admission, base,
	); err != nil {
		t.Fatal(err)
	}
}

// A run is admitted only under this composition's repository and ref, so a
// run recorded under another is refused rather than seeded from it.
func TestARunRecordedUnderAnotherRepositoryIsRefused(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	tip := testTip("2222222222222222222222222222222222222222")
	deriveTip(f.engine, &tip)
	other := testTip("1111111111111111111111111111111111111111")
	other.Repo, other.RepositoryID = "owner/other", 2
	run := seedWriterRun(t, f, "run-other", &other)
	id := productionInvocationID(run.ID)
	if _, _, admitted, err := f.engine.admitAttempt(
		t.Context(), writerBinding(t, run, id), run.Stages[0], id,
	); admitted || !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("admitted %t, %v; want ErrRepositoryIdentityMismatch", admitted, err)
	}
}

// A run held before its first admission reads the current tip on every
// pass, so image selection and the held run converge on the same commit; a
// pass with no stored tip holds as input_unavailable and starts nothing. Once
// admitted the run keeps its base, and a run first admitted after the tip
// moves records the newer one in the same process.
func TestAHeldRunFollowsTheTipUntilItsFirstAdmission(t *testing.T) {
	t.Parallel()
	h := newSelectionHoldFixture(t)
	var tip domain.BaseRevision
	tipErr := exec.ErrInputUnavailable
	h.engine.derive = func(_ context.Context, id domain.InvocationID) (string, domain.BaseRevision, error) {
		return "workspace-" + string(id), tip, tipErr
	}
	var selectedFor []domain.BaseRevision
	h.engine.selectImage = func(
		_ context.Context, _ domain.InvocationID, base domain.BaseRevision, _ *domain.ImageUpkeepRefusal,
	) (domain.ImageRef, error) {
		selectedFor = append(selectedFor, base)
		return h.selected, h.selectErr
	}

	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d with no stored tip", started)
	}
	if hold, found := h.hold(t); !found || hold.Reason != domain.HoldInputUnavailable {
		t.Fatalf("hold = %+v, found %t; want input_unavailable", hold, found)
	}

	tipErr = nil
	h.selectErr = &ImageUpkeepPendingError{Hold: domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}}
	first, second := testTip("1111111111111111111111111111111111111111"), testTip("2222222222222222222222222222222222222222")
	for _, next := range []domain.BaseRevision{first, second} {
		tip = next
		if started := h.dispatch(t); started != 0 {
			t.Fatalf("started %d while upkeep was pending", started)
		}
	}
	if len(selectedFor) != 2 || selectedFor[0] != first || selectedFor[1] != second {
		t.Fatalf("held passes selected for %+v, want %+v then %+v", selectedFor, first, second)
	}
	if run := getRun(t, h.fixture.store, "specification-run"); run.Base != nil {
		t.Fatalf("held run recorded base %+v", *run.Base)
	}

	h.selectErr, h.selected = nil, selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d once an image served the tip, want 1", started)
	}
	admission, err := h.admission(t)
	if err != nil || admission.Base != second {
		t.Fatalf("admission base = %+v, %v; want %+v", admission.Base, err, second)
	}
	if run := getRun(t, h.fixture.store, "specification-run"); run.Base == nil || *run.Base != second {
		t.Fatalf("specification run base = %+v, want %+v", run.Base, second)
	}

	// The specification is accepted and submits the implementation run,
	// which the attended engine admits only when asked.
	tip = testTip("3333333333333333333333333333333333333333")
	if result, err := h.engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	implementationID := productionInvocationID("implementation-run")
	var entry store.QueueEntry
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(implementationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeProductionRequest(entry)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := h.engine.loadProductionBinding(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findProductionStage(binding.run)
	if !ok {
		t.Fatal("implementation run has no production stage")
	}
	implementation, base, admitted, err := h.engine.admitAttempt(t.Context(), binding, stage, implementationID)
	if err != nil || !admitted {
		t.Fatalf("admit implementation: admitted %t, %v", admitted, err)
	}
	if _, _, _, err := h.engine.recordAttempt(
		t.Context(), binding.run.ID, stage.ID, implementationID, binding.purpose, entry.Status, &implementation, base,
	); err != nil {
		t.Fatal(err)
	}
	if run := getRun(t, h.fixture.store, "implementation-run"); run.Base == nil || *run.Base != tip {
		t.Fatalf("implementation run base = %+v, want the moved tip %+v", run.Base, tip)
	}
	if run := getRun(t, h.fixture.store, "specification-run"); run.Base == nil || *run.Base != second {
		t.Fatalf("specification run base moved to %+v, want %+v", run.Base, second)
	}
}

// A retry attempt dispatched after the tip moved starts at the run's base,
// the commit its failed first attempt ran at.
func TestARetryAttemptStartsAtTheRunBase(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 3)
	driver := f.newDriver(t)
	e := f.newEngine(t, driver)
	tip := testTip("1111111111111111111111111111111111111111")
	deriveTip(e, &tip)
	f.submit(t)
	failSpecificationIteration(t, f, driver, e, 1)
	recorded := tip

	tip = testTip("2222222222222222222222222222222222222222")
	if _, err := f.retry(t, retryReason); err != nil {
		t.Fatal(err)
	}
	scriptSpecificationOutput(t, driver, 2, "# Specification\n\nImplement after the retry.")
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	var admissions []domain.ExecutionAdmission
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admissions, err = tx.ListRunExecutionAdmissionRecords(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 2 {
		t.Fatalf("admissions = %d, want the first attempt and its retry", len(admissions))
	}
	for _, admission := range admissions {
		if admission.Base != recorded {
			t.Fatalf("admission %s at %+v, want the run base %+v", admission.InvocationID, admission.Base, recorded)
		}
	}
}
