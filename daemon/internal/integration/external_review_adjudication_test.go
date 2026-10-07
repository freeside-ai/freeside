package integration_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// configureAdjudicator gives the harness a classifier and an adjudicator on a
// scripted driver and rebuilds the engine with them. It returns the driver so
// a test can script the adjudicator and read what it was sent.
func (p *productionPublicationHarness) configureAdjudicator(t *testing.T) *inferencefake.Driver {
	t.Helper()
	driver := inferencefake.New()
	driver.Script(inference.ClassifierSiteID, inferencefake.Script{Response: inference.Response{
		Output:       []byte(`{"materiality":"high","confidence":"high","note":"actionable"}`),
		ComputeUnits: 3,
	}})
	advisoryStore, err := advisory.Open(
		filepath.Join(t.TempDir(), "advisory.json"), 20, 16<<10,
		advisory.WithClock(func() time.Time { return p.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{
		Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour,
	}
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
	p.judgments, err = inference.New(inference.Config{
		StatePath: filepath.Join(t.TempDir(), "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "adjudicator", Driver: driver},
		Sites:     []inference.Site{inference.ClassifierSite(budget), inference.AdjudicatorSite(budget)},
		Advisory:  advisoryStore, Now: func() time.Time { return p.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return driver
}

// adjudicatorVerdict is one entry a scripted adjudicator returns.
type adjudicatorVerdict struct {
	finding domain.FindingID
	goal    domain.GoalRelationship
	// compatibility and route stay empty for the proposal the engine composes
	// itself: a required fix inside the declared paths.
	compatibility domain.ProposedCompatibility
	route         domain.AdjudicationRoute
}

func declineVerdict(id domain.FindingID) adjudicatorVerdict {
	return adjudicatorVerdict{finding: id, goal: domain.GoalContradictory, route: domain.RouteDecline}
}

func deferVerdict(id domain.FindingID) adjudicatorVerdict {
	return adjudicatorVerdict{finding: id, goal: domain.GoalAdjacent, route: domain.RouteDefer}
}

func disputeVerdict(id domain.FindingID) adjudicatorVerdict {
	return adjudicatorVerdict{finding: id, goal: domain.GoalContradictory, route: domain.RouteDispute}
}

func remediateVerdict(id domain.FindingID) adjudicatorVerdict {
	return adjudicatorVerdict{finding: id, goal: domain.GoalRequired}
}

// scriptAdjudication scripts the adjudicator's next answer.
func scriptAdjudication(t *testing.T, driver *inferencefake.Driver, verdicts ...adjudicatorVerdict) {
	t.Helper()
	type entry struct {
		FindingID        domain.FindingID              `json:"finding_id"`
		GoalRelationship domain.GoalRelationship       `json:"goal_relationship"`
		Compatibility    *domain.ProposedCompatibility `json:"compatibility"`
		Route            *domain.AdjudicationRoute     `json:"route"`
		Confidence       string                        `json:"confidence"`
		Rationale        string                        `json:"rationale"`
		Evidence         []string                      `json:"evidence"`
		CitedRules       []string                      `json:"cited_rules"`
		Assumptions      []string                      `json:"assumptions"`
		Alternatives     []string                      `json:"alternatives"`
		OpenQuestions    []string                      `json:"open_questions"`
	}
	entries := make([]entry, 0, len(verdicts))
	for _, verdict := range verdicts {
		next := entry{
			FindingID: verdict.finding, GoalRelationship: verdict.goal,
			Confidence: "high", Rationale: "judged against the approved specification",
			Evidence: []string{"README.md:1"}, CitedRules: []string{"declared scope"},
			Assumptions: []string{}, Alternatives: []string{}, OpenQuestions: []string{},
		}
		if verdict.compatibility != "" {
			next.Compatibility = &verdict.compatibility
		}
		if verdict.route != "" {
			next.Route = &verdict.route
		}
		entries = append(entries, next)
	}
	output, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	driver.Script(inference.AdjudicatorSiteID, inferencefake.Script{Response: inference.Response{
		Output: output, ComputeUnits: 4,
	}})
}

// adjudicatorRequests returns what the adjudicator was sent, oldest first.
func adjudicatorRequests(driver *inferencefake.Driver) []inference.Request {
	var out []inference.Request
	for _, request := range driver.Requests() {
		if request.SiteID == inference.AdjudicatorSiteID {
			out = append(out, request)
		}
	}
	return out
}

// externalDispositions reads the run's external finding dispositions.
func (p *productionPublicationHarness) externalDispositions(t *testing.T) []domain.ExternalFindingDisposition {
	t.Helper()
	var out []domain.ExternalFindingDisposition
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		out, err = tx.ListExternalFindingDispositions(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// startAdjudicatedExternalReview publishes the run, lists the maintainer, and
// starts an external review cycle over the texts the maintainer left on the
// published head. It returns the first cycle's ready item and the findings.
func (p *productionPublicationHarness) startAdjudicatedExternalReview(
	t *testing.T, texts ...string,
) (domain.AttentionItem, []domain.Finding) {
	t.Helper()
	first := p.publishReady(t)
	p.listExternalReviewers(t, externalMaintainer())
	findings := make([]domain.Finding, 0, len(texts))
	for index, text := range texts {
		findings = append(findings,
			p.leaveExternalFinding(t, index+1, externalMaintainer(), p.replay.HeadSHA, text))
	}
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding on the published head started no cycle")
	}
	return first, findings
}

// TestExternalReviewCycleReearnsReadinessWhenEveryFindingIsAnswered: Freeside's
// own review of the head is clean, and adjudication declines one admitted
// finding and defers the other. Nothing needs a fix and nothing needs a
// person, so each finding gets its outcome in the cycle's first round and the
// run is ready again on the head the reviewer read. Neither finding starts
// another cycle.
func TestExternalReviewCycleReearnsReadinessWhenEveryFindingIsAnswered(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	first, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle", "rename this too")
	head := p.replay.HeadSHA
	pushes, body := p.transport.pushCount(), p.forge.pullRequests()[0].Body

	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID), deferVerdict(findings[1].ID))
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	p.assertReentered(t, first, 2, p.baseSHA, head)
	if p.transport.pushCount() != pushes || p.forge.pullRequests()[0].Body != body {
		t.Fatal("the cycle pushed or rewrote the pull request")
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 0 || !end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
			len(end.reviewItems), end.readyItem, end.pending)
	}

	dispositions := p.externalDispositions(t)
	if len(dispositions) != 2 {
		t.Fatalf("external dispositions = %#v, want one per admitted finding", dispositions)
	}
	want := map[domain.FindingID]domain.ReviewDisposition{
		findings[0].ID: domain.ReviewDispositionDeclined,
		findings[1].ID: domain.ReviewDispositionDeferred,
	}
	for _, disposition := range dispositions {
		if disposition.Round != 2 || disposition.Disposition != want[disposition.FindingID] ||
			disposition.AdjudicationDigest == nil || disposition.RemediationInvocationID != nil {
			t.Errorf("external disposition = %#v", disposition)
		}
		delete(want, disposition.FindingID)
	}
	if len(want) != 0 {
		t.Fatalf("findings without an outcome: %v", want)
	}

	// The reviewer's words reach the adjudicator only in the list of their own.
	requests := adjudicatorRequests(driver)
	if len(requests) != 1 {
		t.Fatalf("adjudicator requests = %d, want 1", len(requests))
	}
	assertExternalFindingsFramed(t, requests[0], findings)

	// The trigger skips an answered finding: nothing is left to start a cycle.
	if p.startExternalReview(t) {
		t.Fatal("an answered finding started a second cycle")
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if len(adjudicatorRequests(driver)) != 1 {
		t.Fatal("a converged replay adjudicated again")
	}
}

// assertExternalFindingsFramed checks one adjudicator request against issue
// #1767 decision 3: every admitted finding is in external_findings, quoted and
// labelled, and none is in findings.
func assertExternalFindingsFramed(t *testing.T, request inference.Request, findings []domain.Finding) {
	t.Helper()
	fields := request.Fields
	var external []struct {
		FindingID     domain.FindingID `json:"finding_id"`
		ReviewerLogin string           `json:"reviewer_login"`
		ThreadID      string           `json:"thread_id"`
		HeadSHA       string           `json:"head_sha"`
		QuotedText    string           `json:"quoted_text"`
		Notice        string           `json:"notice"`
	}
	if err := json.Unmarshal([]byte(fields["external_findings"]), &external); err != nil {
		t.Fatalf("external_findings = %q: %v", fields["external_findings"], err)
	}
	if len(external) != len(findings) {
		t.Fatalf("external_findings carries %d findings, want %d", len(external), len(findings))
	}
	for index, finding := range findings {
		got := external[index]
		if got.FindingID != finding.ID || got.ReviewerLogin != finding.External.ReviewerLogin ||
			got.ThreadID != finding.External.ThreadID || got.HeadSHA != finding.External.HeadSHA ||
			got.QuotedText != `"`+finding.Message+`"` ||
			!strings.Contains(got.Notice, "quoted as data") {
			t.Errorf("external finding %d = %#v", index, got)
		}
		if strings.Contains(fields["findings"], finding.Message) ||
			strings.Contains(fields["findings"], string(finding.ID)) {
			t.Errorf("findings carries external finding %s: %s", finding.ID, fields["findings"])
		}
	}
}

// TestExternalReviewCycleDisputeNamesTheFinding: adjudication disputes an
// admitted finding, so the cycle ends on a person. The item names the finding
// by its reviewer and thread and quotes what the reviewer wrote, and the
// finding gets no outcome: the person's answer is the outcome.
func TestExternalReviewCycleDisputeNamesTheFinding(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA

	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, disputeVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
			len(end.reviewItems), end.readyItem, end.pending)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDispute || item.PRHeadSHA != head {
		t.Fatalf("dispute item = %#v", item)
	}
	if item.ID != productionReviewItemIDForTest(p.runID, 2) ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Errorf("dispute item %s reason:\n%s", item.ID, item.Reason)
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
		t.Fatalf("a disputed finding got an outcome: %#v", dispositions)
	}
	if p.startExternalReview(t) {
		t.Fatal("a second cycle started with the first one's item open")
	}
}

// parkVerdict judges a finding to need work outside this work unit, a route
// only a person can accept.
func parkVerdict(id domain.FindingID) adjudicatorVerdict {
	return adjudicatorVerdict{
		finding: id, goal: domain.GoalRequired,
		compatibility: domain.ProposedSeparateWork, route: domain.RouteParkSeparateWork,
	}
}

// decideAdjudicationCard records a person's decision on an open adjudication
// card, as the command path does.
func (p *productionPublicationHarness) decideAdjudicationCard(
	t *testing.T, item domain.AttentionItem, action domain.Action,
) {
	t.Helper()
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-external-card", DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: action,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutCommand(p.ctx, command); err != nil {
			return err
		}
		concluded, err := item.WithDecidedAt(p.now.UTC())
		if err != nil {
			return err
		}
		concluded.Status = domain.StatusResolved
		concluded.ItemVersion++
		return tx.PutAttentionItem(p.ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalReviewCycleCardNamesTheFinding: adjudication gives an admitted
// finding a route that needs a person's choice, so the cycle waits on the
// adjudication card, which names the finding by reviewer and thread and quotes
// it. The cycle records no outcome but decline and defer, so the card says
// accepting records none, and once the person accepts the route the cycle ends
// on an item that says so.
func TestExternalReviewCycleCardNamesTheFinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		action  domain.Action
		handoff bool
	}{
		{"accepted", domain.ActionAcceptRecommendedRoute, true},
		{"stopped", domain.ActionStop, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			driver := p.configureAdjudicator(t)
			_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
			head := p.replay.HeadSHA

			p.scriptCleanReview(2, p.baseSHA, head)
			scriptAdjudication(t, driver, parkVerdict(findings[0].ID))
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
				t.Fatalf("cycle result = %#v, %v", result, err)
			}
			var card domain.AttentionItem
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				var err error
				card, err = tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if card.Type != domain.AttentionFindingAdjudication || card.Status != domain.StatusOpen ||
				card.PRHeadSHA != head {
				t.Fatalf("adjudication card = %#v", card)
			}
			for _, want := range []string{
				string(findings[0].ID), "external finding, maintainer on review_comment/1",
				`the reviewer wrote "this leaks the handle"`,
				"Accepting fixes nothing and records no outcome",
				"a new item hands the findings to a person.",
			} {
				if !strings.Contains(card.Reason, want) {
					t.Errorf("card reason lacks %q:\n%s", want, card.Reason)
				}
			}
			// The cycle waits on the card and on nothing else.
			if waiting := p.externalReviewEnd(t); len(waiting.reviewItems) != 0 || waiting.readyItem ||
				waiting.pending != 1 {
				t.Fatalf("waiting cycle = %#v", waiting)
			}
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
				t.Fatalf("waiting replay = %#v, %v", result, err)
			}
			if len(adjudicatorRequests(driver)) != 1 {
				t.Fatal("a waiting cycle adjudicated again")
			}

			p.decideAdjudicationCard(t, card, tc.action)
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
				t.Fatalf("decided cycle = %#v, %v", result, err)
			}
			end := p.externalReviewEnd(t)
			if end.readyItem || end.pending != 0 {
				t.Fatalf("decided cycle end = ready item %t, %d tasks pending", end.readyItem, end.pending)
			}
			if !tc.handoff {
				if len(end.reviewItems) != 0 {
					t.Fatalf("a stopped card raised %#v", end.reviewItems)
				}
				return
			}
			if len(end.reviewItems) != 1 ||
				end.reviewItems[0].ID != domain.ItemID("production-external-review-handoff-"+string(p.runID)+"-2") {
				t.Fatalf("decided cycle items = %#v", end.reviewItems)
			}
			for _, want := range []string{
				"Adjudication left a finding that is neither declined, deferred, nor fixed in this pull request.",
				`maintainer on review_comment/1: "this leaks the handle"`,
			} {
				if !strings.Contains(end.reviewItems[0].Reason, want) {
					t.Errorf("handoff item reason lacks %q:\n%s", want, end.reviewItems[0].Reason)
				}
			}
			if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
				t.Fatalf("a parked finding got an outcome: %#v", dispositions)
			}
		})
	}
}

