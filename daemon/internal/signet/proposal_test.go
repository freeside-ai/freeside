package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type proposalDecisionFixture struct {
	fixture
	service  *signet.Service
	instance domain.ProposalInstance
	item     domain.AttentionItem
	policy   domain.ResolvedPolicy
	handles  []domain.OpaqueSubjectHandle
}

func newProposalDecisionFixture(t *testing.T) proposalDecisionFixture {
	t.Helper()
	ctx := context.Background()
	base := newFixture(t)
	policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
		Key: "rein", Value: "loose", Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}, {
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handles := []domain.OpaqueSubjectHandle{domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))}
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement, ExpectedCostUnits: 10,
		Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	var instance domain.ProposalInstance
	var item domain.AttentionItem
	err = base.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "proj-1", (*base.now).Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "admit-1"},
			"batch-1", proposal, *base.now)
		if err != nil {
			return err
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(instance.ID), ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-1"},
			Type:    domain.AttentionRunProposal, Priority: domain.PriorityNormal,
			Reason:            "start this work",
			RequestedDecision: []domain.Action{domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest)
	})
	if err != nil {
		t.Fatal(err)
	}
	service := signet.NewService(base.store,
		signet.WithClock(func() time.Time { return *base.now }),
	)
	return proposalDecisionFixture{fixture: base, service: service, instance: instance, item: item, policy: policy, handles: handles}
}

func TestPutItemRejectsUnboundRunProposal(t *testing.T) {
	ctx := context.Background()
	f := newProposalDecisionFixture(t)
	before := f.revision(t)

	unbound := f.item
	unbound.ID = "unbound-run-proposal"
	if err := f.service.PutItem(ctx, unbound); !errors.Is(err, signet.ErrProposalAdmissionRequired) {
		t.Fatalf("PutItem error = %v, want ErrProposalAdmissionRequired", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected proposal moved revision %d to %d", before, after)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, unbound.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAttentionItem after rejection = %v, want ErrNotFound", err)
	}
}

func (f proposalDecisionFixture) proposalCommand(id string, action domain.Action) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: id, DeviceID: f.device.ID, ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, Action: action, ItemVersion: f.item.ItemVersion,
			ArtifactDigests: f.item.ArtifactDigests,
		},
	}
}

