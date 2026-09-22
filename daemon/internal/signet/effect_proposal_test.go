package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// closureFixture seeds one source_issue_closure proposal instance (verified
// provenance by default) with all durable rows its gate re-derives from, then
// exposes the signet service so a test can open the effect_proposal item and
// decide it end to end.
type closureFixture struct {
	fixture
	service  *signet.Service
	instance domain.ProposalInstance
	handle   domain.OpaqueSubjectHandle
	merge    domain.ProspectiveMerge
}

const (
	testProjectRepo         = "owner/repo"
	testProjectRepositoryID = 123
	testClosureIssueNumber  = 7
)

func testDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

func newClosureFixture(
	t *testing.T, resolves bool, origin domain.ClosureFlagOrigin,
) closureFixture {
	t.Helper()
	ctx := context.Background()
	base := newRunFixture(t) // no seeded ready item; we supply the closure graph
	now := *base.now

	policy, err := domain.NewResolvedPolicy("closure-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: testDigest("a"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))

	// Seed the run's task with a same-repository issue-subject source, so the
	// closure gate re-derives a verified determination. daemon_fallback is
	// modelled by the proposal origin, which is what drops approve_with_changes;
	// the provenance stays verified here.
	proposedTarget := domain.IssueSubjectRef{
		Repo: testProjectRepo, RepositoryID: testProjectRepositoryID, IssueNumber: testClosureIssueNumber,
	}
	source := domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &proposedTarget,
	}
	closable := domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceVerified,
		Repo: testProjectRepo, RepositoryID: testProjectRepositoryID, IssueNumber: testClosureIssueNumber,
	}

	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle:  handle,
		Source:         closable,
		ProposedTarget: proposedTarget,
		Origin:         origin,
		Resolves:       resolves,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	var instance domain.ProposalInstance
	if err := base.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: testProjectRepo, RepositoryID: testProjectRepositoryID,
		}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, "proj-1", source)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "proj-1", TaskID: task.ID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "proj-1", now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "closure-event"},
			"batch-closure", proposal, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	service := signet.NewService(base.store, signet.WithClock(func() time.Time { return *base.now }))
	merge := domain.ProspectiveMerge{
		PublicationIdentity: testDigest("b"),
		CandidateHeadSHA:    "head-aaa",
		BaseRef:             "main",
		BaseSHA:             "base-000",
	}
	return closureFixture{fixture: base, service: service, instance: instance, handle: handle, merge: merge}
}

func (f closureFixture) open(t *testing.T, merge domain.ProspectiveMerge) domain.AttentionItem {
	t.Helper()
	item, err := f.service.OpenEffectProposalItem(context.Background(), f.instance.ID, merge)
	if err != nil {
		t.Fatalf("OpenEffectProposalItem: %v", err)
	}
	return item
}

func (f closureFixture) openNotice(t *testing.T, merge domain.ProspectiveMerge) domain.AttentionItem {
	t.Helper()
	item, err := f.service.OpenEffectProposalNotice(context.Background(), f.instance.ID, merge)
	if err != nil {
		t.Fatalf("OpenEffectProposalNotice: %v", err)
	}
	return item
}

func (f closureFixture) decision(item domain.AttentionItem, id string, action domain.Action) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: id, DeviceID: f.device.ID, ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: action, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		},
	}
}

func (f closureFixture) readItem(t *testing.T, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(context.Background(), id)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItem %q: %v", id, err)
	}
	return item
}

func (f closureFixture) approval(t *testing.T) *domain.ClosureApproval {
	t.Helper()
	var approval *domain.ClosureApproval
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		approval, err = tx.ClosureApprovalForInstance(context.Background(), f.instance.ID)
		return err
	}); err != nil {
		t.Fatalf("ClosureApprovalForInstance: %v", err)
	}
	return approval
}

