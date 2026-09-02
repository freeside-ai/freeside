package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type blockedImplementationFixture struct {
	specificationFixture
	engine     *Engine
	driver     *execfake.StageDriver
	run        domain.Run
	attempt    domain.Attempt
	blocked    domain.BlockedOutcome
	blockedRaw []byte
}

// newBlockedImplementationFixture drives an auto-approved specification into
// an admitted, started implementation attempt whose fake driver ends blocked
// with the given summary, and persists the blocked outcome the way the stage
// driver does: the blob in the artifact store and one freeside.blocked claim
// on the invocation.
func newBlockedImplementationFixture(t *testing.T, kind domain.BlockedKind, summary string) blockedImplementationFixture {
	t.Helper()
	return newBlockedImplementationFixtureWith(t, kind, summary, decisionsFixture())
}

// newBlockedImplementationFixtureWithDecisions builds the fixture over an
// edited decision list, with the terminal summary the driver would derive.
func newBlockedImplementationFixtureWithDecisions(
	t *testing.T, kind domain.BlockedKind, edit func([]domain.Decision),
) blockedImplementationFixture {
	t.Helper()
	decisions := decisionsFixture()
	edit(decisions)
	return newBlockedImplementationFixtureWith(t, kind, exec.TruncateSummary(decisions[0].Question), decisions)
}

func newBlockedImplementationFixtureWith(
	t *testing.T, kind domain.BlockedKind, summary string, decisions []domain.Decision,
) blockedImplementationFixture {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	blocked := domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: kind, Decisions: decisions,
	}
	body, err := domain.EncodeBlockedOutcome(blocked)
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Sum(body))
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0,
		specify.Output{Specification: &specify.Specification{
			Summary: "The implementation plan is ready.", Body: "# Specification\n\nImplement the bounded workflow.",
			Addressals: []specify.Addressal{},
		}}); err != nil {
		t.Fatal(err)
	}
	implementationID := productionInvocationID("implementation-run")
	driver.Script(implementationID, execfake.StageScript{
		Outcome: execfake.OutcomeBlocked,
		Result:  exec.StageResult{Artifacts: []domain.Digest{digest}, Summary: summary},
	})
	f.submit(t)
	engine := f.newEngine(t, driver)
	// Pass one accepts the auto-approved specification and submits the
	// implementation run, which creates the invocation row the claim set
	// references.
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	if _, err := f.blobs.Put(digest, strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	claim := domain.AgentClaim{
		Label: export.BlockedEvidenceLabel, Artifact: domain.ArtifactID("blocked-" + implementationID),
		Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: implementationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: f.now.UTC(), Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	artifactMetadata := claim.Metadata
	artifactMetadata.Source = domain.EvidenceSourceRun
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: claim.Digest,
		Provenance: claim.Provenance, Metadata: artifactMetadata,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), artifact); err != nil {
			return err
		}
		return tx.PutAgentClaims(t.Context(), implementationID, []domain.AgentClaim{claim})
	}); err != nil {
		t.Fatal(err)
	}
	// An attended engine holds production dispatch, so admit, record, and
	// start the attempt the way dispatchIntent does; the caller's next pass
	// collects the blocked terminal.
	var entry store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
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
	binding, err := engine.loadProductionBinding(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findProductionStage(binding.run)
	if !ok {
		t.Fatal("approved implementation has no production stage")
	}
	admission, admitted, err := engine.admitAttempt(t.Context(), binding, stage, implementationID)
	if err != nil || !admitted {
		t.Fatalf("admit implementation = admitted %t, %v", admitted, err)
	}
	_, effective, bound, err := engine.recordAttempt(
		t.Context(), binding.run.ID, stage.ID, implementationID, entry.Status, &admission,
	)
	if err != nil || !bound {
		t.Fatalf("record implementation attempt = bound %t, %v", bound, err)
	}
	if err := driver.Start(t.Context(), implementationID, exec.StartSpecFromAdmission(effective)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.MarkOutboxDispatched(t.Context(), string(implementationID)); err != nil {
			return err
		}
		invocation := implementationID
		return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: binding.run.ID, Kind: domain.MilestoneInvocationStarted,
			InvocationID: &invocation, RecordedAt: f.now.UTC(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	run, err := f.run("implementation-run")
	if err != nil {
		t.Fatal(err)
	}
	stage, ok = findProductionStage(run)
	if !ok {
		t.Fatal("started implementation has no production stage")
	}
	if len(stage.Attempts) != 1 {
		t.Fatalf("implementation run stages = %+v", run.Stages)
	}
	return blockedImplementationFixture{
		specificationFixture: f, engine: engine, driver: driver, run: run,
		attempt: stage.Attempts[0], blocked: blocked, blockedRaw: body,
	}
}

func (f blockedImplementationFixture) questionID() domain.ItemID {
	return productionQuestionItemID(f.attempt.InvocationID)
}

func TestBlockedImplementationTerminalCreatesQuestionWithoutPublication(t *testing.T) {
	for _, kind := range domain.AllBlockedKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := newBlockedImplementationFixture(t, kind, decisionsFixture()[0].Question)
			if _, err := f.engine.Reconcile(t.Context()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			f.assertQuestion(t)

			// A replayed pass, including one after a restart, converges without
			// a second item, a changed one, or an implementation restart.
			_, before := f.item(t, f.questionID())
			if _, err := f.engine.Reconcile(t.Context()); err != nil {
				t.Fatalf("replayed reconcile: %v", err)
			}
			reopened := f.reopen(t)
			if _, err := reopened.newEngine(t, reopened.newDriver(t)).Reconcile(t.Context()); err != nil {
				t.Fatalf("restarted reconcile: %v", err)
			}
			f.specificationFixture = reopened
			if _, after := f.item(t, f.questionID()); after.EntityVersion != before.EntityVersion {
				t.Fatalf("replay advanced the question item to version %d", after.EntityVersion)
			}
		})
	}
}

