package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/intake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	intakeTestRepo   = "freeasinbird/freeside"
	intakeTestRepoID = int64(84958515)
	intakeTestLabel  = "freeside"
	intakeTestProj   = domain.ProjectID("project-label-intake")
)

type intakeFixture struct {
	store     *store.Store
	blobs     *signet.BlobStore
	attention *signet.Service
	engine    *engine.Engine
	now       time.Time
}

func newIntakeFixture(t *testing.T) intakeFixture {
	t.Helper()
	return openIntakeFixture(t, t.TempDir())
}

// openIntakeFixture opens (or reopens) a fixture over the given directory, so a
// test can simulate a daemon restart against the same durable store and blobs.
func openIntakeFixture(t *testing.T, root string) intakeFixture {
	t.Helper()
	st := storetest.Open(t, root+"/state.db", store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	attention := signet.NewService(st, signet.WithBlobStore(blobs),
		signet.WithClock(func() time.Time { return now }))
	driver, err := execfake.NewStageDriverAt(root + "/driver")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := engine.New(st, attention, driver)
	if err != nil {
		t.Fatal(err)
	}
	return intakeFixture{store: st, blobs: blobs, attention: attention, engine: workflow, now: now}
}

// reconciler builds a loop over the fixture with a fixed label observation and a
// per-issue state lookup, and a single initiator carrying the given mode.
func (f intakeFixture) reconciler(
	initiators []intakeInitiator, labeled []publish.LabelIssue, issueState map[int]string,
) *intakeReconciler {
	return &intakeReconciler{
		store: f.store, blobs: f.blobs, engine: f.engine, attention: f.attention,
		observeLabel: func(_ context.Context, _, _ string) (publish.LabelIssuesObservation, error) {
			return publish.LabelIssuesObservation{Issues: labeled, RepositoryID: intakeTestRepoID}, nil
		},
		observeIssue: func(_ context.Context, _ string, number int) (publish.IssueObservation, error) {
			state := issueState[number]
			if state == "" {
				state = "open"
			}
			return publish.IssueObservation{Number: number, State: state}, nil
		},
		initiators: initiators,
		now:        func() time.Time { return f.now },
	}
}

func intakePolicyKeys(t *testing.T, mode domain.InitiatorMode, modeSource domain.ProvenanceSource, wipCap int) []domain.PolicyKey {
	t.Helper()
	prov := func(source domain.ProvenanceSource) domain.KeyProvenance {
		return domain.KeyProvenance{Source: source, Digest: domain.Digest(contentaddr.Sum([]byte("intake-test-policy")))}
	}
	return []domain.PolicyKey{
		{Key: intake.PolicyRunWIPCap, Value: strconv.Itoa(wipCap), Provenance: prov(domain.ProvenancePreset)},
		{Key: intake.PolicyInitiatorMode, Value: string(mode), Provenance: prov(modeSource)},
		{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov(domain.ProvenancePreset)},
		{Key: specify.PolicyMaxIterations, Value: "4", Provenance: prov(domain.ProvenancePreset)},
		{Key: specify.PolicyStageActiveTime, Value: "45m", Provenance: prov(domain.ProvenancePreset)},
		{Key: specify.PolicyApprovalWait, Value: "4h", Provenance: prov(domain.ProvenancePreset)},
		{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example,https://api.github.com", Provenance: prov(domain.ProvenancePreset)},
		{Key: specify.PolicyResearchMaxBytes, Value: "1048576", Provenance: prov(domain.ProvenancePreset)},
		{Key: "paths", Value: "src/,docs/", Provenance: prov(domain.ProvenancePreset)},
	}
}

func intakeInitiatorFor(t *testing.T, mode domain.InitiatorMode, modeSource domain.ProvenanceSource, wipCap int) intakeInitiator {
	t.Helper()
	return intakeInitiator{
		Repo: intakeTestRepo, RepositoryID: intakeTestRepoID, Label: intakeTestLabel,
		ProjectID:         intakeTestProj,
		PolicyKeys:        intakePolicyKeys(t, mode, modeSource, wipCap),
		CommitAuthor:      engine.ProductionCommitAuthor{AppSlug: "freeside-bot", BotUserID: 12345},
		ExpectedCostUnits: 100, ComponentCount: 1,
	}
}

func labeledOpen(numbers ...int) []publish.LabelIssue {
	issues := make([]publish.LabelIssue, 0, len(numbers))
	for _, n := range numbers {
		issues = append(issues, publish.LabelIssue{Number: n, State: "open", HasLabel: true})
	}
	return issues
}

// latestOccurrence reads the highest-ordinal occurrence for an issue.
func (f intakeFixture) latestOccurrence(t *testing.T, issue int) domain.IntakeOccurrence {
	t.Helper()
	var o domain.IntakeOccurrence
	var found bool
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		o, found, err = tx.LatestIntakeOccurrence(t.Context(), intakeTestRepoID, issue, intakeTestLabel)
		return err
	}); err != nil {
		t.Fatalf("read occurrence: %v", err)
	}
	if !found {
		t.Fatalf("no occurrence for issue %d", issue)
	}
	return o
}