// TestExternalReviewCycleJudgesFreesideFindingsWithExternalOnes: Freeside's
// own review of the head finds something too. One adjudication judges the
// record's finding and the admitted external finding together, each gets its
// own kind of outcome in the cycle's first round, and the run is ready again.
func TestExternalReviewCycleJudgesFreesideFindingsWithExternalOnes(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	// With no classification the record's finding is residue for the
	// adjudicator instead of the engine's own remediate route.
	driver.Script(inference.ClassifierSiteID)
	first, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA

	own := domain.Finding{
		ID: "external-cycle-own-finding", RunID: p.runID, Source: "codex_local",
		Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "a note for later", RawText: "a note for later", CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("mixed cycle findings")),
			Findings: []domain.Finding{own},
		},
	})
	scriptAdjudication(t, driver, deferVerdict(own.ID), declineVerdict(findings[0].ID))
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	if ready := p.currentReadyItem(t); ready.ID == first.ID || ready.Status != domain.StatusOpen ||
		ready.PRHeadSHA != head {
		t.Fatalf("ready item = %#v", ready)
	}
	requests := adjudicatorRequests(driver)
	if len(requests) != 1 {
		t.Fatalf("adjudicator requests = %d, want 1", len(requests))
	}
	assertExternalFindingsFramed(t, requests[0], findings)
	if !strings.Contains(requests[0].Fields["findings"], string(own.ID)) {
		t.Fatalf("findings lacks the record's finding: %s", requests[0].Fields["findings"])
	}
	external := p.externalDispositions(t)
	if len(external) != 1 || external[0].FindingID != findings[0].ID ||
		external[0].Disposition != domain.ReviewDispositionDeclined || external[0].Round != 2 {
		t.Fatalf("external dispositions = %#v", external)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		disposition, err := tx.GetFindingDisposition(p.ctx, own.ID, 2)
		if err != nil {
			return err
		}
		if disposition.Disposition != domain.ReviewDispositionDeferred {
			t.Errorf("record finding disposition = %#v", disposition)
		}
		// An external finding never gets a review disposition record.
		if _, err := tx.GetFindingDisposition(p.ctx, findings[0].ID, 2); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("external finding review disposition = %v, want none", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalReviewCycleEndsOnAPersonAfterAFailedFirstReview: the cycle's
// first review fails and is retried, so its review record lands one round
// after the round the authority names. Outcomes for external findings are
// keyed to the authority's round, so the cycle judges nothing and ends on a
// person (issue #1767 decision 4).
func TestExternalReviewCycleEndsOnAPersonAfterAFailedFirstReview(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA

	p.scriptCleanReview(2, p.baseSHA, head)
	p.scriptCleanReview(3, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID))
	p.reviewSource = &faultReviewSource{
		ReviewSource: p.reviewer, failPollAt: 1,
		failPollWith: errors.Join(exec.ErrNoResult, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient, Err: errors.New("connection reset"),
		}),
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 6 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		if end := p.externalReviewEnd(t); len(end.reviewItems) != 0 || end.readyItem {
			break
		}
		p.now = p.now.Add(2 * time.Second)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
			len(end.reviewItems), end.readyItem, end.pending)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Round != 3 || end.successor.ReviewRound != 2 {
			t.Fatalf("review round = %d under authority round %d, want 3 under 2",
				review.Round, end.successor.ReviewRound)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDispute ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("item = %s: %s", item.Type, item.Reason)
	}
	if len(adjudicatorRequests(driver)) != 0 || len(p.externalDispositions(t)) != 0 {
		t.Fatal("a cycle whose first review is off the authority's round adjudicated")
	}
}

