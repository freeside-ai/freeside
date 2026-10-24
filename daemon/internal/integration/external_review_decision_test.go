package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (p *productionPublicationHarness) externalDisputeCommand(t *testing.T, item domain.AttentionItem, routes ...signet.AlternativeChoice) signet.ClientCommand {
	t.Helper()
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{ID: "human-device", DisplayName: "Human decision device", Status: domain.DeviceActive, PairedAt: p.now})
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.attention.GetAttentionItem(p.ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return signet.ClientCommand{
		CommandID: "external-human-decision", DeviceID: "human-device", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionAdjudicateFindings, FindingRoutes: routes,
		},
	}
}

func TestExternalReviewDecisionSurvivesRestart(t *testing.T) {
	for _, secondDecision := range []bool{false, true} {
		for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer, domain.RouteRemediate} {
			t.Run(fmt.Sprintf("second=%t/%s", secondDecision, route), func(t *testing.T) {
				p := newProductionPublicationHarness(t, "")
				driver := p.configureAdjudicator(t)
				_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
				finding := findings[0]
				p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
				if secondDecision {
					// An accepted disputed route needs the same second decision
					// as an unrated decline, without changing the finding fixture.
					scriptAdjudication(t, driver, disputeVerdict(finding.ID))
				} else {
					p.judgments = nil
					p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				}
				if _, err := p.reconcileLanes(); err != nil {
					t.Fatal(err)
				}
				end := p.externalReviewEnd(t)
				if end.pending != 1 || len(end.reviewItems) != 1 || end.readyItem {
					t.Fatalf("waiting cycle = %#v", end)
				}
				item := end.reviewItems[0]
				if !item.Offers(domain.ActionAdjudicateFindings) || item.ReviewDispute == nil || len(item.ReviewDispute.Findings) != 1 {
					t.Fatalf("decision surface = %#v", item)
				}
				// An undecided card survives a restart without an adjudicator.
				p.restartDurableState(t)
				p.judgments = nil
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				if _, err := p.reconcileLanes(); err != nil {
					t.Fatal(err)
				}
				if again := p.externalReviewEnd(t); again.pending != 1 || len(again.reviewItems) != 1 || again.reviewItems[0].ItemVersion != item.ItemVersion {
					t.Fatalf("restarted waiting cycle = %#v", again)
				}
				command := p.externalDisputeCommand(t, item, signet.AlternativeChoice{FindingID: finding.ID, Route: route})
				if _, err := p.attention.Submit(p.ctx, command); err != nil {
					t.Fatal(err)
				}
				p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, side engine.DurableTransitionSide) error {
					if transition == engine.DurableTransitionFindingAdjudication && side == engine.DurableTransitionAfter {
						return errors.New("crash after human adjudication")
					}
					return nil
				}}, true)
				if _, err := p.reconcileLanes(); err == nil {
					t.Fatal("human artifact write did not interrupt")
				}
				p.restartDurableState(t)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				if _, err := p.attention.Submit(p.ctx, command); err != nil {
					t.Fatalf("command replay: %v", err)
				}
				if _, err := p.reconcileLanes(); err != nil {
					t.Fatal(err)
				}
				var artifact domain.FindingAdjudication
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					var err error
					artifact, err = tx.GetFindingAdjudicationForRound(p.ctx, p.runID, 2)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				wantRevision := 1
				if secondDecision {
					wantRevision = 2
				}
				if artifact.Revision != wantRevision || artifact.Decision == nil || artifact.Entries[0].Route != route || artifact.Entries[0].Producer != domain.AdjudicationProducerHuman {
					t.Fatalf("human artifact = %#v", artifact)
				}
				if route == domain.RouteRemediate {
					_, input := p.remediateExternalCycle(t, 2, "production change\nfixed the handle\n")
					assertExternalFindingsRemediated(t, input, findings)
				}
				wantDisposition := domain.ReviewDispositionDeclined
				if route == domain.RouteDefer {
					wantDisposition = domain.ReviewDispositionDeferred
				}
				if route == domain.RouteRemediate {
					wantDisposition = domain.ReviewDispositionFixed
				}
				dispositions := p.externalDispositions(t)
				if len(dispositions) != 1 || dispositions[0].Disposition != wantDisposition {
					t.Fatalf("dispositions = %#v", dispositions)
				}
				if route != domain.RouteRemediate && (dispositions[0].AdjudicationDigest == nil || *dispositions[0].AdjudicationDigest != artifact.Digest) {
					t.Fatal("disposition is not bound to the human artifact")
				}
				if route == domain.RouteDecline {
					p.assertExternalReplyQueued(t, finding)
				}
				if ready := p.currentReadyItem(t); ready.Status != domain.StatusOpen || ready.PRHeadSHA != p.replay.HeadSHA {
					t.Fatalf("ready item = %#v", ready)
				}
				if p.startExternalReview(t) {
					t.Fatal("answered finding started another cycle")
				}
			})
		}
	}
}

type externalDecisionIdentity struct{}

func (externalDecisionIdentity) Resolve(context.Context, string) (publish.AppBotIdentity, error) {
	return publish.AppBotIdentity{BotUserID: 7001}, nil
}