func (f intakeFixture) started(t *testing.T, specificationRunID domain.RunID) bool {
	t.Helper()
	present, err := engine.HasSpecificationDispatchMarker(t.Context(), f.store, specificationRunID)
	if err != nil {
		t.Fatalf("inspect marker: %v", err)
	}
	return present
}

func (f intakeFixture) proposalItem(t *testing.T, o domain.IntakeOccurrence) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), domain.ItemID(o.Admission.ProposalInstanceID))
		return err
	}); err != nil {
		t.Fatalf("read proposal item: %v", err)
	}
	return item
}

// TestIntakeProposeCreatesOneProposalPerOccurrence covers acceptance #1 and #2:
// a labeled issue in propose mode yields exactly one open run_proposal, and
// repeated passes and a restart converge on the same admission (no duplicate
// proposal, no start).
func TestIntakeProposeCreatesOneProposalPerOccurrence(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

	for pass := 0; pass < 3; pass++ {
		r.reconcile(t.Context(), nil)
	}
	o := f.latestOccurrence(t, 7)
	if o.Ordinal != 1 {
		t.Fatalf("ordinal = %d, want 1 (re-observation must not allocate a new occurrence)", o.Ordinal)
	}
	if o.Admission == nil {
		t.Fatal("occurrence was not admitted")
	}
	item := f.proposalItem(t, o)
	if item.Type != domain.AttentionRunProposal || item.Status != domain.StatusOpen {
		t.Fatalf("proposal item = type %q status %q, want open run_proposal", item.Type, item.Status)
	}
	if f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("propose mode must not start specification")
	}
}

// TestIntakeKillRecoveryConvergesOnAdmissionKey covers acceptance #1's stated
// mechanism: a daemon restart mid-flight re-derives the same occurrence-keyed
// admission and run identities, so re-observing the same labeled issue after a
// restart converges on the one admitted proposal and the one started run rather
// than admitting or starting a second.
func TestIntakeKillRecoveryConvergesOnAdmissionKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)

	first := openIntakeFixture(t, root)
	first.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	before := first.latestOccurrence(t, 7)
	if before.Admission == nil || !first.started(t, before.Admission.Subject.SpecificationRunID) {
		t.Fatal("first run should have admitted and started")
	}
	if err := first.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Restart: a fresh store, engine, and reconciler over the same durable files.
	second := openIntakeFixture(t, root)
	second.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	after := second.latestOccurrence(t, 7)
	if after.Ordinal != before.Ordinal ||
		after.Admission == nil ||
		after.Admission.ProposalInstanceID != before.Admission.ProposalInstanceID ||
		after.Admission.Subject.SpecificationRunID != before.Admission.Subject.SpecificationRunID {
		t.Fatalf("restart diverged: before=%+v after=%+v", before.Admission, after.Admission)
	}
}

// TestIntakeFailsClosedOnReboundRepository proves the §5.18 rebinding guard: a
// label scan whose observed canonical repository id does not match the
// configured RepositoryID admits nothing, so intake never records authority
// under a name that now resolves to a different repository.
func TestIntakeFailsClosedOnReboundRepository(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
	r := &intakeReconciler{
		store: f.store, blobs: f.blobs, engine: f.engine, attention: f.attention,
		observeLabel: func(_ context.Context, _, _ string) (publish.LabelIssuesObservation, error) {
			// The name now resolves to a different repository id.
			return publish.LabelIssuesObservation{Issues: labeledOpen(7), RepositoryID: intakeTestRepoID + 1}, nil
		},
		observeIssue: func(_ context.Context, _ string, number int) (publish.IssueObservation, error) {
			return publish.IssueObservation{Number: number, State: "open"}, nil
		},
		initiators: []intakeInitiator{init}, now: func() time.Time { return f.now },
	}
	r.reconcile(t.Context(), nil)

	var found bool
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, found, _ = tx.LatestIntakeOccurrence(t.Context(), intakeTestRepoID, 7, intakeTestLabel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a rebound repository must not allocate an occurrence")
	}
}