// TestBlockedImplementationTerminalWithDriverOutcomeIsStillCollected: the
// real stage driver records the blocked outcome before the engine collects,
// and that record must not suppress the question.
// TestBlockedImplementationTerminalBoundsItsSummary: a long leading question
// reaches the item facts in full while the terminal carries the bounded
// summary every stage result is held to.
func TestBlockedImplementationTerminalBoundsItsSummary(t *testing.T) {
	long := strings.Repeat("Which retention period applies to exported logs? ", 20)
	f := newBlockedImplementationFixtureWithDecisions(t, domain.BlockedKindOwnerDecision, func(d []domain.Decision) {
		d[0].Question = strings.TrimSpace(long)
	})
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertQuestion(t)
	item, _ := f.item(t, f.questionID())
	if item.Reason != strings.TrimSpace(long) || len(item.Reason) <= exec.MaxSummaryBytes {
		t.Fatalf("item reason = %d bytes, want the full question", len(item.Reason))
	}
}

func TestBlockedImplementationTerminalWithDriverOutcomeIsStillCollected(t *testing.T) {
	f := newBlockedImplementationFixture(t, domain.BlockedKindScopeExpansion, decisionsFixture()[0].Question)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(t.Context(), f.attempt.InvocationID)
		if err != nil {
			return err
		}
		return tx.RecordExecutionOutcome(t.Context(), domain.ExecutionOutcome{
			InvocationID: f.attempt.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeBlocked, Summary: f.blocked.Decisions[0].Question,
			RecordedAt: f.now.Add(time.Minute).UTC(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertQuestion(t)
}

// TestBlockedImplementationTerminalRejectsMismatchedOutcome: a terminal whose
// summary disagrees with the persisted decisions is refused, and no item or
// publication task appears for it.
func TestBlockedImplementationTerminalRejectsMismatchedOutcome(t *testing.T) {
	f := newBlockedImplementationFixture(t, domain.BlockedKindOwnerDecision, "a different question")
	if _, err := f.engine.Reconcile(t.Context()); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("reconcile with a mismatched summary = %v, want ErrParentKeyMismatch", err)
	}
	if _, err := f.signet.GetAttentionItem(t.Context(), f.questionID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("question item after a rejected terminal = %v, want ErrNotFound", err)
	}
}

func (f blockedImplementationFixture) assertQuestion(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	item, _ := f.item(t, f.questionID())
	facts := item.AgentQuestion
	if item.Type != domain.AttentionAgentQuestion || item.Subject.RunID == nil || *item.Subject.RunID != f.run.ID ||
		item.InterruptionClass != domain.InterruptionExceptional || item.Priority != domain.PriorityNormal ||
		item.Status != domain.StatusOpen ||
		!reflect.DeepEqual(item.RequestedDecision, []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop}) ||
		facts == nil || facts.Stage != domain.StageNameImplementation || facts.InvocationID != f.attempt.InvocationID ||
		facts.Kind == nil || *facts.Kind != f.blocked.Kind || !reflect.DeepEqual(facts.Decisions, f.blocked.Decisions) ||
		len(item.AgentClaims) != 1 || item.AgentClaims[0].Label != domain.AgentQuestionClaimLabel ||
		item.AgentClaims[0].Digest != domain.Digest(contentaddr.Sum(f.blockedRaw)) ||
		item.AgentClaims[0].Provenance.ProducerInvocationID != f.attempt.InvocationID {
		t.Fatalf("question item = %#v", item)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(ctx, domain.ItemID("execution-failure-"+string(f.attempt.InvocationID))); !errors.Is(err, store.ErrNotFound) {
			return errors.New("blocked terminal created an execution_failure item")
		}
		if _, err := tx.GetOutbox(ctx, productionPublicationTaskKey(f.run.ID)); !errors.Is(err, store.ErrNotFound) {
			return errors.New("blocked terminal enqueued a publication task")
		}
		if _, err := tx.GetExecutionExportRecord(ctx, f.attempt.InvocationID); !errors.Is(err, store.ErrNotFound) {
			return errors.New("blocked terminal recorded an execution export")
		}
		outcome, err := tx.GetExecutionOutcomeRecord(ctx, f.attempt.InvocationID)
		if err != nil {
			return err
		}
		if outcome.Status != domain.ExecutionOutcomeBlocked ||
			outcome.Summary != exec.TruncateSummary(f.blocked.Decisions[0].Question) {
			return errors.New("blocked outcome record disagrees with the terminal")
		}
		entry, err := tx.GetInbox(ctx, string(f.attempt.InvocationID))
		if err != nil {
			return err
		}
		var terminal productionTerminalRecord
		if err := json.Unmarshal(entry.Payload, &terminal); err != nil {
			return err
		}
		if terminal.Status != exec.StatusBlocked || terminal.HeadSHA != "" {
			return errors.New("recorded terminal is not a blocked terminal without a head")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBlockedImplementationAnswerRetriesImplementer: answer_and_retry routed
// to retry_implementation re-invokes the implementer on the same run with the
// answer as operator feedback, against the unchanged specification digest.
func TestBlockedImplementationAnswerRetriesImplementer(t *testing.T) {
	f := newBlockedImplementationFixture(t, domain.BlockedKindOwnerDecision, decisionsFixture()[0].Question)
	f.engine.productionPublication = &productionPublicationWorkflow{
		store: f.store, attention: f.signet, artifacts: f.blobs,
		now: func() time.Time { return *f.now },
	}
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertQuestion(t)
	specDigestBefore := f.run.SpecDigest

	snapshot, err := f.signet.GetAttentionItem(t.Context(), f.questionID())
	if err != nil {
		t.Fatal(err)
	}
	route := domain.AnswerRouteRetryImplementation
	const answer = "Current and previous: keep both adapters."
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "answer-implementation", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: f.questionID(), Action: domain.ActionAnswerAndRetry,
			ItemVersion: snapshot.Item.ItemVersion, ArtifactDigests: snapshot.Item.ArtifactDigests,
			Message: answer, AnswerRoute: &route,
		},
	}); err != nil {
		t.Fatal(err)
	}
	created, err := f.engine.reconcileOperatorFeedback(t.Context())
	if err != nil || created != 1 {
		t.Fatalf("reconcileOperatorFeedback = %d, %v", created, err)
	}
	feedbackID := operatorFeedbackInvocationID("answer-implementation")
	var (
		entry   store.QueueEntry
		run     domain.Run
		pending []store.QueueEntry
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		if entry, err = tx.GetOutbox(t.Context(), string(feedbackID)); err != nil {
			return err
		}
		if run, err = tx.GetRun(t.Context(), f.run.ID); err != nil {
			return err
		}
		pending, err = tx.ListPendingOutbox(t.Context(), KindSpecificationInvocationRequested)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeOperatorFeedbackRequest(entry)
	if err != nil || request.RunID != f.run.ID || request.SourceInvocationID != f.attempt.InvocationID ||
		request.CommandID != "answer-implementation" || request.HeadSHA != "" {
		t.Fatalf("operator feedback request = %#v, %v", request, err)
	}
	body, err := loadFakePublicationBlob(f.blobs, request.InputArtifactDigest)
	if err != nil {
		t.Fatal(err)
	}
	var input operatorFeedbackInput
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Feedback != answer || !reflect.DeepEqual(input.Question, snapshot.Item.AgentQuestion) {
		t.Fatalf("implementation answer input = %#v, want answer and authenticated question", input)
	}
	if run.SpecDigest != specDigestBefore {
		t.Fatalf("retry changed the specification digest to %s", run.SpecDigest)
	}
	if len(pending) != 0 {
		t.Fatalf("retry_implementation enqueued %d specification requests", len(pending))
	}
	if replay, err := f.engine.reconcileOperatorFeedback(t.Context()); err != nil || replay != 0 {
		t.Fatalf("replayed feedback reconciliation = %d, %v", replay, err)
	}
}