// TestExternalReviewOutcomesSurviveAnAllowlistEdit: the cycle's adjudication
// and outcomes are bound to the profile the authority names. After the owner
// removes the reviewer, the round reads back the same and the run stays ready.
func TestExternalReviewOutcomesSurviveAnAllowlistEdit(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	ready := p.currentReadyItem(t)
	read := func() (domain.FindingAdjudication, []domain.ExternalFindingDisposition) {
		t.Helper()
		var adjudication domain.FindingAdjudication
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			adjudication, err = tx.GetFindingAdjudicationForRound(p.ctx, p.runID, 2)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return adjudication, p.externalDispositions(t)
	}
	adjudication, dispositions := read()
	if len(adjudication.Entries) != 1 || adjudication.Entries[0].FindingID != findings[0].ID ||
		len(dispositions) != 1 {
		t.Fatalf("round 2 = %#v, %#v", adjudication, dispositions)
	}

	p.listExternalReviewers(t)
	after, afterDispositions := read()
	if after.Digest != adjudication.Digest || len(afterDispositions) != 1 ||
		afterDispositions[0] != dispositions[0] && *afterDispositions[0].AdjudicationDigest != *dispositions[0].AdjudicationDigest {
		t.Fatalf("round 2 after the allowlist edit = %#v, %#v", after, afterDispositions)
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("pass after the allowlist edit = %#v, %v", result, err)
	}
	if again := p.currentReadyItem(t); again.ID != ready.ID || again.Status != domain.StatusOpen {
		t.Fatalf("ready item after the allowlist edit = %#v", again)
	}
	if p.startExternalReview(t) {
		t.Fatal("an answered finding started a cycle after the allowlist edit")
	}
}

