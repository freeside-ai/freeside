package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// externalFilingFixture is the external review cycle with what a follow-up
// filing needs (issue #1787): a registered project, a work-unit declaration,
// and an adjudication of the cycle's first round that defers the admitted
// finding, declines second, and defers third without a disposition row.
// finding has its deferred row and second its declined row.
type externalFilingFixture struct {
	externalCycleFixture
	third       domain.Finding
	artifact    domain.FindingAdjudication
	declaration domain.WorkUnitDeclaration
	policy      domain.ResolvedPolicy
	target      domain.FollowUpFilingTarget
}

func seedExternalFiling(t *testing.T) externalFilingFixture {
	t.Helper()
	ctx := context.Background()
	f := externalFilingFixture{externalCycleFixture: seedExternalCycle(t)}
	f.third = externalFindingBy(t, f.run.ID, reentryHead1, externalReviewerID, externalReviewerLogin,
		"PRRT_third", reentryAt.Add(2*time.Second))
	putExternalFinding(t, f.st, f.third)
	f.artifact = f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
		f.internal.ID: domain.RouteRemediate, f.finding.ID: domain.RouteDefer,
		f.second.ID: domain.RouteDecline, f.third.ID: domain.RouteDefer,
	})
	if err := f.putAdjudication(t, f.artifact); err != nil {
		t.Fatalf("put adjudication: %v", err)
	}
	for _, outcome := range []struct {
		finding     domain.Finding
		disposition domain.ReviewDisposition
	}{{f.finding, domain.ReviewDispositionDeferred}, {f.second, domain.ReviewDispositionDeclined}} {
		if err := f.put(domain.ExternalFindingDisposition{
			FindingID: outcome.finding.ID, RunID: f.run.ID, Round: externalCycleRound,
			Disposition: outcome.disposition, Reason: "decided by adjudication",
			AdjudicationDigest: &f.artifact.Digest, CreatedAt: reentryAt.Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("put %s disposition: %v", outcome.disposition, err)
		}
	}
	project := domain.Project{ID: f.run.ProjectID, Repo: f.binding.Repo, RepositoryID: f.binding.RepositoryID}
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		var err error
		if f.policy, err = tx.GetResolvedPolicy(ctx, f.run.ID); err != nil {
			return err
		}
		f.declaration, err = domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(f.policy),
		}, f.run.ID, f.run.ProjectID, reentryAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(ctx, f.declaration)
	}); err != nil {
		t.Fatalf("seed the filing subject: %v", err)
	}
	var err error
	if f.target, err = domain.DeriveFollowUpFilingTarget(project, f.policy); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f externalFilingFixture) deferredSource(
	finding domain.FindingID, adjudication domain.Digest,
) domain.FollowUpFilingSource {
	return domain.FollowUpFilingSource{
		FindingID: finding, AdjudicationDigest: adjudication, Kind: domain.FollowUpSourceDeferredDisposition,
	}
}

func (f externalFilingFixture) proposal(t *testing.T, source domain.FollowUpFilingSource) domain.EffectProposal {
	t.Helper()
	passed := func(text string) domain.ScreenedIssueText {
		return domain.ScreenedIssueText{
			Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
		}
	}
	proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
		SubjectHandle: domain.OpaqueSubjectHandle(f.declaration.ID), Target: f.target, Source: source,
		Title: passed(`External review finding: "the error return is dropped"`),
		Body:  passed(`Reviewer's words: "the error return is dropped"`),
	}, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func (f externalFilingFixture) allocate(
	t *testing.T, source domain.FollowUpFilingSource, event string,
) (domain.ProposalInstance, error) {
	t.Helper()
	ctx := context.Background()
	proposal := f.proposal(t, source)
	var instance domain.ProposalInstance
	err := f.st.Write(ctx, func(tx *WriteTx) error {
		// The source was searched and found no open issue, so only the
		// source's own backing decides admission. A source its adjudication
		// does not route has no search to bind; admission refuses it alone.
		search, err := domain.NewDuplicateSearch(domain.DuplicateSearchInput{
			RunID: f.run.ID, AdjudicationDigest: source.AdjudicationDigest, FindingID: source.FindingID,
			Kind: source.Kind, CandidateLimit: domain.MaxDuplicateSearchCandidates,
			Repository: domain.FollowUpFilingRepository{Repo: f.target.Repo, RepositoryID: f.target.RepositoryID},
			CreatedAt:  reentryAt.Add(3 * time.Hour),
		})
		if err != nil {
			return err
		}
		if err := tx.PutDuplicateSearch(ctx, search); err != nil && !errors.Is(err, domain.ErrParentKeyMismatch) {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: event},
			"batch-external-filing", proposal, reentryAt.Add(3*time.Hour))
		return err
	})
	return instance, err
}

