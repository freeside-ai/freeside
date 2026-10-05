package engine

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// newFollowUpFilingFixture is the adjudication fixture with one located
// finding the fake adjudicator sends down route, under the given extra policy.
func newFollowUpFilingFixture(
	t *testing.T, severity domain.FindingSeverity, route domain.AdjudicationRoute, policy map[string]string,
) *findingAdjudicationFixture {
	t.Helper()
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	f := newFindingAdjudicationFixtureWithOptions(t, severity, location, "low", "high",
		findingAdjudicationFixtureOptions{note: "producer=fake/test; fixture", policy: policy})
	f.writePath(t, f.headRoot)
	f.workflow.findingAdjudicator = newDeterministicFindingAdjudicator(
		modelRouteEntry(t, f.finding.ID, route, domain.ConfidenceHigh))
	return f
}

// seedFollowUpFilingSubject records the two rows a filing's authority
// resolves through: the run's work-unit declaration and, when asked, its
// project.
func seedFollowUpFilingSubject(t *testing.T, f *findingAdjudicationFixture, withProject bool) {
	t.Helper()
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if withProject {
			if err := tx.RegisterProject(f.ctx, domain.Project{
				ID: f.task.ProjectID, Repo: "owner/repo", RepositoryID: 123,
			}); err != nil {
				return err
			}
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(f.binding.resolvedPolicy),
		}, f.task.RunID, f.task.ProjectID, f.finding.CreatedAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(f.ctx, declaration)
	}); err != nil {
		t.Fatalf("seed filing subject: %v", err)
	}
}

type followUpFilingState struct {
	artifact     domain.FindingAdjudication
	instances    []domain.ProposalInstance
	cards        []domain.AttentionItem
	notices      []domain.AttentionItem
	dispositions []domain.ReviewDispositionRecord
}

func readFollowUpFilingState(t *testing.T, f *findingAdjudicationFixture, round int) followUpFilingState {
	t.Helper()
	var state followUpFilingState
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		if state.artifact, err = tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, round); err != nil {
			return err
		}
		if state.instances, err = tx.ListProposalBatch(
			f.ctx, followUpFilingBatchID(state.artifact.Digest)); err != nil {
			return err
		}
		if state.cards, err = tx.ListOpenAttentionItems(f.ctx, domain.AttentionEffectProposal); err != nil {
			return err
		}
		if state.notices, err = tx.ListOpenAttentionItems(f.ctx, domain.AttentionSystemHealth); err != nil {
			return err
		}
		state.dispositions, err = tx.ListFindingDispositions(f.ctx, f.task.RunID)
		return err
	}); err != nil {
		t.Fatalf("read filing state: %v", err)
	}
	return state
}

func reconcileFollowUpFiling(t *testing.T, f *findingAdjudicationFixture, want productionReviewGateState) {
	t.Helper()
	state, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != want {
		t.Fatalf("reconcile = %d, %v, want %d", state, err, want)
	}
}

// replayFollowUpFiling re-enters the route execution for the round's
// artifact, as a reconcile does for a round that is not yet complete.
func replayFollowUpFiling(t *testing.T, f *findingAdjudicationFixture, artifact domain.FindingAdjudication) {
	t.Helper()
	if _, err := f.workflow.executeFindingAdjudication(
		f.ctx, f.task, f.record, artifact, f.baseRoot, f.headRoot); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func assertOneFilingCard(
	t *testing.T, f *findingAdjudicationFixture, state followUpFilingState, kind domain.FollowUpSourceKind,
) domain.AttentionItem {
	t.Helper()
	if len(state.instances) != 1 || len(state.cards) != 1 || len(state.notices) != 0 {
		t.Fatalf("instances, cards, notices = %d, %d, %d, want 1, 1, 0",
			len(state.instances), len(state.cards), len(state.notices))
	}
	instance, card := state.instances[0], state.cards[0]
	filing := instance.Proposal.FilingProposal
	wantSource := domain.FollowUpFilingSource{
		FindingID: f.finding.ID, AdjudicationDigest: state.artifact.Digest, Kind: kind,
	}
	if instance.Proposal.Kind != domain.EffectFollowUpFiling || filing == nil || filing.Source != wantSource ||
		instance.Admission.ExportIdentity != state.artifact.Digest || instance.Admission.EmissionOrdinal != 1 {
		t.Fatalf("instance = %#v, want one filing for %#v", instance, wantSource)
	}
	if card.ID != domain.ItemID(string(instance.ID)+"/effect") ||
		card.Subject.Type != domain.SubjectProposalBatch || card.DecidedAt != nil ||
		!slices.Equal(card.RequestedDecision,
			[]domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze}) {
		t.Fatalf("card = %#v, want the instance's open approve/decline/snooze item", card)
	}
	return card
}

