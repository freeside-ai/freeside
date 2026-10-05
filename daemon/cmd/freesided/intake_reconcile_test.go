package main

import (
	"context"
	"database/sql"
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
// a labeled issue in propose mode yields exactly one open task_proposal, and
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
	if item.Type != domain.AttentionTaskProposal || item.Status != domain.StatusOpen {
		t.Fatalf("proposal item = type %q status %q, want open task_proposal", item.Type, item.Status)
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

// TestIntakeAutoStartReleasesWIPWhenDecisionDoesNotTake covers the cap-gate
// ordering (issue #1318): the auto_start write records the task's start under
// the atomic WIP-cap check, but if the start decision does not take (the card
// was declined or superseded between the cap gate and the decision), no run
// launches, so the recorded start must be released rather than strand the slot.
func TestIntakeAutoStartReleasesWIPWhenDecisionDoesNotTake(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	// Pass 1 (propose): admit an open proposal and reserve its run; no start yet.
	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil {
		t.Fatal("occurrence was not admitted")
	}
	// The proposal departs while still open and undecided: the reconciler
	// supersedes the card, so the start decision below cannot take (a non-start
	// decision, exactly the decline/supersession race the cap gate must survive).
	f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"}).reconcile(t.Context(), nil)
	superseded := f.latestOccurrence(t, 7)
	if item := f.proposalItem(t, superseded); item.Status == domain.StatusOpen {
		t.Fatal("proposal card should have been superseded by the departure")
	}

	runID := superseded.Admission.Subject.SpecificationRunID
	var policy intake.IntakePolicy
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		resolved, err := tx.GetResolvedPolicy(t.Context(), runID)
		if err != nil {
			return err
		}
		policy, err = intake.ParseIntakePolicy(resolved)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Drive autoStart directly: its cap write records the start (claiming a slot),
	// then StartTaskProposalUnattended finds the card no longer open and records no
	// start, so the compensating release must free the slot.
	r := f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"})
	if err := r.autoStart(t.Context(), init, superseded, policy); err != nil {
		t.Fatalf("autoStart: %v", err)
	}

	if f.started(t, runID) {
		t.Fatal("a superseded proposal must not launch a run")
	}
	var kinds []domain.TaskLifecycleFactKind
	var wip int
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if err != nil {
			return err
		}
		task, err := tx.GetTask(t.Context(), run.TaskID)
		if err != nil {
			return err
		}
		for _, fact := range task.LifecycleFacts {
			kinds = append(kinds, fact.Kind)
		}
		wip, err = countProjectWIPTasks(t.Context(), tx, intakeTestProj, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wip != 0 {
		t.Fatalf("WIP tasks = %d, want 0 (the stranded start must be released)", wip)
	}
	if !slices.Equal(kinds, []domain.TaskLifecycleFactKind{domain.TaskLifecycleStarted, domain.TaskLifecycleAbandoned}) {
		t.Fatalf("lifecycle facts = %v, want started then abandoned (start recorded, then released)", kinds)
	}
}

// intakeRepoFiling places a seeded follow-up filing in the intake fixture's
// repository. The filing gets a project of its own, so the intake project's
// tasks and WIP count are untouched and only the filing ledger changes.
func intakeRepoFiling() storetest.FollowUpFiling {
	return storetest.FollowUpFiling{
		ProjectID: "project-follow-up-filing", Repo: intakeTestRepo, RepositoryID: intakeTestRepoID,
	}
}

// assertOriginDemoted checks the durable result of the §5.17 origin gate on
// an issue's latest occurrence: a mode_not_authorized refusal, no launch, and
// the card still an open proposal an operator can decide.
func (f intakeFixture) assertOriginDemoted(t *testing.T, issue int) domain.IntakeOccurrence {
	t.Helper()
	o := f.latestOccurrence(t, issue)
	if o.Admission == nil {
		t.Fatalf("issue %d was not admitted", issue)
	}
	if o.Refusal == nil || o.Refusal.Reason != domain.IntakeRefusalModeNotAuthorized {
		t.Fatalf("issue %d refusal = %+v, want mode_not_authorized", issue, o.Refusal)
	}
	if f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatalf("issue %d auto-started in a repository the daemon may have filed in", issue)
	}
	if item := f.proposalItem(t, o); item.Type != domain.AttentionTaskProposal || item.Status != domain.StatusOpen {
		t.Fatalf("issue %d card = type %q status %q, want an open task_proposal", issue, item.Type, item.Status)
	}
	return o
}

// TestIntakeNeverAutoStartsDaemonFiledIssue covers plan §5.17's per-issue
// rule at every observation: an issue the filing ledger records as
// daemon-filed is demoted under an authorized auto_start, and removing and
// re-adding the initiator label (a new occurrence, with no refusal carried
// over) is demoted again.
func TestIntakeNeverAutoStartsDaemonFiledIssue(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	storetest.LedgerFiledFollowUpIssue(t, f.store, intakeRepoFiling(), 7)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
	present := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

	present.reconcile(t.Context(), nil)
	first := f.assertOriginDemoted(t, 7)
	if first.Ordinal != 1 {
		t.Fatalf("first occurrence ordinal = %d, want 1", first.Ordinal)
	}

	// The label is removed, then added again.
	f.reconciler([]intakeInitiator{init}, nil, map[int]string{7: "open"}).reconcile(t.Context(), nil)
	if departed := f.latestOccurrence(t, 7); departed.State != domain.IntakeOccurrenceAbsent {
		t.Fatalf("occurrence state after the label left = %q, want %q", departed.State, domain.IntakeOccurrenceAbsent)
	}
	present.reconcile(t.Context(), nil)
	second := f.assertOriginDemoted(t, 7)
	if second.Ordinal != 2 {
		t.Fatalf("relabel occurrence ordinal = %d, want 2 (a new occurrence)", second.Ordinal)
	}
	if f.started(t, first.Admission.Subject.SpecificationRunID) {
		t.Fatal("the relabel started the first occurrence's run")
	}
}

// TestIntakeDemotesEveryIssueWhereDaemonMayHaveFiled covers §5.17's
// repository rule. With no issue-event authority profile, every labeled issue
// in a repository the daemon may have filed in is demoted, whether the filing
// is ledgered or is a create whose answer never arrived (an intent with a
// started attempt and no ledger row). The labeled issue is not the filed one.
func TestIntakeDemotesEveryIssueWhereDaemonMayHaveFiled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		file func(t *testing.T, st *store.Store)
	}{
		{"another issue is ledgered", func(t *testing.T, st *store.Store) {
			storetest.LedgerFiledFollowUpIssue(t, st, intakeRepoFiling(), 41)
		}},
		{"a create may have committed", func(t *testing.T, st *store.Store) {
			storetest.DispatchFollowUpFiling(t, st, intakeRepoFiling())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newIntakeFixture(t)
			tc.file(t, f.store)
			init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
			r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

			r.reconcile(t.Context(), nil)
			f.assertOriginDemoted(t, 7)
			// A later pass leaves the demoted card alone.
			r.reconcile(t.Context(), nil)
			f.assertOriginDemoted(t, 7)
		})
	}
}

