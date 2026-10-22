package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// newClosureGateHarness is a candidate for issue #82 under the closure human
// gate, its export recorded and nothing published yet. Each publication cycle
// asks both author sites once, so the sites are scripted for two cycles.
func newClosureGateHarness(t *testing.T, extraKeys ...domain.PolicyKey) *productionPublicationHarness {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	// The production manifests let the re-entry re-verify on the advanced base.
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`), productionBaseManifests())
	keys := append([]domain.PolicyKey{{
		Key: "gates.source_issue_closure", Value: "true", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: submissionDigest("closure-gate", "policy-source"),
		},
	}}, extraKeys...)
	p := newAuthoredMetadataHarnessFromBase(t, h, metadata, keys, nil)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	explain := inferencefake.Script{Response: inference.Response{Output: []byte(authoredExplainOutput), ComputeUnits: 5}}
	propose := inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}}
	driver := scriptPublicationSites(t, p, explain, &propose)
	driver.Script(inference.PublicationAuthorExplainSiteID, explain, explain)
	driver.Script(inference.PublicationAuthorProposeSiteID, propose, propose)
	p.startAndRecordExport(t)
	return p
}

// closureCards returns the effect_proposal cards split into open and closed.
// A card's subject is its proposal batch, not the run; the harness holds one
// run, and closure is its only effect proposal.
func (p *productionPublicationHarness) closureCards(t *testing.T) (open, closed []domain.AttentionItem) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(p.ctx)
		for _, row := range items {
			item := row.Value
			if item.Type != domain.AttentionEffectProposal {
				continue
			}
			if item.Status == domain.StatusOpen {
				open = append(open, item)
			} else {
				closed = append(closed, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return open, closed
}

// openClosureMerge is the prospective merge the publication's open closure
// card binds.
func (p *productionPublicationHarness) openClosureMerge(t *testing.T, publication domain.InvocationID) domain.ProspectiveMerge {
	t.Helper()
	var merge *domain.ProspectiveMerge
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, "production-closure/"+string(p.runID)+"/"+string(publication))
		if err != nil {
			return err
		}
		var checkpoint struct {
			InstanceID domain.ProposalInstanceID `json:"instance_id"`
		}
		if err := json.Unmarshal(entry.Payload, &checkpoint); err != nil {
			return err
		}
		_, bound, found, err := tx.OpenEffectItemForInstance(p.ctx, checkpoint.InstanceID)
		if err == nil && (!found || bound == nil) {
			err = fmt.Errorf("publication %s has no open closure card", publication)
		}
		merge = bound
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return *merge
}

// requireRetiredClosure checks the run's only closed closure card was
// superseded by the daemon with a reason containing want.
func requireRetiredClosure(t *testing.T, closed []domain.AttentionItem, want string) {
	t.Helper()
	if len(closed) != 1 {
		t.Fatalf("closed closure cards = %d, want 1", len(closed))
	}
	conclusion := closed[0].Conclusion
	if closed[0].Status != domain.StatusSuperseded || conclusion == nil ||
		conclusion.Cause != domain.ConcludedBySupersession || !strings.Contains(conclusion.Reason, want) {
		t.Fatalf("retired closure card = %s %+v, want superseded naming %q", closed[0].Status, conclusion, want)
	}
}

// TestBaseAdvanceRetiresTheReplacedClosureCard: the first publication opens
// its closure card at the admitted base, the publisher refuses the advanced
// base, and the re-entry seal retires that card. The re-entry then opens its
// own card at the advanced base, so one card is open, naming the current
// merge.
func TestBaseAdvanceRetiresTheReplacedClosureCard(t *testing.T) {
	t.Parallel()
	p := newClosureGateHarness(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)

	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	successor := p.unpublishedReentry(t, advanced)
	open, closed := p.closureCards(t)
	if len(open) != 0 {
		t.Fatalf("open closure cards after the re-entry seal = %d, want 0", len(open))
	}
	requireRetiredClosure(t, closed, "target base advanced")

	for pass := 0; pass < 6 && len(open) == 0; pass++ {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		open, closed = p.closureCards(t)
	}
	if len(open) != 1 || len(closed) != 1 {
		t.Fatalf("closure cards after the re-entry published = %d open, %d closed, want 1 and 1",
			len(open), len(closed))
	}
	merge := p.openClosureMerge(t, successor.PublicationID())
	if merge.BaseSHA != advanced || merge.CandidateHeadSHA != p.replay.HeadSHA {
		t.Fatalf("open closure card binds %+v, want head %s into %s", merge, p.replay.HeadSHA, advanced)
	}
}

// TestBlockedPublicationRetiresItsClosureCard: a run with no review round
// left cannot re-enter after the base advance, so its publication completes
// blocked and the card it opened is retired with the blocked reason.
func TestBlockedPublicationRetiresItsClosureCard(t *testing.T) {
	t.Parallel()
	p := newClosureGateHarness(t, domain.PolicyKey{
		Key: "review.hard_round_limit", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	})
	p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")

	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.BlockedItemsCreated != 1 {
		t.Fatalf("round-limit result = %#v, want one blocked item", result)
	}
	open, closed := p.closureCards(t)
	if len(open) != 0 {
		t.Fatalf("open closure cards after the blocked publication = %d, want 0", len(open))
	}
	requireRetiredClosure(t, closed, "the publication was blocked")
}