// TestOpenEffectProposalItemIdempotentAndSupersedes proves the open call yields
// one item for a merge, returns the same item when re-opened with that merge,
// and supersedes it for a changed merge.
func TestOpenEffectProposalItemIdempotentAndSupersedes(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	first := f.open(t, f.merge)
	if first.Type != domain.AttentionEffectProposal || first.Status != domain.StatusOpen {
		t.Fatalf("first item = type %q status %q", first.Type, first.Status)
	}
	if first.PRHeadSHA != f.merge.CandidateHeadSHA {
		t.Fatalf("item head = %q, want %q", first.PRHeadSHA, f.merge.CandidateHeadSHA)
	}
	again := f.open(t, f.merge)
	if again.ID != first.ID {
		t.Fatalf("re-open with same merge yielded %q, want %q", again.ID, first.ID)
	}

	changed := f.merge
	changed.CandidateHeadSHA = "head-bbb"
	second := f.open(t, changed)
	if second.ID == first.ID {
		t.Fatal("changed merge must open a new item id")
	}
	if superseded := f.readItem(t, first.ID); superseded.Status != domain.StatusSuperseded {
		t.Fatalf("first item status = %q, want superseded", superseded.Status)
	}
	if second.Status != domain.StatusOpen || second.PRHeadSHA != "head-bbb" {
		t.Fatalf("second item = status %q head %q", second.Status, second.PRHeadSHA)
	}
}

// TestGenericIntakeRefusesEffectProposal proves only the opener creates an
// effect_proposal item; generic intake refuses the type.
func TestGenericIntakeRefusesEffectProposal(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	item.ID = "hand-made-effect-proposal"
	if err := f.service.PutItem(context.Background(), item); err == nil {
		t.Fatal("generic intake accepted an effect_proposal item")
	}
}

// TestEffectProposalApproveRecordsBindingAndAuthorizes proves approve concludes
// the item, records the decision, and yields a ClosureApproval whose five values
// match the merge; AuthorizesClose accepts against that merge and rejects a
// changed one.
func TestEffectProposalApproveRecordsBindingAndAuthorizes(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	if _, err := f.service.Submit(context.Background(), f.decision(item, "approve-1", domain.ActionApprove)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decided := f.readItem(t, item.ID); decided.Status != domain.StatusResolved || decided.DecidedAt == nil {
		t.Fatalf("item = status %q decided %v", decided.Status, decided.DecidedAt)
	}
	approval := f.approval(t)
	if approval == nil {
		t.Fatal("approve recorded no ClosureApproval")
	}
	if approval.ProposalDigest != f.instance.Proposal.Digest ||
		approval.PublicationIdentity != f.merge.PublicationIdentity ||
		approval.CandidateHeadSHA != f.merge.CandidateHeadSHA ||
		approval.BaseRef != f.merge.BaseRef || approval.BaseSHA != f.merge.BaseSHA {
		t.Fatalf("approval = %+v, want it bound to the opened merge and proposal", approval)
	}
	if !approval.AuthorizesClose(f.instance.Proposal, f.merge) {
		t.Fatal("AuthorizesClose rejected the merge it was approved against")
	}
	changed := f.merge
	changed.BaseSHA = "base-999"
	if approval.AuthorizesClose(f.instance.Proposal, changed) {
		t.Fatal("AuthorizesClose accepted a changed merge")
	}
}

// TestEffectProposalDeclineAndSnoozeLeaveUnapproved proves neither decline nor
// snooze produces a ClosureApproval.
func TestEffectProposalDeclineAndSnoozeLeaveUnapproved(t *testing.T) {
	t.Run("decline", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		item := f.open(t, f.merge)
		if _, err := f.service.Submit(context.Background(), f.decision(item, "decline-1", domain.ActionDecline)); err != nil {
			t.Fatalf("decline: %v", err)
		}
		if decided := f.readItem(t, item.ID); decided.Status != domain.StatusDismissed {
			t.Fatalf("item status = %q, want dismissed", decided.Status)
		}
		if approval := f.approval(t); approval != nil {
			t.Fatalf("decline produced an approval: %+v", approval)
		}
	})
	t.Run("snooze", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		item := f.open(t, f.merge)
		until := (*f.now).Add(time.Hour).UTC()
		cmd := f.decision(item, "snooze-1", domain.ActionSnooze)
		cmd.Payload.SnoozeUntil = &until
		if _, err := f.service.Submit(context.Background(), cmd); err != nil {
			t.Fatalf("snooze: %v", err)
		}
		if decided := f.readItem(t, item.ID); decided.Status != domain.StatusOpen {
			t.Fatalf("snoozed item status = %q, want open", decided.Status)
		}
		if approval := f.approval(t); approval != nil {
			t.Fatalf("snooze produced an approval: %+v", approval)
		}
	})
}