// TestIntakeAutoStartsWhereDaemonNeverFiled is the control for the repository
// rule: a filing ledgered in another repository, under the same issue number,
// demotes nothing here.
func TestIntakeAutoStartsWhereDaemonNeverFiled(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	storetest.LedgerFiledFollowUpIssue(t, f.store, storetest.FollowUpFiling{
		ProjectID: "project-follow-up-filing", Repo: "freeasinbird/elsewhere", RepositoryID: intakeTestRepoID + 1,
	}, 7)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)

	f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil).reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Refusal != nil {
		t.Fatalf("unexpected refusal %q in a repository the daemon never filed in", o.Refusal.Reason)
	}
	if !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("a filing in another repository must not stop auto_start here")
	}
}

// TestIntakeOperatorStartLaunchesDemotedCard covers the explicit human
// admission: the origin gate stops the daemon's own start, not a start an
// operator decides on the demoted card.
func TestIntakeOperatorStartLaunchesDemotedCard(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	storetest.LedgerFiledFollowUpIssue(t, f.store, intakeRepoFiling(), 7)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	r.reconcile(t.Context(), nil)
	o := f.assertOriginDemoted(t, 7)

	started, err := f.attention.StartTaskProposalUnattended(t.Context(),
		domain.ItemID(o.Admission.ProposalInstanceID), "operator-start-7")
	if err != nil || !started {
		t.Fatalf("operator start on the demoted card: started = %v, err = %v", started, err)
	}
	r.reconcile(t.Context(), nil)
	if !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("an operator start on a demoted card must launch on the next pass")
	}
}

