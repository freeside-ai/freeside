package main

import (
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// admittedPublication returns the publication bytes the occurrence's first
// production attempt froze and the publication its started specification
// invocation carries.
func (f intakeFixture) admittedPublication(
	t *testing.T, o domain.IntakeOccurrence,
) ([]byte, engine.ProductionPublication, engine.ProductionPublication) {
	t.Helper()
	campaignID, err := engine.ProductionCampaignIDForImplementation(intakeImplementationRunID(o))
	if err != nil {
		t.Fatal(err)
	}
	var attempt domain.ProductionAttempt
	var invocation store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		if attempt, err = tx.GetProductionAttempt(t.Context(), campaignID, 1); err != nil {
			return err
		}
		invocation, err = tx.GetOutbox(t.Context(),
			string(domain.SpecificationInvocationID(o.Admission.Subject.SpecificationRunID, 1)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var frozen engine.ProductionPublication
	if err := json.Unmarshal(attempt.Publication, &frozen); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Publication engine.ProductionPublication `json:"publication"`
	}
	if err := json.Unmarshal(invocation.Payload, &request); err != nil {
		t.Fatal(err)
	}
	return attempt.Publication, frozen, request.Publication
}

// TestIntakeFreezesIntakeRecipeOnFirstAdmission proves a new label-initiated run
// freezes freeside.intake-publication/v1 beside its literal title and body, and
// the started specification carries that record.
func TestIntakeFreezesIntakeRecipeOnFirstAdmission(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 1)
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil || !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("occurrence was not admitted and started")
	}
	_, frozen, started := f.admittedPublication(t, o)
	literal := intakeLiteralPublication(init, o)
	if frozen.Recipe != engine.IntakePublicationRecipe || frozen.Title != literal.Title ||
		frozen.Body != literal.Body || frozen.SourceIssue != "" {
		t.Fatalf("frozen publication = %+v, want the intake recipe over the literal text", frozen)
	}
	if started != frozen {
		t.Fatalf("started publication = %+v, want the frozen record %+v", started, frozen)
	}
}

// TestIntakePreChangeOccurrenceReplaysLiteralRecord proves an occurrence whose
// first attempt was frozen before the intake recipe existed keeps its literal
// record. The seed is the one write the recipe changes: the attempt a
// pre-change admission stored before crashing ahead of BindIntakeAdmission. The
// re-run admission must converge on those bytes instead of failing the
// attempt's immutability check, and the start must submit the same record.
func TestIntakePreChangeOccurrenceReplaysLiteralRecord(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 1)
	var occurrence domain.IntakeOccurrence
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		occurrence, _, err = tx.AllocateNextIntakeOccurrence(
			t.Context(), init.Repo, init.RepositoryID, 7, init.Label, f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	implementationRunID := intakeImplementationRunID(occurrence)
	specificationRunID, err := engine.ResolveSpecificationRunID(t.Context(), f.store, implementationRunID)
	if err != nil {
		t.Fatal(err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		t.Fatal(err)
	}
	literal, err := json.Marshal(intakeLiteralPublication(init, occurrence))
	if err != nil {
		t.Fatal(err)
	}
	workItem := submissionBytes(intakeWorkItemDocument(occurrence))
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(t.Context(), domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: workItem.digest, PublicationDigest: submissionBytes(literal).digest, Publication: literal,
			SpecificationRunID: specificationRunID, ImplementationRunID: implementationRunID,
		})
	}); err != nil {
		t.Fatal(err)
	}

	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil || !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("pre-change occurrence was not admitted and started")
	}
	stored, frozen, started := f.admittedPublication(t, o)
	if string(stored) != string(literal) || frozen.Recipe != "" {
		t.Fatalf("stored publication = %s, want the unchanged literal record", stored)
	}
	if started != frozen {
		t.Fatalf("started publication = %+v, want the literal record %+v", started, frozen)
	}
}