// TestEffectProposalApproveWithChangesFlipsResolves proves approve_with_changes
// revises the proposal (only resolves changes), supersedes the item, and binds
// the approval to the revised digest.
func TestEffectProposalApproveWithChangesFlipsResolves(t *testing.T) {
	f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	cmd := f.decision(item, "approve-changes-1", domain.ActionApproveWithChanges)
	resolves := true
	cmd.Payload.EffectProposalRevision = &signet.EffectProposalRevisionInput{Resolves: resolves}
	if _, err := f.service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("approve_with_changes: %v", err)
	}
	if superseded := f.readItem(t, item.ID); superseded.Status != domain.StatusSuperseded {
		t.Fatalf("original item status = %q, want superseded", superseded.Status)
	}
	approval := f.approval(t)
	if approval == nil {
		t.Fatal("approve_with_changes recorded no approval")
	}
	if approval.ProposalDigest == f.instance.Proposal.Digest {
		t.Fatal("approval still bound to the prior digest, not the revised one")
	}
	if approval.CandidateHeadSHA != f.merge.CandidateHeadSHA || approval.BaseSHA != f.merge.BaseSHA {
		t.Fatalf("approval merge changed: %+v", approval)
	}
}

// TestEffectProposalApproveWithChangesRejectsUnchanged proves a revision that
// leaves resolves unchanged is rejected because it changes no digest.
func TestEffectProposalApproveWithChangesRejectsUnchanged(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	cmd := f.decision(item, "approve-changes-noop", domain.ActionApproveWithChanges)
	cmd.Payload.EffectProposalRevision = &signet.EffectProposalRevisionInput{Resolves: true}
	if _, err := f.service.Submit(context.Background(), cmd); err == nil {
		t.Fatal("approve_with_changes accepted an unchanged resolves")
	}
}

// TestEffectProposalFallbackOmitsApproveWithChanges proves a daemon_fallback
// proposal offers no approve_with_changes: the action is not in the offered set,
// so a decision carrying it is refused.
func TestEffectProposalFallbackOmitsApproveWithChanges(t *testing.T) {
	f := newClosureFixture(t, false, domain.ClosureFlagOriginDaemonFallback)
	item := f.open(t, f.merge)
	for _, a := range item.RequestedDecision {
		if a == domain.ActionApproveWithChanges {
			t.Fatal("daemon_fallback item offered approve_with_changes")
		}
	}
	cmd := f.decision(item, "fallback-changes", domain.ActionApproveWithChanges)
	cmd.Payload.EffectProposalRevision = &signet.EffectProposalRevisionInput{Resolves: true}
	if _, err := f.service.Submit(context.Background(), cmd); err == nil {
		t.Fatal("daemon_fallback item accepted approve_with_changes")
	}
}

// TestEffectProposalStaleAndWrongHeadRejected proves a decision against a
// superseded item is refused, and a command carrying a different pr_head_sha is
// refused by the binding check.
func TestEffectProposalStaleAndWrongHeadRejected(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	stale := f.open(t, f.merge)
	changed := f.merge
	changed.CandidateHeadSHA = "head-ccc"
	f.open(t, changed) // supersedes `stale`

	if _, err := f.service.Submit(context.Background(), f.decision(stale, "stale-approve", domain.ActionApprove)); err == nil {
		t.Fatal("approve accepted against a superseded item")
	}

	fresh := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := fresh.open(t, fresh.merge)
	wrong := fresh.decision(item, "wrong-head", domain.ActionApprove)
	wrong.Payload.PRHeadSHA = "head-not-the-item"
	if _, err := fresh.service.Submit(context.Background(), wrong); err == nil {
		t.Fatal("approve accepted a command with a mismatched pr_head_sha")
	}
}

// TestEffectProposalStartActionRejected proves a task-proposal action (start) is
// not offered on a closure item and is refused.
func TestEffectProposalStartActionRejected(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	if _, err := f.service.Submit(context.Background(), f.decision(item, "wrong-family", domain.ActionStart)); err == nil {
		t.Fatal("closure item accepted a start action")
	}
}

