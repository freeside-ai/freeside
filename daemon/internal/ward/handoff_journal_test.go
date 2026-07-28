package ward

import (
	"errors"
	"strings"
	"testing"
)

// journalled wires a fake journal into the fixture, mirroring its calls into
// the runtime call log.
func (fx *handoffFixture) journalled() *fakeJournal {
	j := newFakeJournal()
	j.rt = fx.rt
	fx.cfg.Journal = j
	return j
}

// TestHandoffJournalledSuccess pins the write points on the happy path:
// intent-before-create (Begin durable before the first runtime object), the
// two pre-writer proofs and writer-complete amended as they are earned, and
// the record closed completed only when the caller receives the export.
func TestHandoffJournalledSuccess(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	hs, _ := fx.leased(t)
	hs.Seed = fx.seed(t).Seed
	names := namesFor(hs.RunID)

	res, err := fx.runSpec(t, hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}

	begin := fx.rt.callIndex("journal-begin " + hs.RunID)
	firstObject := fx.rt.callIndex("create-volume " + names.Workspace)
	writerComplete := fx.rt.callIndex("journal-writer-complete " + hs.RunID)
	writerDelete := fx.rt.callIndex("delete-container " + names.Agent)
	switch {
	case begin == -1 || firstObject == -1:
		t.Fatalf("begin=%d createVolume=%d; both must happen", begin, firstObject)
	case begin > firstObject:
		t.Errorf("journal begin at %d, after the first runtime object at %d", begin, firstObject)
	case writerComplete == -1 || writerComplete < writerDelete:
		t.Errorf("writer-complete at %d, want after the writer delete at %d", writerComplete, writerDelete)
	}

	rec := j.snapshot(hs.RunID)
	if rec == nil {
		t.Fatal("no journal record")
	}
	if rec.OwnershipToken == "" || !ownershipTokenPattern.MatchString(rec.OwnershipToken) {
		t.Errorf("record ownership token %q is not the minted token shape", rec.OwnershipToken)
	}
	wantDigest, err := specDigest(hs)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SpecDigest != wantDigest {
		t.Error("record spec digest does not bind to the run's spec")
	}
	if rec.ObservedBaseSHA != res.Workspace.ObservedBaseSHA || rec.ObservedBaseSHA == "" {
		t.Errorf("record observed base %q, want the attested %q", rec.ObservedBaseSHA, res.Workspace.ObservedBaseSHA)
	}
	if rec.CredentialPreDigest != res.AuthStore.PreDigest || rec.CredentialPreDigest == "" {
		t.Errorf("record credential pre-digest %q, want the attested %q", rec.CredentialPreDigest, res.AuthStore.PreDigest)
	}
	if !rec.WriterComplete {
		t.Error("record does not carry writer-complete")
	}
	if rec.Lease == nil || rec.Lease.AuthIdentityID != "identity-fixture" || rec.Lease.Fence != 1 {
		t.Errorf("record lease = %+v, want the acquired lease reference", rec.Lease)
	}
	if rec.Outcome == nil || *rec.Outcome != HandoffCompleted {
		t.Errorf("record outcome = %v, want completed", rec.Outcome)
	}
	if rec.ExportDir != res.ExportDir || rec.ExportDir == "" {
		t.Errorf("record export dir %q, want the released %q", rec.ExportDir, res.ExportDir)
	}
	fx.assertReaped(t)
}

// TestHandoffJournalledFailureClosesLoss: a failed run whose teardown proved
// every object absent commits loss — the durable rerun-safe signal.
func TestHandoffJournalledFailureClosesLoss(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	fx.rt.onStart = func(id string) error {
		if id == names.Agent {
			return errors.New("fixture: start refused")
		}
		return nil
	}

	_, err := fx.runSpec(t, hs)
	if err == nil {
		t.Fatal("Handoff succeeded; fixture meant to fail the writer start")
	}
	rec := j.snapshot(hs.RunID)
	if rec == nil || rec.Outcome == nil || *rec.Outcome != HandoffLoss {
		t.Fatalf("record = %+v, want closed as loss", rec)
	}
	if rec.WriterComplete {
		t.Error("record carries writer-complete for a writer that never ran")
	}
	fx.assertReaped(t)
}