// rewriteSource replaces a stored instance's source with a self-consistent
// body and content address, past every write gate, so only the gate the read
// re-runs can refuse it.
func (f externalFilingFixture) rewriteSource(
	t *testing.T, instance domain.ProposalInstance, source domain.FollowUpFilingSource,
) {
	t.Helper()
	forged := instance
	forged.Proposal = f.proposal(t, source)
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.st.db.ExecContext(context.Background(),
		`UPDATE effect_proposal_instances SET content_digest = ?, body = ? WHERE instance_id = ?`,
		forged.Proposal.Digest, string(body), instance.ID)
	if err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("tamper changed %d rows, %v", changed, err)
	}
}

func (f externalFilingFixture) read(id domain.ProposalInstanceID) error {
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProposalInstance(ctx, id)
		return err
	})
}

func (f externalFilingFixture) current(source domain.FollowUpFilingSource) error {
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error {
		// The backing half alone: whether the source was searched is the
		// duplicate-search gate's question, tested on its own.
		return tx.gateFollowUpFilingSourceBacked(ctx, f.declaration, source)
	})
}

// appendRevision appends a successor revision of the fixture's adjudication,
// with the same routes, behind the feedback a discussion of its card
// returned: the authority a revision's read re-proves.
func (f externalFilingFixture) appendRevision(t *testing.T) domain.FindingAdjudication {
	t.Helper()
	ctx := context.Background()
	at := reentryAt.Add(4 * time.Hour)
	conversationID := domain.ConversationID("conversation-external-filing")
	question, err := domain.NewMessage("message-question", conversationID, domain.AuthorUser, "reconsider", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	conversation, _ := domain.Conversation{ID: conversationID, Status: domain.ConversationIdle}.Append(question)
	invocation, err := domain.NewAgentInvocation("inv-external-filing-revision", nil, &conversationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, err := conversation.PrefixContent(1)
	if err != nil {
		t.Fatal(err)
	}
	feedback := domain.AdjudicationFeedback{
		InvocationID: invocation.ID, ConversationID: conversationID, ThroughSequence: 1, PrefixDigest: prefix,
	}
	entries := slices.Clone(f.artifact.Entries)
	entries[0].Rationale = "amended after discussion"
	successor, err := domain.NewSuccessorFindingAdjudication(f.artifact, feedback, entries, "", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	findings := map[domain.FindingID]domain.Finding{}
	for _, finding := range []domain.Finding{f.internal, f.finding, f.second, f.third} {
		findings[finding.ID] = finding
	}
	proposals := make([]domain.FindingAdjudicationProposal, 0, len(f.artifact.Entries))
	for _, entry := range f.artifact.Entries {
		finding := findings[entry.FindingID]
		proposals = append(proposals, domain.FindingAdjudicationProposal{
			FindingID: entry.FindingID, FindingMessage: domain.NormalizeFindingMessage(finding.Message),
			FindingLocation: finding.Location, Producer: entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale, Evidence: slices.Clone(entry.Evidence),
			CitedRules: slices.Clone(entry.CitedRules), Assumptions: slices.Clone(entry.Assumptions),
			OpenQuestions: slices.Clone(entry.OpenQuestions), Confidence: entry.Confidence,
			OfferedAlternatives: slices.Clone(entry.OfferedAlternatives),
		})
	}
	runID := f.run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-external-filing-revision", ProjectID: f.run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason: "choose the finding route",
		RequestedDecision: []domain.Action{
			domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop,
		},
		PRHeadSHA: reentryHead1,
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			RunID: runID, Round: f.artifact.Round, AdjudicationDigest: f.artifact.Digest,
			Proposals: proposals,
		},
		ItemVersion: 2, ConversationID: &conversationID,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(domain.ConversationInvocationIntent{
		InvocationID: invocation.ID, ConversationID: conversationID, ItemID: item.ID, ItemVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	replyBody := "reply for " + string(invocation.ID)
	completion, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		Body         string              `json:"body"`
		Attachments  []domain.Digest     `json:"attachments"`
	}{invocation.ID, replyBody, nil})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := domain.NewMessage(domain.MessageID("msg-agent-"+string(invocation.ID)), conversationID, domain.AuthorAgent, replyBody, nil, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	answered, _ := conversation.Append(reply)
	answered.Status = domain.ConversationIdle
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutConversation(ctx, conversation); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		if err := putTestAttentionItem(ctx, tx, &item); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(
			ctx, string(invocation.ID), string(domain.AgentInvocationRequestedKind), intent); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, string(invocation.ID), "agent_completion", completion); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, answered); err != nil {
			return err
		}
		item.ItemVersion++
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, successor)
	}); err != nil {
		t.Fatalf("append the successor revision: %v", err)
	}
	return successor
}