// TestIntakeProposalDoesNotOfferStartWithChanges proves the label proposal omits
// start_with_changes (decision note Decision 4): the subject is fixed to the
// occurrence's issue, so offering a subject revision would strand the occurrence.
func TestIntakeProposalDoesNotOfferStartWithChanges(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)

	item := f.proposalItem(t, f.latestOccurrence(t, 7))
	for _, action := range item.RequestedDecision {
		if action == domain.ActionStartWithChanges {
			t.Fatalf("label proposal offers start_with_changes: %v", item.RequestedDecision)
		}
	}
	// It still offers the actions a label proposal supports.
	if !slices.Contains(item.RequestedDecision, domain.ActionStart) ||
		!slices.Contains(item.RequestedDecision, domain.ActionDecline) {
		t.Fatalf("label proposal offered set = %v, want start + decline", item.RequestedDecision)
	}
}

// TestIntakeAutoStartUnderCapStarts covers acceptance #2: an override-authorized
// auto_start below the WIP cap launches specification (the dispatch marker exists)
// and resolves the proposal card.
func TestIntakeAutoStartUnderCapStarts(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 1)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Refusal != nil {
		t.Fatalf("unexpected refusal %q", o.Refusal.Reason)
	}
	if !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("authorized auto_start under cap must start specification")
	}
	if item := f.proposalItem(t, o); item.Status != domain.StatusResolved {
		t.Fatalf("auto_start card status = %q, want resolved", item.Status)
	}
	// Idempotent: a second pass does not double-start or error.
	r.reconcile(t.Context(), nil)
}

// TestIntakeAutoStartPresetDowngrades covers acceptance #2's "explicit recorded
// preset override": a preset-sourced auto_start is not authorized, so it is
// recorded as a mode_not_authorized refusal and left an ordinary proposal.
func TestIntakeAutoStartPresetDowngrades(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenancePreset, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Refusal == nil || o.Refusal.Reason != domain.IntakeRefusalModeNotAuthorized {
		t.Fatalf("refusal = %+v, want mode_not_authorized", o.Refusal)
	}
	if f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("a downgraded auto_start must not start")
	}
	if item := f.proposalItem(t, o); item.Status != domain.StatusOpen {
		t.Fatalf("downgraded card status = %q, want open", item.Status)
	}
}

// TestIntakeAutoStartWIPCapRefusesBeyondAndSerializes covers acceptance #2's
// "refused beyond WIP caps" and the WIP-cap race: with a cap of one, the first
// occurrence starts (its run fills the slot), the second is refused
// wip_cap_exhausted and left an ordinary proposal.
func TestIntakeAutoStartWIPCapRefusesBeyondAndSerializes(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 1)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7, 8), nil)

	r.reconcile(t.Context(), nil)
	first := f.latestOccurrence(t, 7)
	second := f.latestOccurrence(t, 8)
	if !f.started(t, first.Admission.Subject.SpecificationRunID) {
		t.Fatal("first occurrence should have taken the single WIP slot")
	}
	if second.Refusal == nil || second.Refusal.Reason != domain.IntakeRefusalWIPCapExhausted {
		t.Fatalf("second refusal = %+v, want wip_cap_exhausted", second.Refusal)
	}
	if f.started(t, second.Admission.Subject.SpecificationRunID) {
		t.Fatal("second occurrence must not start beyond the WIP cap")
	}
	if item := f.proposalItem(t, second); item.Status != domain.StatusOpen {
		t.Fatalf("refused card status = %q, want open (left an ordinary proposal)", item.Status)
	}
}

// TestIntakeWorkItemCarriesNoIssueContent covers acceptance #4 / §5.13: the
// daemon-authored work-item document delivered in the specification role is a
// pure function of the occurrence coordinates and carries no observed issue
// content, even when the observation is hostile. The publication is likewise
// coordinate-derived.
func TestIntakeWorkItemCarriesNoIssueContent(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	// A hostile observation cannot inject content: LabelIssue carries only the
	// number, state, and label presence. There is no field for a title or body.
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	r.reconcile(t.Context(), nil)

	o := f.latestOccurrence(t, 7)
	doc := string(intakeWorkItemDocument(o))
	for _, forbidden := range []string{"IGNORE", "<script", "title:", "body:"} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("work-item document leaked non-coordinate content: contains %q", forbidden)
		}
	}
	if !strings.Contains(doc, "issue: 7") || !strings.Contains(doc, "initiator_label: freeside") {
		t.Fatalf("work-item document missing coordinates: %q", doc)
	}
	// The reserved run's spec digest is exactly the coordinate document's digest.
	var run domain.Run
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), o.Admission.Subject.SpecificationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if run.SpecDigest != domain.Digest(contentaddr.Sum(intakeWorkItemDocument(o))) {
		t.Fatal("reserved run spec digest is not the coordinate work-item digest")
	}
	wantCampaign, err := engine.ProductionCampaignIDForImplementation(intakeImplementationRunID(o))
	if err != nil {
		t.Fatal(err)
	}
	if run.CampaignID != wantCampaign || run.AttemptNumber != 1 {
		t.Fatalf("reserved run lineage = %q/%d, want %q/1", run.CampaignID, run.AttemptNumber, wantCampaign)
	}
}