// TestHandoffJournalledTeardownFailureLeavesOpen: when teardown cannot prove
// absence, the record stays open — loss is a proven claim, not a default,
// and recovery owns the record from here.
func TestHandoffJournalledTeardownFailureLeavesOpen(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	started := false
	fx.rt.onStart = func(id string) error {
		if id == names.Agent {
			started = true
		}
		return nil
	}
	fx.rt.onStop = func(id string) error {
		if id == names.Agent {
			return errors.New("fixture: stop refused")
		}
		return nil
	}
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == names.Agent && started {
			rep.State = StateRunning
		}
		return rep, nil
	}
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		if id == names.Agent {
			return true, nil // lie: report success, keep the survivor
		}
		return false, nil
	}

	_, err := fx.runSpec(t, hs)
	if err == nil {
		t.Fatal("Handoff succeeded; fixture meant to fail teardown")
	}
	rec := j.snapshot(hs.RunID)
	if rec == nil || rec.Outcome != nil {
		t.Fatalf("record = %+v, want open (absence unproven)", rec)
	}
}

// TestHandoffJournalBeginFailureRefusesRun: a run that cannot be journalled
// is refused before any runtime object exists.
func TestHandoffJournalBeginFailureRefusesRun(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	inject := errors.New("fixture: journal unavailable")
	j.failBegin = inject
	names := namesFor(testHandoffSpec().RunID)

	_, err := fx.run(t)
	if !errors.Is(err, inject) {
		t.Fatalf("Handoff error = %v, want the journal failure", err)
	}
	if fx.rt.callIndex("create-volume "+names.Workspace) != -1 {
		t.Error("workspace volume was created despite the journal refusal")
	}
}

// TestHandoffJournalMarkFailureFailsRun: an amendment that could not be made
// durable fails the run — an unpersisted proof is not a proof.
func TestHandoffJournalMarkFailureFailsRun(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	inject := errors.New("fixture: journal write failed")
	j.failMark = inject
	names := namesFor(testHandoffSpec().RunID)

	_, err := fx.run(t)
	if !errors.Is(err, inject) {
		t.Fatalf("Handoff error = %v, want the journal failure", err)
	}
	// The blank-seed, non-leased fixture's first mark is writer-complete, so
	// the exporter must never have run.
	if fx.rt.callIndex("create-container "+names.Exporter) != -1 {
		t.Error("exporter ran despite the failed writer-complete mark")
	}
	fx.assertReaped(t)
}

// TestHandoffJournalCloseFailureVoidsDelivery: release follows the durable
// append; a completed-close that could not be written voids the delivery and
// removes the output.
func TestHandoffJournalCloseFailureVoidsDelivery(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	inject := errors.New("fixture: journal close failed")
	j.failClose = inject

	res, err := fx.run(t)
	if !errors.Is(err, inject) {
		t.Fatalf("Handoff error = %v, want the journal failure", err)
	}
	if res != nil {
		t.Fatal("close failure still delivered a result")
	}
	for _, d := range scratchDirs(t, testHandoffSpec().RunID) {
		t.Errorf("leftover handoff temp dir after voided delivery: %s", d)
	}
	fx.assertReaped(t)
}

// TestHandoffJournalledBlankSeedNonLeasedMarks: a blank-seed, non-leased run
// records neither pre-writer proof — the record reflects what was earned,
// not the schema's ambitions.
func TestHandoffJournalledBlankSeedNonLeasedMarks(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()

	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	hs := testHandoffSpec()
	rec := j.snapshot(hs.RunID)
	if rec == nil {
		t.Fatal("no journal record")
	}
	if rec.ObservedBaseSHA != "" || rec.CredentialPreDigest != "" || rec.Lease != nil {
		t.Errorf("record = %+v, want no seed, credential, or lease marks", rec)
	}
	if !rec.WriterComplete || rec.Outcome == nil || *rec.Outcome != HandoffCompleted {
		t.Errorf("record = %+v, want writer-complete and completed", rec)
	}
	for _, c := range j.calls {
		if strings.HasPrefix(c, "journal-seed-observed") || strings.HasPrefix(c, "journal-cred-observed") {
			t.Errorf("unexpected journal call %q on a blank-seed non-leased run", c)
		}
	}
}

// TestHandoffJournallessUnchanged: a nil journal preserves the one-shot
// semantics byte for byte — no journal calls anywhere in the run.
func TestHandoffJournallessUnchanged(t *testing.T) {
	fx := newHandoffFixture(t)
	if _, err := fx.run(t); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	for _, c := range fx.rt.calls {
		if strings.HasPrefix(c, "journal-") {
			t.Errorf("journalless run recorded journal call %q", c)
		}
	}
	fx.assertReaped(t)
}
