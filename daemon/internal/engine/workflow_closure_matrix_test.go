package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// Each registry member needs a real supported producer shape. The review and
// proposal readers below also reconstruct their private authority after expiry.
func TestWorkflowClosureAttentionRegistry(t *testing.T) {
	for _, kind := range domain.AllAttentionTypes {
		t.Run(string(kind), func(t *testing.T) {
			st, runID, item := closureMatrixItem(t, kind)
			before := item
			closeWorkflowForTest(t, st, runID, true)
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				current, err := tx.GetAttentionItem(t.Context(), item.ID)
				if err != nil {
					return err
				}
				if before.Status == domain.StatusOpen {
					if current.Status != domain.StatusExpired || current.ItemVersion != before.ItemVersion+1 ||
						current.DecidedAt != nil || current.Reason != before.Reason {
						t.Fatalf("bad retirement: before=%+v after=%+v", before, current)
					}
				} else if current.Status != before.Status || current.ItemVersion != before.ItemVersion {
					t.Fatalf("terminal history changed: %+v", current)
				}
				switch kind {
				case domain.AttentionFindingAdjudication:
					_, _, err = tx.FindingAdjudicationDecision(t.Context(), item.ID)
					if !errors.Is(err, store.ErrTaskWorkflowClosed) {
						t.Fatalf("adjudication decision = %v", err)
					}
				case domain.AttentionReviewDiminishing:
					_, err = tx.ReviewDiminishingDecision(t.Context(), item.ID)
					if !errors.Is(err, store.ErrTaskWorkflowClosed) {
						t.Fatalf("diminishing decision = %v", err)
					}
				case domain.AttentionEffectProposal, domain.AttentionTaskProposal:
					if _, _, err := tx.ProposalForItem(t.Context(), item.ID); err != nil {
						return err
					}
				case domain.AttentionSpecApproval, domain.AttentionExecutionFailure, domain.AttentionAgentQuestion,
					domain.AttentionReviewDispute, domain.AttentionReviewContradiction, domain.AttentionReviewConfiguration,
					domain.AttentionReadyForFinalReview, domain.AttentionPublishBlocked, domain.AttentionSystemHealth,
					domain.AttentionBlocked:
					// These readers use the item and common workflow gate below.
				}
				if err := tx.RequireItemWorkflowOpen(t.Context(), current); !errors.Is(err, store.ErrTaskWorkflowClosed) {
					t.Fatalf("closed decision gate = %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if before.Status == domain.StatusOpen {
				// No new decision or card is authorized while cleanup/replay
				// sees this same episode, even if a caller carries old data.
				if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
					if kind == domain.AttentionTaskProposal || kind == domain.AttentionEffectProposal {
						instance, proposal, err := tx.ProposalForItem(t.Context(), before.ID)
						if err != nil {
							return err
						}
						before.ID += "-late"
						if err := tx.PutAttentionItem(t.Context(), before); err != nil {
							return err
						}
						return tx.BindProposalItem(t.Context(), before.ID, instance.ID, proposal.Digest, nil)
					}
					before.ID += "-late"
					return tx.PutAttentionItem(t.Context(), before)
				}); err == nil {
					t.Fatal("closed producer recreated an open card")
				}
			}
		})
	}
}