// TestIntakeSupersedesDepartedProposal covers acceptance #3: an open admitted
// proposal whose issue leaves the labeled-open set is superseded — absent when
// the label was removed (issue still open), closed when the issue closed.
func TestIntakeSupersedesDepartedProposal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		state      string
		wantState  domain.IntakeOccurrenceState
		wantReason domain.IntakeSupersessionReason
	}{
		{"label removed", "open", domain.IntakeOccurrenceAbsent, domain.IntakeSupersededLabelRemoved},
		{"issue closed", "closed", domain.IntakeOccurrenceClosed, domain.IntakeSupersededIssueClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeFixture(t)
			init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
			// Pass 1: labeled-open, admit a propose card.
			present := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
			present.reconcile(t.Context(), nil)
			before := f.latestOccurrence(t, 7)
			if before.Admission == nil {
				t.Fatal("occurrence was not admitted")
			}
			// Pass 2: the issue is gone from the labeled-open set.
			departed := f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: tc.state})
			departed.reconcile(t.Context(), nil)
			after := f.latestOccurrence(t, 7)
			if after.State != tc.wantState {
				t.Fatalf("state = %q, want %q", after.State, tc.wantState)
			}
			if after.Supersession == nil || after.Supersession.Reason != tc.wantReason {
				t.Fatalf("supersession = %+v, want %q", after.Supersession, tc.wantReason)
			}
			if item := f.proposalItem(t, after); item.Status != domain.StatusSuperseded {
				t.Fatalf("card status = %q, want superseded", item.Status)
			}
		})
	}
}

// TestIntakeDoesNotSupersedeDecidedProposal covers acceptance #3's boundary: a
// proposal already decided (auto-started) before the departure is left
// untouched — no supersession withdraws a running run.
func TestIntakeDoesNotSupersedeDecidedProposal(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
	present := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	present.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("occurrence should have auto-started")
	}
	// Now the label is removed.
	departed := f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"})
	departed.reconcile(t.Context(), nil)
	after := f.latestOccurrence(t, 7)
	if after.Supersession != nil {
		t.Fatalf("a decided proposal must not be superseded, got %+v", after.Supersession)
	}
}

// TestIntakeLaunchesDepartedDecidedStart proves a start decided (by an operator)
// between the present pass and a departure still launches: retiring the
// occurrence must not strand the recorded start.
func TestIntakeLaunchesDepartedDecidedStart(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)

	// An operator decides start (records the decision, resolves the card); the
	// loop has not launched it yet.
	if _, err := f.attention.StartRunProposalUnattended(t.Context(),
		domain.ItemID(o.Admission.ProposalInstanceID), "operator-start-7"); err != nil {
		t.Fatal(err)
	}
	if f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("recording the decision must not itself launch")
	}

	// The issue departs (label removed) before the loop launched it.
	f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"}).reconcile(t.Context(), nil)
	after := f.latestOccurrence(t, 7)
	if !f.started(t, after.Admission.Subject.SpecificationRunID) {
		t.Fatal("a departed decided start must launch before the occurrence retires")
	}
	if after.State == domain.IntakeOccurrencePresent {
		t.Fatalf("occurrence should have retired, state = %q", after.State)
	}
}

// TestIntakeDefersDepartureUntilDecidedStartLaunches proves the atomic guard: a
// start decided but not yet launched (no dispatch marker) is not retired by a
// departure, so the recorded start is never stranded. advanceDeparture defers
// and leaves the occurrence present for a later pass to launch then retire.
func TestIntakeDefersDepartureUntilDecidedStartLaunches(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)

	// A start is decided (card resolved) but the run has not launched (no marker).
	if _, err := f.attention.StartRunProposalUnattended(t.Context(),
		domain.ItemID(o.Admission.ProposalInstanceID), "op-7"); err != nil {
		t.Fatal(err)
	}
	// advanceDeparture alone must DEFER, atomically, rather than retire and strand
	// the decided start.
	r := f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"})
	if err := r.advanceDeparture(t.Context(), o); !errors.Is(err, errIntakeDeferDepartureRetire) {
		t.Fatalf("advanceDeparture on a decided-but-unstarted proposal: err = %v, want defer", err)
	}
	if after := f.latestOccurrence(t, 7); after.State != domain.IntakeOccurrencePresent {
		t.Fatalf("deferred occurrence must stay present, state = %q", after.State)
	}
}