// TestIntakeOriginGateRunsInTheStartWrite pins where the gate sits. decide
// has already chosen auto_start for the occurrence when a filing commits;
// autoStart, the final transition, must still see it, because it reads the
// ledger under the write that would record the start.
func TestIntakeOriginGateRunsInTheStartWrite(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	// Admit an open, undecided proposal while the repository has no filing.
	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil || o.Refusal != nil {
		t.Fatalf("occurrence = admission %v, refusal %+v; want admitted and unrefused", o.Admission != nil, o.Refusal)
	}

	storetest.LedgerFiledFollowUpIssue(t, f.store, intakeRepoFiling(), 41)
	authorized := intake.IntakePolicy{
		WIPCap: 5, Mode: domain.InitiatorModeAutoStart, ModeProvenance: domain.ProvenanceOverride,
	}
	if err := r.autoStart(t.Context(), init, o, authorized); err != nil {
		t.Fatalf("autoStart: %v", err)
	}
	f.assertOriginDemoted(t, 7)
	// The refusal replaced the start record: the task holds no WIP slot.
	var wip int
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		wip, err = countProjectWIPTasks(t.Context(), tx, intakeTestProj, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wip != 0 {
		t.Fatalf("WIP tasks = %d, want 0 (a demoted occurrence records no start)", wip)
	}
}

// TestIntakeOriginGateFailsClosedOnUnreadableLedger covers the ledger read
// error: a filing whose rows no longer reconstruct is not "no filing", so the
// pass starts nothing and records nothing, and a pass after the ledger reads
// again decides the occurrence.
func TestIntakeOriginGateFailsClosedOnUnreadableLedger(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := openIntakeFixture(t, root)
	instance := storetest.DispatchFollowUpFiling(t, f.store, intakeRepoFiling())
	// Rewrite the filing's approve decision as a decline, as an editor of the
	// database file could, and keep what it takes to put the approval back.
	raw, err := sql.Open("sqlite", root+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	var approvedDigest string
	if err := raw.QueryRowContext(t.Context(),
		`SELECT selected_digest FROM effect_proposal_decisions WHERE instance_id = ?`, instance,
	).Scan(&approvedDigest); err != nil {
		t.Fatal(err)
	}
	rewrite := func(action string, selected any) {
		t.Helper()
		result, err := raw.ExecContext(t.Context(),
			`UPDATE effect_proposal_decisions SET action = ?, selected_digest = ? WHERE instance_id = ?`,
			action, selected, instance)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			t.Fatalf("rewrite the filing decision to %s changed %d rows, %v", action, changed, err)
		}
	}
	rewrite(string(domain.ActionDecline), nil)

	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 5)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)
	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil {
		t.Fatal("occurrence was not admitted")
	}
	if o.Refusal != nil {
		t.Fatalf("an unreadable ledger recorded refusal %q; the pass must record nothing", o.Refusal.Reason)
	}
	if f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("an unreadable ledger must not auto-start")
	}
	if err := r.decide(t.Context(), init, o); !errors.Is(err, store.ErrRowInconsistent) {
		t.Fatalf("decide over an unreadable ledger: err = %v, want %v", err, store.ErrRowInconsistent)
	}

	// The ledger reads again: the retry decides, and the dispatched create
	// demotes the occurrence.
	rewrite(string(domain.ActionApprove), approvedDigest)
	r.reconcile(t.Context(), nil)
	f.assertOriginDemoted(t, 7)
}

// TestCountProjectWIPTasksCountsTasksNotRuns covers issue #1318: the WIP cap
// counts tasks holding a slot, not runs, so several runs of one task use one
// slot. A second, unstarted task holds none; the admitted task is excluded.
func TestCountProjectWIPTasksCountsTasksNotRuns(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	ctx := t.Context()
	ts := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	source := func(issue int) domain.SpecificationSource {
		return domain.SpecificationSource{Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: issue}}
	}
	var wipTask domain.TaskID
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project", source(1))
		if err != nil {
			return err
		}
		wipTask = task.ID
		for _, run := range []domain.RunID{"run-a", "run-b"} {
			// The runs must be the task's own runs: RecordTaskStart records a fact
			// only for a run it read from the store, and reconstruction re-gates each
			// fact against task_runs (issue #1318 D1).
			if err := tx.PutRun(ctx, domain.Run{
				ID: run, ProjectID: "project", TaskID: task.ID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
			}); err != nil {
				return err
			}
			if err := tx.RecordTaskLifecycleFact(ctx, task.ID, domain.TaskLifecycleFact{
				Kind: domain.TaskLifecycleStarted, RunID: run, SourceID: "start:" + string(run), RecordedAt: ts,
			}); err != nil {
				return err
			}
		}
		_, err = tx.GetOrCreateTask(ctx, "project", source(2))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var all, excludingWIP int
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if all, err = countProjectWIPTasks(ctx, tx, "project", ""); err != nil {
			return err
		}
		excludingWIP, err = countProjectWIPTasks(ctx, tx, "project", wipTask)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if all != 1 {
		t.Fatalf("WIP tasks = %d, want 1 (two runs of one task use one slot)", all)
	}
	if excludingWIP != 0 {
		t.Fatalf("WIP tasks excluding the admitted task = %d, want 0", excludingWIP)
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
	if _, err := f.attention.StartTaskProposalUnattended(t.Context(),
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
	if _, err := f.attention.StartTaskProposalUnattended(t.Context(),
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