func (p *productionPublicationHarness) assertExternalReplyQueued(t *testing.T, finding domain.Finding) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("queue pass made unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(server.Close)
	replier, err := publish.NewExternalReviewReplier(p.store, integrationTokenSource(), server.Client(), server.URL, externalDecisionIdentity{}, func() time.Time { return p.now })
	if err != nil {
		t.Fatal(err)
	}
	if err := replier.Pass(p.ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		queued, err := tx.ListPendingOutbox(p.ctx, publish.ExternalReplyIntentKind)
		if err != nil {
			return err
		}
		if len(queued) != 1 {
			t.Fatalf("reply intents = %d, want one", len(queued))
		}
		var intent struct {
			FindingID domain.FindingID `json:"finding_id"`
			Thread    string           `json:"thread"`
		}
		if err := json.Unmarshal(queued[0].Payload, &intent); err != nil {
			return err
		}
		if intent.FindingID != finding.ID || intent.Thread != finding.External.ThreadID {
			t.Fatalf("reply intent = %#v", intent)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalReviewDecisionFreezesFindingsAndRejectsForeignChoices(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	_, findings := p.startAdjudicatedExternalReview(t, "first finding")
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	item := p.externalReviewEnd(t).reviewItems[0]
	later := p.leaveExternalFinding(t, 2, externalMaintainer(), p.replay.HeadSHA, "arrived after the card")
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	current := p.externalReviewEnd(t).reviewItems[0]
	if current.ItemVersion != item.ItemVersion || !slices.Equal(current.ReviewDispute.FindingIDs, []domain.FindingID{findings[0].ID}) {
		t.Fatalf("late finding changed the card: %#v", current.ReviewDispute)
	}
	valid := p.externalDisputeCommand(t, item, signet.AlternativeChoice{FindingID: findings[0].ID, Route: domain.RouteDecline})
	for _, change := range []func(*signet.ClientCommand){
		func(c *signet.ClientCommand) {
			c.Payload.FindingRoutes = []signet.AlternativeChoice{{FindingID: later.ID, Route: domain.RouteDecline}}
		},
		func(c *signet.ClientCommand) { c.Payload.PRHeadSHA = "foreign-head" },
		func(c *signet.ClientCommand) {
			c.Payload.ItemID = domain.ProductionFindingAdjudicationItemID(p.runID, 3, 1)
		},
	} {
		invalid := valid
		change(&invalid)
		if _, err := p.attention.Submit(p.ctx, invalid); err == nil {
			t.Fatal("accepted foreign decision")
		}
	}
	if _, err := p.attention.Submit(p.ctx, valid); err != nil {
		t.Fatal(err)
	}
	changed := valid
	changed.Payload.FindingRoutes = []signet.AlternativeChoice{{FindingID: findings[0].ID, Route: domain.RouteDefer}}
	if _, err := p.attention.Submit(p.ctx, changed); err == nil {
		t.Fatal("accepted replay with different route")
	}
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if !p.externalReviewEnd(t).readyItem {
		t.Fatal("decline did not restore ready item")
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 1 || dispositions[0].FindingID != findings[0].ID {
		t.Fatalf("dispositions = %#v", dispositions)
	}
	if !p.startExternalReview(t) {
		t.Fatal("unanswered later finding did not start its own cycle")
	}
}

func TestExternalReviewDecisionRefusesFixOutsideScope(t *testing.T) {
	for _, location := range []*domain.FindingLocation{nil, {Path: "outside.go", StartLine: 1, EndLine: 1}} {
		t.Run(fmt.Sprint(location), func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.publishReady(t)
			reviewer := externalMaintainer()
			p.listExternalReviewers(t, reviewer)
			finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
				RunID: p.runID, Forge: reviewer.Forge, ReviewerAccountID: reviewer.AccountID, ReviewerLogin: reviewer.Login,
				ThreadID: "review_comment/1", HeadSHA: p.replay.HeadSHA, Location: location,
				Message: "finding", RawText: "finding", CreatedAt: p.now,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error { return tx.PutExternalFinding(p.ctx, finding) }); err != nil {
				t.Fatal(err)
			}
			if !p.startExternalReview(t) {
				t.Fatal("cycle did not start")
			}
			p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			item := p.externalReviewEnd(t).reviewItems[0]
			if slices.Contains(item.ReviewDispute.Findings[0].OfferedRoutes, domain.RouteRemediate) {
				t.Fatal("out-of-scope fix offered")
			}
			command := p.externalDisputeCommand(t, item, signet.AlternativeChoice{FindingID: finding.ID, Route: domain.RouteRemediate})
			if _, err := p.attention.Submit(p.ctx, command); err == nil {
				t.Fatal("out-of-scope fix accepted")
			}
			command.Payload.Action, command.Payload.FindingRoutes = domain.ActionStop, nil
			if _, err := p.attention.Submit(p.ctx, command); err != nil {
				t.Fatal(err)
			}
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if end := p.externalReviewEnd(t); end.pending != 0 || end.readyItem || len(end.reviewItems) != 0 {
				t.Fatalf("stop ending = %#v", end)
			}
		})
	}
}