// A deferred disposition yields one filing proposal and one open card, in the
// transaction that writes the row. A process lost after that commit, a replay
// of the write, and a decision on the card add no second proposal or card.
func TestDeferredDispositionProposesOneFollowUpFiling(t *testing.T) {
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
		domain.PolicyFollowUpFilingLabels:    "lane:spine, deferral",
		domain.PolicyFollowUpFilingMilestone: "1B",
	})
	seedFollowUpFilingSubject(t, f, true)
	after := 0
	f.workflow.transitionHook = func(transition DurableTransition, side DurableTransitionSide) error {
		if transition == DurableTransitionFindingAdjudication && side == DurableTransitionAfter {
			if after++; after == 2 {
				return errors.New("injected process loss after disposition commit")
			}
		}
		return nil
	}
	if _, err := f.workflow.reconcileFindingAdjudication(
		f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err == nil {
		t.Fatal("disposition after-hook did not interrupt reconciliation")
	}
	f.workflow.transitionHook = nil
	reconcileFollowUpFiling(t, f, productionReviewPassed)

	state := readFollowUpFilingState(t, f, 1)
	card := assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	filing := state.instances[0].Proposal.FilingProposal
	if filing.Repository != (domain.FollowUpFilingRepository{Repo: "owner/repo", RepositoryID: 123}) || !slices.Equal(filing.Labels, []string{"deferral", "lane:spine"}) ||
		filing.Milestone == nil || *filing.Milestone != "1B" {
		t.Fatalf("target = %#v, want the project row and policy keys", filing)
	}
	wantBody := "review finding\n\nLocation: daemon/a.go:1\n\nAdjudication rationale: deterministic fake route"
	if filing.Title.Text != "review finding" || filing.Body.Text != wantBody {
		t.Fatalf("text = %q, %q", filing.Title.Text, filing.Body.Text)
	}
	facts, err := f.signet.GetEffectProposalFacts(f.ctx, card.ID)
	if err != nil || facts.FollowUpFiling == nil || facts.SourceIssueClosure != nil ||
		facts.EffectKind != domain.EffectFollowUpFiling {
		t.Fatalf("facts = %#v, %v, want the follow_up_filing arm", facts, err)
	}

	// A later pass, hours on, finds the instance and recomposes nothing.
	later := f.workflow.now().Add(time.Hour)
	f.workflow.now = func() time.Time { return later }
	replayFollowUpFiling(t, f, state.artifact)
	replayed := readFollowUpFilingState(t, f, 1)
	if again := assertOneFilingCard(t, f, replayed, domain.FollowUpSourceDeferredDisposition); again.ID != card.ID ||
		!again.CreatedAt.Equal(*card.CreatedAt) {
		t.Fatalf("replayed card = %#v, want the first card unchanged", again)
	}

	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "decline-filing", DeviceID: "device-test", ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: card.ID, Action: domain.ActionDecline, ItemVersion: card.ItemVersion,
			ArtifactDigests: card.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	replayFollowUpFiling(t, f, state.artifact)
	if decided := readFollowUpFilingState(t, f, 1); len(decided.instances) != 1 || len(decided.cards) != 0 {
		t.Fatalf("after decline: instances, open cards = %d, %d, want 1, 0",
			len(decided.instances), len(decided.cards))
	}
}