// TestExternalFollowUpFilingSourceAccepted: a deferred external finding's
// source is admitted, reads back, and is current while its deferred row is
// the finding's latest external disposition.
func TestExternalFollowUpFilingSourceAccepted(t *testing.T) {
	t.Parallel()
	f := seedExternalFiling(t)
	source := f.deferredSource(f.finding.ID, f.artifact.Digest)
	instance, err := f.allocate(t, source, "event-accepted")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := f.read(instance.ID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := f.current(source); err != nil {
		t.Fatalf("current gate: %v", err)
	}
}

// TestExternalFollowUpFilingSourceRefused: the source gate refuses each
// external source no deferred external disposition backs, at admission and
// when a stored proposal's source is rewritten to it, and each refusal is a
// source mismatch. The rows it reads decide that the finding is external;
// the proposal says nothing about it.
//
// Two cases reach the external disposition check: a deferral with no row,
// and a row under another revision. The other three are refused earlier, by
// the entry check, because a declined row needs a decline entry and an
// adjudication cannot name a finding its cycle does not admit; they pin that
// the external branch opened no way around it.
func TestExternalFollowUpFilingSourceRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*testing.T, externalFilingFixture) domain.FollowUpFilingSource{
		"no external disposition": func(_ *testing.T, f externalFilingFixture) domain.FollowUpFilingSource {
			return f.deferredSource(f.third.ID, f.artifact.Digest)
		},
		"a declined external disposition": func(_ *testing.T, f externalFilingFixture) domain.FollowUpFilingSource {
			return f.deferredSource(f.second.ID, f.artifact.Digest)
		},
		"a deferred row under another revision": func(t *testing.T, f externalFilingFixture) domain.FollowUpFilingSource {
			return f.deferredSource(f.finding.ID, f.appendRevision(t).Digest)
		},
		"another reviewer's finding": func(t *testing.T, f externalFilingFixture) domain.FollowUpFilingSource {
			stranger := externalFindingBy(t, f.run.ID, reentryHead1, 900, "stranger",
				"PRRT_stranger", reentryAt.Add(3*time.Second))
			putExternalFinding(t, f.st, stranger)
			return f.deferredSource(stranger.ID, f.artifact.Digest)
		},
		"a finding on another head": func(t *testing.T, f externalFilingFixture) domain.FollowUpFilingSource {
			moved := externalFindingBy(t, f.run.ID, reentryHead2, externalReviewerID, externalReviewerLogin,
				"PRRT_moved", reentryAt.Add(3*time.Second))
			putExternalFinding(t, f.st, moved)
			return f.deferredSource(moved.ID, f.artifact.Digest)
		},
	}
	for name, refused := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalFiling(t)
			valid, err := f.allocate(t, f.deferredSource(f.finding.ID, f.artifact.Digest), "event-valid")
			if err != nil {
				t.Fatalf("allocate the valid source: %v", err)
			}
			source := refused(t, f)
			if _, err := f.allocate(t, source, "event-refused"); !errors.Is(err, domain.ErrFollowUpFilingSourceMismatch) {
				t.Fatalf("allocate = %v, want ErrFollowUpFilingSourceMismatch", err)
			}
			f.rewriteSource(t, valid, source)
			if err := f.read(valid.ID); !errors.Is(err, domain.ErrFollowUpFilingSourceMismatch) {
				t.Fatalf("read of a rewritten source = %v, want ErrFollowUpFilingSourceMismatch", err)
			}
		})
	}
}

// TestExternalFollowUpFilingSourceStale: the current gate reports a deferred
// external source stale unless the finding's latest external disposition is
// deferred under the source's adjudication.
func TestExternalFollowUpFilingSourceStale(t *testing.T) {
	t.Parallel()
	f := seedExternalFiling(t)
	revision := f.appendRevision(t)
	if err := f.current(f.deferredSource(f.finding.ID, f.artifact.Digest)); err != nil {
		t.Fatalf("the deferred row's own adjudication: %v", err)
	}
	for name, source := range map[string]domain.FollowUpFilingSource{
		"no external disposition":         f.deferredSource(f.third.ID, f.artifact.Digest),
		"a declined external disposition": f.deferredSource(f.second.ID, f.artifact.Digest),
		"another revision":                f.deferredSource(f.finding.ID, revision.Digest),
	} {
		if err := f.current(source); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
			t.Errorf("%s: current gate = %v, want ErrFollowUpFilingSourceStale", name, err)
		}
	}
}