// TestExternalReviewCycleSkipsFindingsAnEarlierCycleAnswered: a second
// finding on the same head starts a second cycle. The first finding already
// has its outcome, so the second cycle judges only the new one, and its item
// lists the first as already answered.
func TestExternalReviewCycleSkipsFindingsAnEarlierCycleAnswered(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("first cycle result = %#v, %v", result, err)
	}

	later := p.leaveExternalFinding(t, 2, externalMaintainer(), head, "and this one races")
	if !p.startExternalReview(t) {
		t.Fatal("a new finding on the published head started no cycle")
	}
	if successor := p.externalReviewEnd(t).successor; successor.ExternalFindingID != later.ID ||
		successor.ReviewRound != 3 {
		t.Fatalf("second cycle authority = %#v", successor)
	}
	p.scriptCleanReview(3, p.baseSHA, head)
	scriptAdjudication(t, driver, disputeVerdict(later.ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("second cycle result = %#v, %v", result, err)
	}
	requests := adjudicatorRequests(driver)
	if len(requests) != 2 {
		t.Fatalf("adjudicator requests = %d, want 2", len(requests))
	}
	assertExternalFindingsFramed(t, requests[1], []domain.Finding{later})
	if strings.Contains(requests[1].Fields["external_findings"], string(findings[0].ID)) {
		t.Fatalf("the second cycle judged an answered finding again: %s", requests[1].Fields["external_findings"])
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem {
		t.Fatalf("second cycle end = %#v", end)
	}
	reason := end.reviewItems[0].Reason
	for _, want := range []string{
		`"and this one races"`,
		"Already answered in an earlier cycle and not judged again:",
		`maintainer on review_comment/1: "this leaks the handle"`,
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("second cycle item reason lacks %q:\n%s", want, reason)
		}
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 1 ||
		dispositions[0].FindingID != findings[0].ID || dispositions[0].Round != 2 {
		t.Fatalf("external dispositions = %#v", dispositions)
	}
}

// TestExternalReviewStartsNoCycleUnderAnUnfinishedTask: a cycle wrote its
// ready item and lost its process before its task ended. A finding left in
// that window starts no cycle until the task ends: started under it, the
// unfinished task would come back to a run whose round has passed and raise
// an item against findings the next cycle already answered.
func TestExternalReviewStartsNoCycleUnderAnUnfinishedTask(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID))
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterReady: func() error { return errors.New("injected process loss") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("the cycle was not interrupted after its ready item")
	}
	if end := p.externalReviewEnd(t); !end.readyItem || end.pending != 1 {
		t.Fatalf("interrupted cycle = ready item %t, %d tasks pending", end.readyItem, end.pending)
	}

	later := p.leaveExternalFinding(t, 2, externalMaintainer(), head, "and this one races")
	if p.startExternalReview(t) {
		t.Fatal("a cycle started under a task that had not ended")
	}

	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if end := p.externalReviewEnd(t); !end.readyItem || end.pending != 0 || len(end.reviewItems) != 0 {
		t.Fatalf("healed cycle = %#v", end)
	}
	if !p.startExternalReview(t) {
		t.Fatal("the later finding started no cycle once the task ended")
	}
	if successor := p.externalReviewEnd(t).successor; successor.ExternalFindingID != later.ID {
		t.Fatalf("second cycle authority = %#v", successor)
	}
}