// A separate-work verdict yields its proposal once the operator accepts the
// route: none while the adjudication item is open, none after a stop, and
// exactly one across the passes that follow an acceptance.
func TestAcceptedSeparateWorkVerdictProposesOneFollowUpFiling(t *testing.T) {
	for _, action := range []domain.Action{domain.ActionAcceptRecommendedRoute, domain.ActionStop} {
		t.Run(string(action), func(t *testing.T) {
			f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, nil)
			f.workflow.artifacts = f.blobs
			seedFollowUpFilingSubject(t, f, true)
			reconcileFollowUpFiling(t, f, productionReviewPending)
			if open := readFollowUpFilingState(t, f, 1); len(open.instances) != 0 || len(open.cards) != 0 {
				t.Fatalf("before a decision: instances, cards = %d, %d, want 0, 0",
					len(open.instances), len(open.cards))
			}
			decideFindingAdjudicationItem(t, f, productionReviewItemID(f.task.RunID, 1), action)
			for range 2 {
				reconcileFollowUpFiling(t, f, productionReviewPending)
			}
			state := readFollowUpFilingState(t, f, 1)
			if len(state.dispositions) != 0 {
				t.Fatalf("dispositions = %#v, want none", state.dispositions)
			}
			if action == domain.ActionStop {
				if len(state.instances) != 0 || len(state.cards) != 0 {
					t.Fatalf("after a stop: instances, cards = %d, %d, want 0, 0",
						len(state.instances), len(state.cards))
				}
				return
			}
			assertOneFilingCard(t, f, state, domain.FollowUpSourceSeparateWorkVerdict)
		})
	}
}

// A parked run re-enters route execution on every reconcile. Once its
// separate-work filing is proposed, or refused and noticed, a pass commits no
// write, so the revision clients sync against stays where it was.
func TestSeparateWorkFilingReplayWritesNothing(t *testing.T) {
	for _, declared := range []bool{true, false} {
		t.Run(strconv.FormatBool(declared), func(t *testing.T) {
			f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, nil)
			if declared {
				seedFollowUpFilingSubject(t, f, true)
			}
			reconcileFollowUpFiling(t, f, productionReviewPending)
			acceptFindingAdjudicationItem(t, f, productionReviewItemID(f.task.RunID, 1))
			artifact := readFollowUpFilingState(t, f, 1).artifact
			routes := map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteParkSeparateWork}
			var revisions [2]int64
			for pass := range revisions {
				if err := f.workflow.proposeSeparateWorkFilings(f.ctx, f.task, artifact, routes); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				server, err := f.store.ServerState(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				revisions[pass] = server.Revision
			}
			state := readFollowUpFilingState(t, f, 1)
			if revisions[0] != revisions[1] || len(state.instances)+len(state.notices) != 1 ||
				(len(state.cards) == 1) != declared {
				t.Fatalf("revisions = %v, instances, cards, notices = %d, %d, %d",
					revisions, len(state.instances), len(state.cards), len(state.notices))
			}
		})
	}
}

// A run that can carry no filing still records its disposition. It raises one
// notice naming the run and the reason class, and a replay raises no second.
func TestFollowUpFilingRefusalRaisesOneNotice(t *testing.T) {
	for _, tc := range []struct {
		class                 string
		policy                map[string]string
		declared, withProject bool
	}{
		{
			class: "policy_invalid", declared: true, withProject: true,
			policy: map[string]string{domain.PolicyFollowUpFilingLabels: "lane:spine,,deferral"},
		},
		{class: "project_missing", declared: true},
		{class: "work_unit_undeclared"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, tc.policy)
			if tc.declared {
				seedFollowUpFilingSubject(t, f, tc.withProject)
			}
			reconcileFollowUpFiling(t, f, productionReviewPassed)
			replayFollowUpFiling(t, f, readFollowUpFilingState(t, f, 1).artifact)
			state := readFollowUpFilingState(t, f, 1)
			if len(state.dispositions) != 1 || len(state.instances) != 0 || len(state.cards) != 0 {
				t.Fatalf("dispositions, instances, cards = %d, %d, %d, want 1, 0, 0",
					len(state.dispositions), len(state.instances), len(state.cards))
			}
			if len(state.notices) != 1 {
				t.Fatalf("notices = %#v, want one", state.notices)
			}
			notice := state.notices[0]
			if notice.ID != followUpFilingRefusedItemID(f.task.RunID) ||
				!strings.Contains(notice.Reason, string(f.task.RunID)) || !strings.Contains(notice.Reason, tc.class) ||
				!slices.Equal(notice.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) {
				t.Fatalf("notice = %#v, want the run's %s notice", notice, tc.class)
			}
		})
	}
}