// TestIntakeRequiresForgeHostInAllowlist proves the loop fails admission closed
// when the initiator's research allowlist omits the forge host, rather than
// silently widening the operator's policy under its original provenance.
func TestIntakeRequiresForgeHostInAllowlist(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	for i := range init.PolicyKeys {
		if init.PolicyKeys[i].Key == specify.PolicyResearchAllowlist {
			init.PolicyKeys[i].Value = "https://docs.example" // no forge host
		}
	}
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)

	var found bool
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		o, ok, err := tx.LatestIntakeOccurrence(t.Context(), intakeTestRepoID, 7, intakeTestLabel)
		found = ok && o.Admission != nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("an allowlist omitting the forge host must not admit")
	}
}

// TestIntakeCrossRepoProjectRefusedAtMint covers the project-authority acceptance
// (issue #740 tie): an initiator whose project is registered to a different
// repository cannot be admitted — the mint gate fails closed.
func TestIntakeCrossRepoProjectRefusedAtMint(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	// Pre-register the project against a DIFFERENT repository.
	foreign, err := domain.NewProject(intakeTestProj, "someone/other", 99999999)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RegisterProject(t.Context(), foreign)
	}); err != nil {
		t.Fatal(err)
	}
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	// A cross-repo project makes admission fail; the pass isolates the failure.
	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission != nil {
		t.Fatal("a cross-repo project must not admit an occurrence")
	}
}

// TestIntakeUnadmittedDepartureAdvances covers the admission-failure departure:
// an occurrence whose admission never completed (a cross-repo mint refusal) is
// advanced out of present when its issue departs, instead of lingering present
// forever. A stuck-present occurrence would let a later re-label reuse its
// ordinal rather than allocate a fresh occurrence, so the re-label allocates
// ordinal 2 here.
func TestIntakeUnadmittedDepartureAdvances(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	// Pre-register the project against a DIFFERENT repository so admission fails.
	foreign, err := domain.NewProject(intakeTestProj, "someone/other", 99999999)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RegisterProject(t.Context(), foreign)
	}); err != nil {
		t.Fatal(err)
	}
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	// Pass 1: labeled-open, admission fails, occurrence left present + unadmitted.
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	before := f.latestOccurrence(t, 7)
	if before.Admission != nil {
		t.Fatal("a cross-repo project must not admit the occurrence")
	}
	if before.State != domain.IntakeOccurrencePresent {
		t.Fatalf("unadmitted occurrence state = %q, want present", before.State)
	}
	// Pass 2: the label is removed (issue still open); the departure advances the
	// unadmitted occurrence to absent rather than skipping it.
	f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"}).reconcile(t.Context(), nil)
	after := f.latestOccurrence(t, 7)
	if after.State != domain.IntakeOccurrenceAbsent {
		t.Fatalf("departed unadmitted occurrence state = %q, want absent", after.State)
	}
	// Pass 3: the label is re-added; a fresh occurrence (next ordinal) is
	// allocated, not the stale one.
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	relabeled := f.latestOccurrence(t, 7)
	if relabeled.Ordinal != before.Ordinal+1 {
		t.Fatalf("re-label ordinal = %d, want %d (a fresh occurrence)", relabeled.Ordinal, before.Ordinal+1)
	}
}

// TestIntakeUnregisteredProjectFailsClosed proves an occurrence whose project was
// never registered is not admitted (GetProject ErrNotFound propagates, nothing
// defaults open). This fixture registers no project of its own, so a fresh
// initiator that also cannot register — because the id is taken by a foreign
// repo — exercises the closed path; the plain unregistered path is the mint
// gate's GetProject miss, covered here by leaving the store empty and asserting
// the loop registers-then-mints in one pass (admission succeeds when the project
// is the occurrence's own).
func TestIntakeRegistersOwnProjectAndAdmits(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil {
		t.Fatal("the loop should register the occurrence's own project and admit")
	}
	var project domain.Project
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		project, err = tx.GetProject(t.Context(), intakeTestProj)
		return err
	}); err != nil {
		t.Fatalf("project authority was not registered: %v", err)
	}
	if project.RepositoryID != intakeTestRepoID {
		t.Fatalf("project repository = %d, want %d", project.RepositoryID, intakeTestRepoID)
	}
}