func closureMatrixItem(t *testing.T, kind domain.AttentionType) (*store.Store, domain.RunID, domain.AttentionItem) {
	t.Helper()
	ctx := t.Context()
	switch kind {
	case domain.AttentionSpecApproval:
		f, _, _ := discussionAwaitingReply(t, 0)
		item, _ := f.item(t, "spec-approval-implementation-run-1")
		return f.store, "specification-run", item
	case domain.AttentionExecutionFailure:
		f := newFailedImplementationFixture(t, execfake.OutcomeFail, "The provider failed.")
		f.preRecordOutcome(t, domain.ExecutionOutcomeFailed, "The provider failed.")
		reconcileOnce(t, f.engine)
		item, _ := f.item(t, f.failureItemID())
		return f.store, f.run.ID, item
	case domain.AttentionAgentQuestion:
		f := newSpecificationFixture(t, true, 4)
		f.submit(t)
		run, _, item := seedSpecificationQuestion(t, f, f.newEngine(t, f.newDriver(t)))
		return f.store, run.ID, item
	case domain.AttentionReviewDispute, domain.AttentionReviewConfiguration:
		f := newAttentionDiscussionFixture(t, kind, nil)
		// The shared fixture records execution outcome, claims and the
		// recovery binding consumed by the real discussion reader.
		f.submit(t)
		item, err := f.signet.GetAttentionItem(ctx, f.item.ID)
		if err != nil {
			t.Fatal(err)
		}
		return f.store, "run-discussion", item.Item
	case domain.AttentionFindingAdjudication:
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
		item := putFindingAdjudicationRecommendationCase(t, f, nil, nil)
		return f.store, f.task.RunID, item
	case domain.AttentionReviewDiminishing:
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2,
			&domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}, "low", "high")
		record, artifact := seedDiminishingReviewRound(t, f)
		producer := remediationInvocationID(f.task.RunID, record.Round-1)
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutAgentInvocation(ctx, domain.AgentInvocation{ID: producer, InputIDs: []domain.ArtifactID{"summary-input"}}); err != nil {
				return err
			}
			return tx.PutAgentClaims(ctx, producer, []domain.AgentClaim{summaryClaimFixture(producer, "One recurring finding remains.")})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.workflow.executeFindingAdjudication(ctx, f.task, record, artifact, f.baseRoot, f.headRoot); err != nil {
			t.Fatal(err)
		}
		return f.store, f.task.RunID, closureMatrixRead(t, f.store, productionReviewDiminishingItemID(f.task.RunID, record.Round))
	case domain.AttentionReviewContradiction:
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
		failure := domain.ReviewFailure{
			InvocationID: "contradicted-review", RunID: f.task.RunID, Round: 2,
			BaseSHA: f.record.BaseSHA, HeadSHA: f.record.HeadSHA,
			Class: domain.ReviewFailureContradiction, Reason: "Conflicting review evidence.", ObservedAt: f.workflow.now(),
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutReviewFailure(ctx, failure) }); err != nil {
			t.Fatal(err)
		}
		if err := f.workflow.putReviewContradictionAttention(ctx, f.task, failure); err != nil {
			t.Fatal(err)
		}
		return f.store, f.task.RunID, closureMatrixRead(t, f.store, productionReviewItemID(f.task.RunID, 2))
	case domain.AttentionEffectProposal:
		f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
			domain.PolicyFollowUpFilingLabels: "lane:spine, deferral",
		})
		seedFollowUpFilingSubject(t, f, true)
		reconcileFollowUpFiling(t, f, productionReviewPassed)
		state := readFollowUpFilingState(t, f, 1)
		if len(state.cards) != 1 {
			t.Fatalf("filing cards = %d", len(state.cards))
		}
		return f.store, f.task.RunID, state.cards[0]
	case domain.AttentionTaskProposal:
		st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
		request := proposalAdmissionFixture(t, "closure-proposal")
		seedProposalAdmissionPolicy(t, ctx, st, request)
		result, err := newProposalAdmissionEngine(t, st).admitProposalAt(ctx, request, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return st, proposalAdmissionPolicy(t).RunID, result.Item
	case domain.AttentionSystemHealth:
		s := heldWorkDispatchScenario(t)
		backdateRunHold(t, s.store(), s.runID, heldWorkNoticeAfter+time.Minute)
		s.refuse(t)
		items := openHeldWorkNotices(t, s.store())
		if len(items) != 1 {
			t.Fatalf("held-work notices = %d", len(items))
		}
		return s.store(), s.runID, items[0]
	case domain.AttentionReadyForFinalReview:
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
		recipe := domain.Digest("sha256:recipe")
		checkpoint := productionVerificationCheckpoint{
			Imported:      importer.Result{CommitSHA: f.record.HeadSHA},
			Authorization: domain.CandidateAuthorization{Repo: "owner/repo"},
		}
		readiness := productionReadiness{
			verdict: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: adjudicationDigest("a")},
			detail: domain.ReadinessDetail{
				EvaluationSetDigest: adjudicationDigest("a"), CandidateHead: f.record.HeadSHA,
				Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: f.record.BaseSHA},
				Requirements: []domain.ReadinessRequirement{{
					RequirementKey: "clean", CheckClass: domain.CheckClassCleanVerification,
					Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
					ProofRecipeDigest: &recipe,
				}},
			},
		}
		item, err := f.workflow.readyItem(ctx, f.task, checkpoint, publish.Result{PRNumber: 123}, readiness,
			domain.ReviewYieldHistory{Rounds: []domain.ReviewYieldRound{{Round: 1, Outcome: domain.ReviewClean}}, TerminalOutcome: domain.ReviewClean})
		if err != nil {
			t.Fatal(err)
		}
		return f.store, f.task.RunID, closureMatrixPut(t, f.store, item)
	case domain.AttentionPublishBlocked:
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
		item, err := f.workflow.newBlockedItem(ctx, f.task, importer.Result{CommitSHA: f.record.HeadSHA},
			nil, "Publication is held.", []domain.Action{domain.ActionStop}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f.store, f.task.RunID, closureMatrixPut(t, f.store, item)
	case domain.AttentionBlocked:
		// Retired blocked has no supported open producer. Only closed
		// history is legal, and closure must preserve it.
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
		runID := f.task.RunID
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: "legacy-blocked", ProjectID: f.task.ProjectID, Type: kind,
			Subject:  domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
			Priority: domain.PriorityHigh, Reason: "Legacy blocked history.",
			RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 2,
			Status: domain.StatusExpired, InterruptionClass: domain.InterruptionExceptional,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f.store, runID, closureMatrixPut(t, f.store, item)
	}
	t.Fatalf("attention kind %q needs an explicit closure disposition fixture", kind)
	return nil, "", domain.AttentionItem{}
}

func closureMatrixPut(t *testing.T, st *store.Store, item domain.AttentionItem) domain.AttentionItem {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutAttentionItem(t.Context(), item) }); err != nil {
		t.Fatal(err)
	}
	return item
}

func closureMatrixRead(t *testing.T, st *store.Store, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}