// Only a route that concludes "separate work" proposes: a declined finding
// and a P1 defer held on the dispute path yield nothing.
func TestFollowUpFilingProposesNothingForOtherConclusions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		severity domain.FindingSeverity
		route    domain.AdjudicationRoute
		want     productionReviewGateState
	}{
		{"decline", domain.FindingSeverityP2, domain.RouteDecline, productionReviewPassed},
		{"high defer on the dispute path", domain.FindingSeverityP1, domain.RouteDefer, productionReviewPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFollowUpFilingFixture(t, tc.severity, tc.route, nil)
			seedFollowUpFilingSubject(t, f, true)
			reconcileFollowUpFiling(t, f, tc.want)
			if state := readFollowUpFilingState(t, f, 1); len(state.instances) != 0 || len(state.cards) != 0 ||
				len(state.notices) != 0 {
				t.Fatalf("instances, cards, notices = %d, %d, %d, want none",
					len(state.instances), len(state.cards), len(state.notices))
			}
		})
	}
}

// A diminishing-returns finish defers the round's findings without an
// adjudicated defer taking effect, so it proposes no filing.
func TestDiminishingFinishProposesNoFollowUpFiling(t *testing.T) {
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	record, artifact := seedDiminishingReviewRound(t, f)
	execute := func(want productionReviewGateState) {
		t.Helper()
		state, err := f.workflow.executeFindingAdjudication(
			f.ctx, f.task, record, artifact, f.baseRoot, f.headRoot)
		if err != nil || state != want {
			t.Fatalf("diminishing gate = %d, %v, want %d", state, err, want)
		}
	}
	execute(productionReviewPending)
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(
			f.ctx, productionReviewDiminishingItemID(f.task.RunID, record.Round))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(f.ctx, signet.ClientCommand{
		CommandID: "finish-now", DeviceID: "device-test", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionFinishNow, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	execute(productionReviewPassed)
	state := readFollowUpFilingState(t, f, record.Round)
	if len(state.dispositions) != 1 || state.dispositions[0].Disposition != domain.ReviewDispositionDeferred ||
		len(state.instances) != 0 || len(state.cards) != 0 {
		t.Fatalf("dispositions, instances, cards = %#v, %d, %d, want one deferred row and no filing",
			state.dispositions, len(state.instances), len(state.cards))
	}
}

// A replay that reaches a deferred row after a later round fixed the finding
// skips it: the source no longer backs a filing, and nothing is raised.
func TestFollowUpFilingSkipsAStaleSource(t *testing.T) {
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	// The first pass runs before the run is declared, so it proposes nothing.
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	seedFollowUpFilingSubject(t, f, true)
	// The round that verifies a fix reviews a new head.
	later := func(round int, outcome domain.ReviewOutcome, ids ...domain.FindingID) domain.ReviewRecord {
		t.Helper()
		record := f.record
		if outcome == domain.ReviewClean {
			record.HeadSHA = strings.Repeat("3", 40)
		}
		record.InvocationID = domain.InvocationID("review-later-" + strconv.Itoa(round))
		record.Round, record.Outcome, record.FindingIDs = round, outcome, ids
		record.CompletedAt = f.record.CompletedAt.Add(time.Duration(round) * time.Hour)
		got, err := domain.NewReviewRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	second, third := later(2, domain.ReviewFindings, f.finding.ID), later(3, domain.ReviewClean)
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(f.ctx, second, []domain.Finding{f.finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(f.ctx, third, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(f.ctx, domain.ReviewDispositionRecord{
			FindingID: f.finding.ID, RunID: f.task.RunID, Round: 2,
			Disposition: domain.ReviewDispositionFixed, Reason: "fixed in the remediation head",
			RemediationInvocationID: third.InvocationID, CreatedAt: third.CompletedAt,
		})
	}); err != nil {
		t.Fatalf("record the later fix: %v", err)
	}
	artifact := readFollowUpFilingState(t, f, 1).artifact
	routes := map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteDefer}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		return f.workflow.proposeFollowUpFilings(f.ctx, tx, f.task.ProjectID, artifact, routes,
			domain.FollowUpSourceDeferredDisposition)
	}); err != nil {
		t.Fatalf("propose over a stale source: %v", err)
	}
	if state := readFollowUpFilingState(t, f, 1); len(state.instances) != 0 || len(state.cards) != 0 {
		t.Fatalf("instances, cards = %d, %d, want 0, 0", len(state.instances), len(state.cards))
	}
}

// An instance an earlier daemon allocated under the same admission key is
// left as it is, even though this daemon would compose its text differently:
// the replay skips the entry instead of meeting an immutable conflict.
func TestFollowUpFilingReplayKeepsAnInstanceComposedDifferently(t *testing.T) {
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, nil)
	seedFollowUpFilingSubject(t, f, true)
	reconcileFollowUpFiling(t, f, productionReviewPending)
	artifact := readFollowUpFilingState(t, f, 1).artifact
	earlier := domain.ScreenedIssueText{
		Text: "Text an earlier daemon composed", Ruleset: domain.IssueTextRulesetGitHubIssue1,
		Verdict: domain.ScreeningVerdictPassed,
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(f.task.RunID))
		target, err := domain.DeriveFollowUpFilingTarget(
			domain.Project{ID: f.task.ProjectID, Repo: "owner/repo", RepositoryID: 123}, f.binding.resolvedPolicy)
		if err != nil {
			return err
		}
		proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
			SubjectHandle: handle, Target: target, Title: earlier, Body: earlier,
			Source: domain.FollowUpFilingSource{
				FindingID: f.finding.ID, AdjudicationDigest: artifact.Digest,
				Kind: domain.FollowUpSourceSeparateWorkVerdict,
			},
		}, f.binding.resolvedPolicy)
		if err != nil {
			return err
		}
		_, _, err = tx.AllocateProposalInstance(f.ctx, domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceRunEmission, ExportIdentity: artifact.Digest, EmissionOrdinal: 1,
		}, followUpFilingBatchID(artifact.Digest), proposal, f.workflow.now())
		return err
	}); err != nil {
		t.Fatalf("allocate the earlier instance: %v", err)
	}
	acceptFindingAdjudicationItem(t, f, productionReviewItemID(f.task.RunID, 1))
	reconcileFollowUpFiling(t, f, productionReviewPending)
	state := readFollowUpFilingState(t, f, 1)
	if len(state.instances) != 1 || state.instances[0].Proposal.FilingProposal.Title != earlier ||
		len(state.cards) != 0 || len(state.notices) != 0 {
		t.Fatalf("instances, cards, notices = %#v, %d, %d, want the earlier instance alone",
			state.instances, len(state.cards), len(state.notices))
	}
}

