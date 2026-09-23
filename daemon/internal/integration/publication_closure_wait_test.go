package integration_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestHumanGateClosureHoldsDraftUntilDecided proves the post-decision
// convergence trigger end to end: under the human gate, a closable source's PR
// opens as a draft and stays one, with no further forge writes, while the
// effect_proposal item is open; once the item is decided, the next pass writes
// the decided reference and marks the PR ready, then retires the wait.
func TestHumanGateClosureHoldsDraftUntilDecided(t *testing.T) {
	for _, tc := range []struct {
		action   domain.Action
		want     string
		withheld string
	}{
		{action: domain.ActionApprove, want: "Closes #82"},
		{action: domain.ActionDecline, want: "Refs #82", withheld: "Closes #82"},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			p, instance := newHeldClosureHarness(t)

			// An undecided item costs no forge write on a later pass.
			writes := p.forge.writeCountSnapshot()
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if after := p.forge.writeCountSnapshot(); !equalCounts(writes, after) {
				t.Fatalf("undecided pass wrote to the forge: %v -> %v", writes, after)
			}
			if pr := p.forge.pullRequests()[0]; !pr.Draft {
				t.Fatal("undecided pass released the draft hold")
			}

			decideEffectItem(t, p, instance, tc.action)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			released := p.forge.pullRequests()
			if len(released) != 1 || released[0].Draft || !strings.Contains(released[0].Body, tc.want) ||
				(tc.withheld != "" && strings.Contains(released[0].Body, tc.withheld)) {
				t.Fatalf("decided PR = %+v, want ready with %q", released, tc.want)
			}
			if waits := pendingClosureWaits(t, p); len(waits) != 0 {
				t.Fatalf("closure wait still pending after convergence: %d", len(waits))
			}
			writes = p.forge.writeCountSnapshot()
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			if after := p.forge.writeCountSnapshot(); !equalCounts(writes, after) {
				t.Fatalf("retired wait still reconverges: %v -> %v", writes, after)
			}
		})
	}
}

// newHeldClosureHarness publishes a same-repository v2 candidate under the
// human gate and returns it held as a draft with its closure wait armed.
func newHeldClosureHarness(t *testing.T) (*productionPublicationHarness, domain.ProposalInstanceID) {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	p := newAuthoredMetadataHarness(t, metadata, []domain.PolicyKey{
		{Key: "gates.source_issue_closure", Value: "true", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: submissionDigest("closure-gate", "policy-source"),
		}},
	})
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	driver := scriptPublicationSites(t, p,
		inferencefake.Script{Response: inference.Response{Output: []byte(authoredExplainOutput), ComputeUnits: 5}},
		&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}})
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, request := range driver.Requests() {
		if request.SiteID != inference.PublicationAuthorExplainSiteID && request.SiteID != inference.PublicationAuthorProposeSiteID {
			continue
		}
		if request.Fields["source_issue_title"] != fakePublicationIssueTitle ||
			request.Fields["source_issue_body"] != fakePublicationIssueBody {
			t.Fatalf("%s source issue text = (%q, %q), want issue #82 text",
				request.SiteID, request.Fields["source_issue_title"], request.Fields["source_issue_body"])
		}
		seen[request.SiteID] = true
	}
	if !seen[inference.PublicationAuthorExplainSiteID] || !seen[inference.PublicationAuthorProposeSiteID] {
		t.Fatalf("publication author sites called = %v, want explain and propose", seen)
	}
	if p.forge.issueReads != 1 {
		t.Fatalf("source issue reads = %d, want one shared observation", p.forge.issueReads)
	}
	held := p.forge.pullRequests()
	if len(held) != 1 || !held[0].Draft || strings.Contains(held[0].Body, "Closes #82") {
		t.Fatalf("held PR = %+v, want one draft with no Closes", held)
	}
	return p, pendingClosureWaitInstance(t, p)
}

// TestSourceIssueReadFailureFallsBack proves a failed issue read cannot drive
// either author site or approve a closure proposal. The fallback is durable
// when the issue becomes readable on a later pass at the same head and base.
func TestSourceIssueReadFailureFallsBack(t *testing.T) {
	t.Run("API failure", func(t *testing.T) { testSourceIssueReadFailureFallsBack(t, false) })
	t.Run("malformed title", func(t *testing.T) { testSourceIssueReadFailureFallsBack(t, true) })
}

func testSourceIssueReadFailureFallsBack(t *testing.T, malformedTitle bool) {
	t.Helper()
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = "", ""
	metadata.Recipe = "freeside.client-publication/v2"
	metadata.SourceIssue = "https://github.com/" + fakePublicationRepo + "/issues/82"
	p := newAuthoredMetadataHarness(t, metadata, nil)
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	p.forge.failIssueRead = !malformedTitle
	if malformedTitle {
		empty := ""
		p.forge.issueTitle = &empty
	}
	p.forge.requestHook = func(method, path string) bool {
		if method == "GET" && strings.HasSuffix(path, "/issues/82") && p.forge.issueReads == 1 {
			p.forge.failIssueRead = false
			p.forge.issueTitle = nil
			return true
		}
		return false
	}
	driver := scriptPublicationSites(t, p,
		inferencefake.Script{Response: inference.Response{Output: []byte(authoredExplainOutput), ComputeUnits: 5}},
		&inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}})
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	prs := p.forge.pullRequests()
	if len(prs) != 1 || prs[0].Draft || !strings.Contains(prs[0].Body, "Refs #82") ||
		strings.Contains(prs[0].Body, "Closes #82") {
		t.Fatalf("failed issue read PR = %+v, want ready with Refs #82", prs)
	}
	if len(driver.Requests()) != 0 || len(pendingClosureWaits(t, p)) != 0 {
		t.Fatal("failed issue read reached an author site or armed a closure wait")
	}
	if p.forge.issueReads != 1 {
		t.Fatalf("source issue reads = %d, want failed read shared by both sites", p.forge.issueReads)
	}

	p.forge.mu.Lock()
	p.forge.failIssueRead = false
	p.forge.issueTitle = nil
	p.forge.mu.Unlock()
	writes := p.forge.writeCountSnapshot()
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if len(driver.Requests()) != 0 || !equalCounts(writes, p.forge.writeCountSnapshot()) {
		t.Fatal("same candidate retried authoring after its durable fallback")
	}
}