// TestOpenEffectProposalNoticeExceptionalAndActions proves the fallback notice
// opens as an exceptional interruption offering approve / decline / snooze (no
// approve_with_changes, a fallback cannot be revised to resolve).
func TestOpenEffectProposalNoticeExceptionalAndActions(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginDaemonFallback)
	item := f.openNotice(t, f.merge)
	if item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusOpen {
		t.Fatalf("notice = type %q status %q", item.Type, item.Status)
	}
	if item.InterruptionClass != domain.InterruptionExceptional {
		t.Fatalf("notice interruption class = %q, want exceptional", item.InterruptionClass)
	}
	got := map[domain.Action]bool{}
	for _, a := range item.RequestedDecision {
		got[a] = true
	}
	want := map[domain.Action]bool{
		domain.ActionApprove: true, domain.ActionDecline: true, domain.ActionSnooze: true,
	}
	if len(got) != len(want) {
		t.Fatalf("notice actions = %v, want %v", item.RequestedDecision, want)
	}
	for a := range want {
		if !got[a] {
			t.Fatalf("notice actions = %v, want %v", item.RequestedDecision, want)
		}
	}
}

// TestOpenEffectProposalNoticeApprovalNeverCloses proves approving the notice
// records a human decision whose reconstructed approval never authorizes a
// close: the fallback proposal does not resolve, so AuthorizesClose is false.
func TestOpenEffectProposalNoticeApprovalNeverCloses(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginDaemonFallback)
	item := f.openNotice(t, f.merge)
	if _, err := f.service.Submit(context.Background(), f.decision(item, "notice-approve", domain.ActionApprove)); err != nil {
		t.Fatalf("approve notice: %v", err)
	}
	approval := f.approval(t)
	if approval == nil {
		t.Fatal("approving the notice recorded no approval binding")
	}
	if approval.AuthorizesClose(f.instance.Proposal, f.merge) {
		t.Fatal("a notice approval authorized a close")
	}
}

// TestOpenEffectProposalNoticeSupersedesOnMergeChange proves a changed merge
// supersedes the open notice and opens a fresh one, exactly as the gate item.
func TestOpenEffectProposalNoticeSupersedesOnMergeChange(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginDaemonFallback)
	first := f.openNotice(t, f.merge)
	changed := f.merge
	changed.CandidateHeadSHA = "head-bbb"
	second := f.openNotice(t, changed)
	if second.ID == first.ID {
		t.Fatal("changed merge must open a new notice id")
	}
	if superseded := f.readItem(t, first.ID); superseded.Status != domain.StatusSuperseded {
		t.Fatalf("first notice status = %q, want superseded", superseded.Status)
	}
}

// TestOpenEffectProposalNoticeRefusesProposeSite proves the notice is only for a
// daemon_fallback proposal; a propose_site instance takes the gate card instead.
func TestOpenEffectProposalNoticeRefusesProposeSite(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	if _, err := f.service.OpenEffectProposalNotice(context.Background(), f.instance.ID, f.merge); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("notice on propose_site error = %v, want ErrEffectProposalInconsistent", err)
	}
}

// TestEffectProposalNoticeFactsServed proves the facts route serves an open
// fallback notice the same as a gate item: it is a visible effect_proposal.
func TestEffectProposalNoticeFactsServed(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginDaemonFallback)
	item := f.openNotice(t, f.merge)
	facts, err := f.service.GetEffectProposalFacts(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts(notice): %v", err)
	}
	if facts.EffectKind != domain.EffectSourceIssueClosure || facts.SourceIssueClosure == nil {
		t.Fatalf("notice facts = %#v, want a source_issue_closure arm", facts)
	}
	if facts.SourceIssueClosure.Origin != domain.ClosureFlagOriginDaemonFallback ||
		facts.SourceIssueClosure.Resolves {
		t.Fatalf("notice closure facts = %#v, want daemon_fallback and resolves=false", facts.SourceIssueClosure)
	}
}