// Each source kind selects the entries its own route concluded, by position
// in the artifact, and none the operator moved to another route.
func TestFollowUpFilingEntriesFollowTheEffectiveRoute(t *testing.T) {
	var artifact domain.FindingAdjudication
	routes := map[domain.FindingID]domain.AdjudicationRoute{}
	for _, route := range domain.AllAdjudicationRoutes {
		id := domain.FindingID("finding-" + string(route))
		artifact.Entries = append(artifact.Entries, domain.FindingAdjudicationEntry{FindingID: id, Route: route})
		routes[id] = route
	}
	for kind, route := range map[domain.FollowUpSourceKind]domain.AdjudicationRoute{
		domain.FollowUpSourceDeferredDisposition: domain.RouteDefer,
		domain.FollowUpSourceSeparateWorkVerdict: domain.RouteParkSeparateWork,
	} {
		got := followUpFilingEntries(artifact, routes, kind)
		want := slices.Index(domain.AllAdjudicationRoutes, route) + 1
		if len(got) != 1 || got[0].ordinal != want || got[0].entry.Route != route {
			t.Fatalf("%s entries = %#v, want the %s entry at ordinal %d", kind, got, route, want)
		}
		overridden := map[domain.FindingID]domain.AdjudicationRoute{got[0].entry.FindingID: domain.RouteDecline}
		if moved := followUpFilingEntries(artifact, overridden, kind); len(moved) != 0 {
			t.Fatalf("%s entries after an override = %#v, want none", kind, moved)
		}
	}
}