// TestExternalReviewCycleStoppedByTheReviewPolicyEndsOnAPerson: every finding
// of the round is declined or deferred, but the review policy stops the run
// at this round. The item that stop raises answers only the review record's
// findings, so the cycle ends on its own item, which names the reviewer's
// finding, and the run is not ready.
func TestExternalReviewCycleStoppedByTheReviewPolicyEndsOnAPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.low_value_streak_before_attention", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-low-value-streak"),
		},
	}})
	driver := p.configureAdjudicator(t)
	driver.Script(inference.ClassifierSiteID)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA
	own := domain.Finding{
		ID: "external-cycle-low-value-finding", RunID: p.runID, Source: "codex_local",
		Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "a note for later", RawText: "a note for later", CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("low value cycle findings")),
			Findings: []domain.Finding{own},
		},
	})
	scriptAdjudication(t, driver, deferVerdict(own.ID), declineVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
			len(end.reviewItems), end.readyItem, end.pending)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDispute ||
		item.ID != domain.ItemID("production-external-review-handoff-"+string(p.runID)+"-2") {
		t.Fatalf("cycle item = %s %s", item.Type, item.ID)
	}
	for _, want := range []string{
		"The review policy stops this run at this round",
		`maintainer on review_comment/1: "this leaks the handle"`,
	} {
		if !strings.Contains(item.Reason, want) {
			t.Errorf("cycle item reason lacks %q:\n%s", want, item.Reason)
		}
	}
}