// TestEffectProposalFactsMatchItemAndCarryNoAuthority proves the facts read
// projects the open closure item's version tuple, digest, resolved target, and
// bound merge, with no supersedes and no subject handle on the wire.
func TestEffectProposalFactsMatchItemAndCarryNoAuthority(t *testing.T) {
	f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	snapshot, err := f.service.GetAttentionItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := f.service.GetEffectProposalFacts(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts: %v", err)
	}
	if facts.AsOfRevision != snapshot.AsOfRevision || facts.EntityVersion != snapshot.EntityVersion ||
		facts.ItemVersion != item.ItemVersion || facts.ProposalDigest != f.instance.Proposal.Digest ||
		facts.EffectKind != domain.EffectSourceIssueClosure || facts.Supersedes != nil {
		t.Fatalf("facts = %#v, want the open item's tuple and initial projection", facts)
	}
	closure := facts.SourceIssueClosure
	if closure == nil {
		t.Fatal("facts carried no source_issue_closure arm")
	}
	want := f.instance.Proposal.ClosureProposal
	if closure.Target != want.Target || closure.Resolves != want.Resolves ||
		closure.Provenance != want.Provenance || closure.Origin != want.Origin {
		t.Fatalf("closure facts = %#v, want %#v", closure, want)
	}
	if closure.Merge.PublicationIdentity != f.merge.PublicationIdentity ||
		closure.Merge.CandidateHeadSHA != f.merge.CandidateHeadSHA ||
		closure.Merge.BaseRef != f.merge.BaseRef || closure.Merge.BaseSHA != f.merge.BaseSHA {
		t.Fatalf("merge facts = %#v, want the opened merge %#v", closure.Merge, f.merge)
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

// TestEffectProposalFactsHideSnoozedAndWrongType proves the facts route hides a
// snoozed closure item and a task_proposal item, matching the item reads'
// visibility rules.
func TestEffectProposalFactsHideSnoozedAndWrongType(t *testing.T) {
	t.Run("snoozed", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		item := f.open(t, f.merge)
		until := (*f.now).Add(time.Hour).UTC()
		cmd := f.decision(item, "snooze-facts", domain.ActionSnooze)
		cmd.Payload.SnoozeUntil = &until
		if _, err := f.service.Submit(context.Background(), cmd); err != nil {
			t.Fatalf("snooze: %v", err)
		}
		if _, err := f.service.GetEffectProposalFacts(context.Background(), item.ID); !errors.Is(err, signet.ErrProposalSnoozed) {
			t.Fatalf("snoozed facts error = %v, want ErrProposalSnoozed", err)
		}
	})
	t.Run("task proposal", func(t *testing.T) {
		f := newProposalDecisionFixture(t)
		if _, err := f.service.GetEffectProposalFacts(context.Background(), f.item.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("task_proposal facts error = %v, want ErrNotFound", err)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		if _, err := f.service.GetEffectProposalFacts(context.Background(), "item-does-not-exist"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown facts error = %v, want ErrNotFound", err)
		}
	})
}

// TestEffectProposalRevisionFactsCarrySupersedes proves the replacement item's
// facts after approve_with_changes carry the revised digest and a supersedes
// with the prior digest and prior resolves.
func TestEffectProposalRevisionFactsCarrySupersedes(t *testing.T) {
	f := newClosureFixture(t, false, domain.ClosureFlagOriginProposeSite)
	item := f.open(t, f.merge)
	cmd := f.decision(item, "approve-changes-facts", domain.ActionApproveWithChanges)
	cmd.Payload.EffectProposalRevision = &signet.EffectProposalRevisionInput{Resolves: true}
	if _, err := f.service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("approve_with_changes: %v", err)
	}
	replacementID := domain.ItemID(string(f.instance.ID) + "/revision/" + cmd.CommandID)
	facts, err := f.service.GetEffectProposalFacts(context.Background(), replacementID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts(replacement): %v", err)
	}
	if facts.SourceIssueClosure == nil || !facts.SourceIssueClosure.Resolves {
		t.Fatalf("revised facts = %#v, want resolves=true", facts.SourceIssueClosure)
	}
	if facts.Supersedes == nil || facts.Supersedes.ProposalDigest != f.instance.Proposal.Digest ||
		facts.Supersedes.SourceIssueClosure == nil || facts.Supersedes.SourceIssueClosure.Resolves != false ||
		facts.ProposalDigest == facts.Supersedes.ProposalDigest {
		t.Fatalf("supersedes = %#v, want the prior digest and prior resolves=false", facts.Supersedes)
	}
}
