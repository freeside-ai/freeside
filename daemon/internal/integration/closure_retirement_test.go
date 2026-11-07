package integration_test

import (
	"encoding/json"
	"fmt"
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
func newClosureGateHarness(t *testing.T) *productionPublicationHarness {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	// The production manifests let the handoff verify the merge into an advanced tip.
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`), productionBaseManifests())
	keys := []domain.PolicyKey{{
		Key: "gates.source_issue_closure", Value: "true", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: submissionDigest("closure-gate", "policy-source"),
		},
	}}
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

// TestHandoffOpensTheClosureCardAtTheTip: a target that advanced before the
// first publication's handoff never re-enters, so no card is retired: the
// publication opens its one card binding the head's merge into the tip.
func TestHandoffOpensTheClosureCardAtTheTip(t *testing.T) {
	t.Parallel()
	p := newClosureGateHarness(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	open, closed := p.closureCards(t)
	for pass := 0; pass < 6 && len(open) == 0; pass++ {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		open, closed = p.closureCards(t)
	}
	if len(open) != 1 || len(closed) != 0 {
		t.Fatalf("closure cards after the handoff = %d open, %d closed, want 1 and 0", len(open), len(closed))
	}
	merge := p.openClosureMerge(t, domain.ProductionPublicationInvocationID(p.runID))
	if merge.BaseSHA != advanced || merge.CandidateHeadSHA != p.replay.HeadSHA {
		t.Fatalf("open closure card binds %+v, want head %s into %s", merge, p.replay.HeadSHA, advanced)
	}
}

// TestHandoffConflictOpensNoClosureCard: the handoff runs before the
// publication proposes its close, so a head that conflicts with the tip goes
// to remediation with no card opened against a merge that does not exist.
func TestHandoffConflictOpensNoClosureCard(t *testing.T) {
	t.Parallel()
	p := newClosureGateHarness(t)
	p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	p.startHandoffConflictRemediation(t, 1)
	if open, closed := p.closureCards(t); len(open) != 0 || len(closed) != 0 {
		t.Fatalf("closure cards after the conflicting handoff = %d open, %d closed, want none", len(open), len(closed))
	}
}