// TestExternalReviewCardOutlivesARestartWithoutAnAdjudicator: the cycle waits
// on its adjudication card, and the daemon comes back with no adjudicator
// configured. The round was already judged, so the card stays the item the
// cycle waits on; nothing tries to raise another item under its identity.
func TestExternalReviewCardOutlivesARestartWithoutAnAdjudicator(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, parkVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}

	p.restartDurableState(t)
	p.judgments = nil
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("pass without an adjudicator = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		card, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		if err != nil {
			return err
		}
		if card.Type != domain.AttentionFindingAdjudication || card.Status != domain.StatusOpen {
			t.Errorf("adjudication card = %#v", card)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if waiting := p.externalReviewEnd(t); len(waiting.reviewItems) != 0 || waiting.readyItem ||
		waiting.pending != 1 {
		t.Fatalf("waiting cycle = %#v", waiting)
	}
}

// TestExternalReviewCycleAfterABaseAdvanceJudgesNothing: the base advanced
// after the run's producer was admitted. The cycle's base is then not the
// base a remediation could be admitted at, so even with an adjudicator the
// cycle judges nothing and ends on a person, on an item that names the
// reviewer's finding (issue #1767 decision 1).
func TestExternalReviewCycleAfterABaseAdvanceJudgesNothing(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	p.publishReady(t)
	head := p.replay.HeadSHA
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("base advance re-entry = %#v, %v", result, err)
	}

	p.listExternalReviewers(t, externalMaintainer())
	finding := p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle after a base advance")
	}
	scriptAdjudication(t, driver, declineVerdict(finding.ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	if stop := p.reentryStopItem(t); !strings.Contains(stop.Reason,
		`maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("stop item reason:\n%s", stop.Reason)
	}
	if end := p.externalReviewEnd(t); end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	if len(adjudicatorRequests(driver)) != 0 || len(p.externalDispositions(t)) != 0 {
		t.Fatal("a cycle at a base other than the admitted one adjudicated")
	}
}

// TestExternalReviewCycleNeverDismissesAnUnratedFindingAlone: most review
// comments carry no severity, and a missing severity reads as high (plan §7).
// Adjudication alone never declines such a finding: the cycle ends on a
// person, and the finding gets no outcome.
func TestExternalReviewCycleNeverDismissesAnUnratedFindingAlone(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	p.publishReady(t)
	head := p.replay.HeadSHA
	p.listExternalReviewers(t, externalMaintainer())
	maintainer := externalMaintainer()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: p.runID, Forge: maintainer.Forge,
		ReviewerAccountID: maintainer.AccountID, ReviewerLogin: maintainer.Login,
		ThreadID: "review_comment/1", HeadSHA: head,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "this leaks the handle", RawText: "this leaks the handle",
		CreatedAt: p.now.UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finding.Severity != "" {
		t.Fatalf("unrated finding severity = %q", finding.Severity)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutExternalFinding(p.ctx, finding)
	}); err != nil {
		t.Fatal(err)
	}
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(finding.ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
			len(end.reviewItems), end.readyItem, end.pending)
	}
	if item := end.reviewItems[0]; item.Type != domain.AttentionReviewDispute ||
		!strings.Contains(item.Reason, "requires a second decision") ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("item %s: %s", item.Type, item.Reason)
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
		t.Fatalf("an unrated finding was dismissed without a person: %#v", dispositions)
	}
}

// TestAdjudicatedExternalReviewCycleConvergesAcrossRestarts interrupts the
// cycle on each side of its two adjudication writes, the judgment and then
// the outcomes, restarts, and requires the same end: each finding's outcome
// once and the run ready again.
func TestAdjudicatedExternalReviewCycleConvergesAcrossRestarts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// at is which firing of the adjudication transition is interrupted.
		at                      int
		recorded, adjudications int
	}{
		// Lost before the judgment is stored, it is asked for again.
		{"before the judgment", 1, 0, 2},
		{"after the judgment", 2, 0, 1},
		{"before the outcomes", 3, 0, 1},
		{"after the outcomes", 4, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			driver := p.configureAdjudicator(t)
			first, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle", "rename this too")
			head := p.replay.HeadSHA
			p.scriptCleanReview(2, p.baseSHA, head)
			verdicts := []adjudicatorVerdict{declineVerdict(findings[0].ID), deferVerdict(findings[1].ID)}
			scriptAdjudication(t, driver, verdicts...)
			fired := 0
			p.workflow = p.newEngine(t, productionCrashSeams{
				transitionHook: func(got engine.DurableTransition, _ engine.DurableTransitionSide) error {
					if got != engine.DurableTransitionFindingAdjudication {
						return nil
					}
					if fired++; fired == tc.at {
						return errors.New("injected process loss")
					}
					return nil
				},
			}, true)
			if _, err := p.reconcileLanes(); err == nil || fired != tc.at {
				t.Fatalf("firing %d did not interrupt the cycle (fired %d): %v", tc.at, fired, err)
			}
			if recorded := p.externalDispositions(t); len(recorded) != tc.recorded {
				t.Fatalf("outcomes at the interruption = %d, want %d", len(recorded), tc.recorded)
			}

			p.restartDurableState(t)
			p.scriptCleanReview(2, p.baseSHA, head)
			scriptAdjudication(t, driver, verdicts...)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			ready := 0
			for pass := 1; pass <= 3 && ready == 0; pass++ {
				result, err := p.reconcileLanes()
				if err != nil {
					t.Fatalf("pass %d after the restart: %v", pass, err)
				}
				ready += result.ReadyItemsCreated
			}
			if ready != 1 {
				t.Fatalf("ready items after the restart = %d, want 1", ready)
			}
			p.assertReentered(t, first, 2, p.baseSHA, head)
			if recorded := p.externalDispositions(t); len(recorded) != 2 {
				t.Fatalf("outcomes after the restart = %#v", recorded)
			}
			if got := len(adjudicatorRequests(driver)); got != tc.adjudications {
				t.Fatalf("adjudicator requests = %d, want %d", got, tc.adjudications)
			}
			if end := p.externalReviewEnd(t); len(end.reviewItems) != 0 || end.pending != 0 {
				t.Fatalf("cycle end = %#v", end)
			}
		})
	}
}