// TestHumanGateClosureWaitNeverWedgesTheLane proves a closure wait cannot stop
// the publication lane: a wait whose pull request concluded retires without
// re-entering the task, a re-entry the repair cannot complete fails loud once
// and retires its wait, and an unreadable wait row is skipped and left pending.
func TestHumanGateClosureWaitNeverWedgesTheLane(t *testing.T) {
	t.Run("concluded", func(t *testing.T) {
		p, _ := newHeldClosureHarness(t)
		// The active-resource reconciler observed a human close the held PR.
		p.forge.mu.Lock()
		p.forge.prs[0].State = "closed"
		p.forge.mu.Unlock()
		if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			ready, err := tx.GetAttentionItemRecord(p.ctx, domain.ProductionReadyItemID(p.runID))
			if err != nil {
				return err
			}
			ready.Status = domain.StatusResolved
			ready.ItemVersion++
			decidedAt := p.now
			ready.DecidedAt = &decidedAt
			return tx.PutAttentionItem(p.ctx, ready)
		}); err != nil {
			t.Fatal(err)
		}
		writes := p.forge.writeCountSnapshot()
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("concluded wait failed the pass: %v", err)
		}
		if after := p.forge.writeCountSnapshot(); !equalCounts(writes, after) {
			t.Fatalf("concluded wait re-entered the task: %v -> %v", writes, after)
		}
		if waits := pendingClosureWaits(t, p); len(waits) != 0 {
			t.Fatalf("concluded wait still pending: %d", len(waits))
		}
	})
	t.Run("failed re-entry fails once", func(t *testing.T) {
		p, instance := newHeldClosureHarness(t)
		decideEffectItem(t, p, instance, domain.ActionApprove)
		// A human merged the held PR before any pass observed it, so the
		// repair meets a closed PR whose body lacks the decided reference.
		p.forge.mu.Lock()
		p.forge.prs[0].State = "closed"
		p.forge.mu.Unlock()
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("unreachable repair did not fail loud")
		}
		if waits := pendingClosureWaits(t, p); len(waits) != 0 {
			t.Fatalf("failed re-entry left its wait pending: %d", len(waits))
		}
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("failed re-entry wedged later passes: %v", err)
		}
	})
	t.Run("unreadable row skipped", func(t *testing.T) {
		p, _ := newHeldClosureHarness(t)
		const badKey = "production-closure-wait/production-publication/from-a-newer-daemon"
		if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(p.ctx, badKey, engine.KindProductionClosureWait,
				[]byte(`{"version":"freeside.production-closure-wait/v9"}`))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("unreadable wait row failed the pass: %v", err)
		}
		if waits := pendingClosureWaits(t, p); len(waits) != 2 {
			t.Fatalf("pending waits = %d, want the held wait and the skipped row", len(waits))
		}
		if pr := p.forge.pullRequests()[0]; !pr.Draft {
			t.Fatal("unreadable wait row released the held PR")
		}
	})
}

func pendingClosureWaits(t *testing.T, p *productionPublicationHarness) []store.QueueEntry {
	t.Helper()
	var waits []store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		waits, err = tx.ListPendingOutbox(p.ctx, engine.KindProductionClosureWait)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return waits
}

func pendingClosureWaitInstance(t *testing.T, p *productionPublicationHarness) domain.ProposalInstanceID {
	t.Helper()
	waits := pendingClosureWaits(t, p)
	if len(waits) != 1 {
		t.Fatalf("pending closure waits = %d, want 1", len(waits))
	}
	var wait struct {
		InstanceID domain.ProposalInstanceID `json:"instance_id"`
	}
	if err := json.Unmarshal(waits[0].Payload, &wait); err != nil || wait.InstanceID == "" {
		t.Fatalf("decode closure wait: %v", err)
	}
	return wait.InstanceID
}

// decideEffectItem submits a paired client's decision on the instance's open
// effect_proposal item.
func decideEffectItem(t *testing.T, p *productionPublicationHarness, instance domain.ProposalInstanceID, action domain.Action) {
	t.Helper()
	var itemID domain.ItemID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, _, open, err := tx.OpenEffectItemForInstance(p.ctx, instance)
		if err == nil && !open {
			err = errors.New("no open effect_proposal item")
		}
		itemID = item.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	const deviceID domain.DeviceID = "closure-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{ID: deviceID, DisplayName: "Closure device", Status: domain.DeviceActive, PairedAt: p.now})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "closure-" + string(action), DeviceID: deviceID, ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: itemID, Action: action, ItemVersion: snapshot.Item.ItemVersion,
			PRHeadSHA: snapshot.Item.PRHeadSHA, ArtifactDigests: snapshot.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("decide %s: %v", action, err)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