// Clean finding text is used as written. A field the issue-text screen
// refuses takes its fixed text and leaves the other field alone, and an
// over-long field is cut on a rune boundary.
func TestFollowUpFilingTextScreensEachField(t *testing.T) {
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 4, EndLine: 9}
	long := "a" + strings.Repeat("é", 10<<10)
	for _, tc := range []struct {
		name, message, rationale string
		location                 *domain.FindingLocation
		wantTitle, wantBody      string
	}{
		{
			name: "clean", message: "  Retry budget   is unbounded\nThe client retries forever.\n",
			rationale: "adjacent to the goal", location: location,
			wantTitle: "Retry budget is unbounded",
			wantBody: "Retry budget   is unbounded\nThe client retries forever.\n\n" +
				"Location: daemon/a.go:4-9\n\nAdjudication rationale: adjacent to the goal",
		},
		{
			name: "mention in the rationale", message: "Retry budget is unbounded", rationale: "ask @octocat",
			wantTitle: "Retry budget is unbounded", wantBody: followUpFilingFallbackBody,
		},
		{
			name: "command line in the message", message: "/close this\nRetry budget is unbounded",
			wantTitle: followUpFilingFallbackTitle, wantBody: followUpFilingFallbackBody,
		},
		{
			name: "blank message", message: " \n", rationale: "adjacent to the goal", location: location,
			wantTitle: followUpFilingFallbackTitle,
			wantBody:  "Location: daemon/a.go:4-9\n\nAdjudication rationale: adjacent to the goal",
		},
		{
			name: "over-long", message: long, rationale: "dropped by the cut",
			wantTitle: long[:domain.MaxFollowUpFilingTitleBytes-1], wantBody: long[:domain.MaxFollowUpFilingBodyBytes-1],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, body := followUpFilingText(
				domain.Finding{Message: tc.message, Location: tc.location},
				domain.FindingAdjudicationEntry{Rationale: tc.rationale})
			if title.Text != tc.wantTitle || body.Text != tc.wantBody {
				t.Fatalf("text = %q, %q, want %q, %q", title.Text, body.Text, tc.wantTitle, tc.wantBody)
			}
			if !utf8.ValidString(title.Text) || !utf8.ValidString(body.Text) {
				t.Fatal("cut text is not valid UTF-8")
			}
			if err := errors.Join(
				publicationtext.ScreenIssueTitle(title.Ruleset, title.Text, domain.MaxFollowUpFilingTitleBytes),
				publicationtext.ScreenIssueBody(body.Ruleset, body.Text, domain.MaxFollowUpFilingBodyBytes),
			); err != nil || title.Verdict != domain.ScreeningVerdictPassed || body.Verdict != domain.ScreeningVerdictPassed {
				t.Fatalf("composed text does not pass its own screen: %v", err)
			}
		})
	}
}