func TestRunProposalStartAndDeclineConcludePerInstance(t *testing.T) {
	for _, action := range []domain.Action{domain.ActionStart, domain.ActionDecline} {
		t.Run(string(action), func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			if _, err := f.service.Submit(context.Background(), f.proposalCommand("command-"+string(action), action)); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
				item, err := tx.GetAttentionItem(context.Background(), f.item.ID)
				if err != nil {
					return err
				}
				want := domain.StatusResolved
				if action == domain.ActionDecline {
					want = domain.StatusDismissed
				}
				if item.Status != want || item.DecidedAt == nil {
					t.Fatalf("item = status %q decided %v", item.Status, item.DecidedAt)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStartRunProposalUnattendedReportsStart proves the daemon-attributed start
// records a decision and reports started only when the card is open, so the
// label-intake caller launches only a start it actually made. A card an operator
// declined between the caller's gate and this call reports started=false and
// keeps its decline: an explicit non-start decision can never become a run. An
// already-started card likewise reports no second start (convergence-launch is
// the reconciler's already-decided path, not a second decision here).
func TestStartRunProposalUnattendedReportsStart(t *testing.T) {
	ctx := context.Background()
	readStatus := func(t *testing.T, f proposalDecisionFixture) domain.ItemStatus {
		t.Helper()
		var item domain.AttentionItem
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			item, err = tx.GetAttentionItem(ctx, f.item.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return item.Status
	}

	t.Run("open card starts", func(t *testing.T) {
		f := newProposalDecisionFixture(t)
		started, err := f.service.StartRunProposalUnattended(ctx, f.item.ID, "cmd-start-1")
		if err != nil {
			t.Fatal(err)
		}
		if !started {
			t.Fatal("an open run_proposal must report started")
		}
		if status := readStatus(t, f); status != domain.StatusResolved {
			t.Fatalf("item status = %q, want resolved", status)
		}
	})

	t.Run("declined card does not start", func(t *testing.T) {
		f := newProposalDecisionFixture(t)
		if _, err := f.service.Submit(ctx, f.proposalCommand("decline-1", domain.ActionDecline)); err != nil {
			t.Fatal(err)
		}
		started, err := f.service.StartRunProposalUnattended(ctx, f.item.ID, "cmd-start-2")
		if err != nil {
			t.Fatal(err)
		}
		if started {
			t.Fatal("a declined run_proposal must not report started")
		}
		if status := readStatus(t, f); status != domain.StatusDismissed {
			t.Fatalf("item status = %q, want the decline preserved (dismissed)", status)
		}
	})

	t.Run("already started card does not re-decide", func(t *testing.T) {
		f := newProposalDecisionFixture(t)
		if started, err := f.service.StartRunProposalUnattended(ctx, f.item.ID, "cmd-a"); err != nil || !started {
			t.Fatalf("first start: started=%v err=%v", started, err)
		}
		started, err := f.service.StartRunProposalUnattended(ctx, f.item.ID, "cmd-b")
		if err != nil {
			t.Fatal(err)
		}
		if started {
			t.Fatal("a resolved card must not report a second start")
		}
	})
}

func TestRunProposalSnoozeAdvancesVersionAndRetryConverges(t *testing.T) {
	f := newProposalDecisionFixture(t)
	command := f.proposalCommand("command-snooze", domain.ActionSnooze)
	until := (*f.now).Add(time.Hour)
	command.Payload.SnoozeUntil = &until
	first, err := f.service.Submit(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.service.Submit(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Record.CommandID != first.Record.CommandID || retry.Revision != first.Revision {
		t.Fatalf("retry = %#v, first %#v", retry, first)
	}
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(context.Background(), f.item.ID)
		if err == nil && (item.Status != domain.StatusOpen || item.ItemVersion != 2) {
			t.Fatalf("snoozed item = status %q version %d", item.Status, item.ItemVersion)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	contains := func(items []signet.AttentionItemSnapshot) bool {
		for _, item := range items {
			if item.Item.ID == f.item.ID {
				return true
			}
		}
		return false
	}
	bootstrap, err := f.service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contains(bootstrap.AttentionItems) {
		t.Fatal("snoozed proposal remained in bootstrap")
	}
	listed, err := f.service.ListAttentionItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contains(listed) {
		t.Fatal("snoozed proposal remained in list")
	}
	if _, err := f.service.GetAttentionItem(context.Background(), f.item.ID); !errors.Is(err, signet.ErrProposalSnoozed) {
		t.Fatalf("get snoozed proposal error = %v", err)
	}
	start := f.proposalCommand("command-start-while-snoozed", domain.ActionStart)
	start.ExpectedEntityVersion = 2
	start.Payload.ItemVersion = 2
	if _, err := f.service.Submit(context.Background(), start); !errors.Is(err, signet.ErrProposalSnoozed) {
		t.Fatalf("submit snoozed proposal error = %v", err)
	}
	deliveryService := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: "http://127.0.0.1:1", TopicKey: []byte("0123456789abcdef0123456789abcdef"),
			ClickBaseURL: "http://127.0.0.1:2",
		}),
	)
	if _, err := deliveryService.SubmitDelivery(context.Background(), f.item.ID, f.device.ID); !errors.Is(err, signet.ErrProposalSnoozed) {
		t.Fatalf("deliver snoozed proposal error = %v", err)
	}
	// An already in-flight delivery may still recompute timing while the item
	// is hidden. Expiry must advance from that newer version, not require the
	// snooze transition to remain the last item write.
	if err := f.store.Write(context.Background(), func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(context.Background(), f.item.ID)
		if err != nil {
			return err
		}
		item.ItemVersion++
		return tx.PutAttentionItem(context.Background(), item)
	}); err != nil {
		t.Fatal(err)
	}
	beforeExpiry, err := f.store.ServerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*f.now = (*f.now).Add(2 * time.Hour)
	revision, err := f.service.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revision.Revision != beforeExpiry.Revision+1 {
		t.Fatalf("expiry revision = %d, want exactly %d", revision.Revision, beforeExpiry.Revision+1)
	}
	listed, err = f.service.ListAttentionItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(listed) {
		t.Fatal("expired snooze did not restore proposal")
	}
	start.ExpectedEntityVersion = 4
	start.Payload.ItemVersion = 4
	if _, err := f.service.Submit(context.Background(), start); err != nil {
		t.Fatalf("submit after snooze expiry: %v", err)
	}
}

func TestRunProposalStartWithChangesBindsExactRevisedDigest(t *testing.T) {
	f := newProposalDecisionFixture(t)
	revision := signet.RunProposalRevisionInput{
		Intent:            domain.RunProposalIntentImplement,
		ExpectedCostUnits: 5, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}
	command := f.proposalCommand("command-revise", domain.ActionStartWithChanges)
	command.Payload.RunProposalRevision = &revision
	if _, err := f.service.Submit(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	parameters := domain.RunProposalParameters{
		SubjectHandle: f.handles[0], Intent: revision.Intent,
		ExpectedCostUnits: revision.ExpectedCostUnits, Scope: revision.Scope,
	}
	revised, err := domain.NewEffectProposal(domain.EffectRunProposal, parameters, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	replacementID := domain.ItemID(string(f.instance.ID) + "/revision/" + command.CommandID)
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		original, err := tx.GetAttentionItem(context.Background(), f.item.ID)
		if err != nil {
			return err
		}
		replacement, err := tx.GetAttentionItem(context.Background(), replacementID)
		if err != nil {
			return err
		}
		if original.Status != domain.StatusSuperseded || original.ItemVersion != 2 {
			t.Fatalf("original = status %q version %d", original.Status, original.ItemVersion)
		}
		if replacement.Status != domain.StatusResolved || replacement.ItemVersion != 2 ||
			len(replacement.ArtifactDigests) != 1 || replacement.ArtifactDigests[0] != revised.Digest || replacement.DecidedAt == nil {
			t.Fatalf("replacement = %#v, want exact revised digest %q", replacement, revised.Digest)
		}
		_, got, err := tx.ProposalForItem(context.Background(), replacement.ID)
		if err == nil && got.Digest != revised.Digest {
			t.Fatalf("rendered proposal digest = %q, want %q", got.Digest, revised.Digest)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunProposalRevisionRemainsReadableAfterReleasedSnooze(t *testing.T) {
	f := newProposalDecisionFixture(t)
	until := (*f.now).Add(time.Hour)
	snooze := f.proposalCommand("command-snooze-before-revision", domain.ActionSnooze)
	snooze.Payload.SnoozeUntil = &until
	if _, err := f.service.Submit(context.Background(), snooze); err != nil {
		t.Fatal(err)
	}
	*f.now = until.Add(time.Minute)
	if _, err := f.service.Revision(context.Background()); err != nil {
		t.Fatal(err)
	}

	revision := signet.RunProposalRevisionInput{
		Intent: domain.RunProposalIntentImplement, ExpectedCostUnits: 5,
		Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}
	command := f.proposalCommand("command-revise-after-snooze", domain.ActionStartWithChanges)
	command.ExpectedEntityVersion = 3
	command.Payload.ItemVersion = 3
	command.Payload.RunProposalRevision = &revision
	if _, err := f.service.Submit(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	replacementID := domain.ItemID(string(f.instance.ID) + "/revision/" + command.CommandID)
	if _, err := f.service.GetAttentionItem(context.Background(), replacementID); err != nil {
		t.Fatalf("get replacement after released snooze: %v", err)
	}
	if _, err := f.service.GetRunProposalFacts(context.Background(), replacementID); err != nil {
		t.Fatalf("get replacement facts after released snooze: %v", err)
	}
	items, err := f.service.ListAttentionItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Item.ID == replacementID {
			return
		}
	}
	t.Fatalf("replacement %q missing from list after released snooze", replacementID)
}

func TestRunProposalStartWithChangesRejectsNoOpAsRequestError(t *testing.T) {
	f := newProposalDecisionFixture(t)
	before, err := f.store.ServerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	command := f.proposalCommand("command-no-op-revision", domain.ActionStartWithChanges)
	command.Payload.RunProposalRevision = &signet.RunProposalRevisionInput{
		Intent:            f.instance.Proposal.RunProposal.Intent,
		ExpectedCostUnits: f.instance.Proposal.RunProposal.ExpectedCostUnits,
		Scope:             f.instance.Proposal.RunProposal.Scope,
	}
	if _, err := f.service.Submit(context.Background(), command); !errors.Is(err, signet.ErrInvalidProposalDecisionPayload) {
		t.Fatalf("no-op revision error = %v, want ErrInvalidProposalDecisionPayload", err)
	}
	item, err := f.service.GetAttentionItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen || item.Item.ItemVersion != f.item.ItemVersion {
		t.Fatalf("item after no-op revision = %#v, want unchanged open item", item.Item)
	}
	after, err := f.store.ServerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision after no-op = %d, want %d", after.Revision, before.Revision)
	}
	err = f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		_, err := tx.GetCommand(context.Background(), command.CommandID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no-op command persistence error = %v, want ErrNotFound", err)
	}
}

func TestRunProposalStartWithChangesRejectsDeclaredPathCountMismatch(t *testing.T) {
	f := newProposalDecisionFixture(t)
	before := f.revision(t)
	revision := signet.RunProposalRevisionInput{
		Intent: domain.RunProposalIntentImplement, ExpectedCostUnits: 20,
		Scope: f.instance.Proposal.RunProposal.Scope,
	}
	revision.Scope.DeclaredPathCount++
	command := f.proposalCommand("command-scope-mismatch", domain.ActionStartWithChanges)
	command.Payload.RunProposalRevision = &revision
	if _, err := f.service.Submit(context.Background(), command); !errors.Is(err, signet.ErrInvalidProposalDecisionPayload) {
		t.Fatalf("scope mismatch error = %v, want ErrInvalidProposalDecisionPayload", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("scope mismatch revision = %d, want %d", after, before)
	}
	item, err := f.service.GetAttentionItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen || item.Item.ItemVersion != f.item.ItemVersion {
		t.Fatalf("item after scope mismatch = %#v, want unchanged open item", item.Item)
	}
}

func TestRunProposalRevisionFactsExposeExactAuthenticatedDiff(t *testing.T) {
	f := newProposalDecisionFixture(t)
	revision := signet.RunProposalRevisionInput{
		Intent: domain.RunProposalIntentImplement, ExpectedCostUnits: 25,
		Scope: domain.RunProposalScope{
			ComponentCount: 2, DeclaredPathCount: 1, TouchesControlPlane: true,
		},
	}
	command := f.proposalCommand("command-revision-facts", domain.ActionStartWithChanges)
	command.Payload.RunProposalRevision = &revision
	if _, err := f.service.Submit(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	replacementID := domain.ItemID(string(f.instance.ID) + "/revision/" + command.CommandID)
	facts, err := f.service.GetRunProposalFacts(context.Background(), replacementID)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Supersedes == nil || facts.Supersedes.ProposalDigest != f.instance.Proposal.Digest ||
		facts.Supersedes.ExpectedCostUnits != 10 || facts.Supersedes.Scope != f.instance.Proposal.RunProposal.Scope ||
		facts.ExpectedCostUnits != revision.ExpectedCostUnits || facts.Scope != revision.Scope ||
		facts.ProposalDigest == facts.Supersedes.ProposalDigest {
		t.Fatalf("revision facts = %#v, want exact prior/current diff", facts)
	}
}

func TestRunProposalDecisionUsesStoreResolvedSubject(t *testing.T) {
	f := newProposalDecisionFixture(t)
	service := signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }))
	_, err := service.Submit(context.Background(), f.proposalCommand("command-start", domain.ActionStart))
	if err != nil {
		t.Fatalf("store-resolved proposal decision: %v", err)
	}
}

func TestRunProposalFactsAreDigestAndVersionBoundWithoutAuthorityFields(t *testing.T) {
	f := newProposalDecisionFixture(t)
	facts, err := f.service.GetRunProposalFacts(context.Background(), f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ItemVersion != f.item.ItemVersion || facts.ProposalDigest != f.instance.Proposal.Digest ||
		facts.Intent != f.instance.Proposal.RunProposal.Intent ||
		facts.ExpectedCostUnits != f.instance.Proposal.RunProposal.ExpectedCostUnits ||
		facts.Scope != f.instance.Proposal.RunProposal.Scope || facts.Supersedes != nil {
		t.Fatalf("facts = %#v, want exact initial proposal projection", facts)
	}
	body, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"subject_handle", "resolved_policy", "policy_run"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("facts leaked authority field %q: %s", forbidden, body)
		}
	}
}
