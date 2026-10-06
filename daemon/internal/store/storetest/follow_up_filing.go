package storetest

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// filingSeedAt anchors every instant a seeded follow-up filing records. The
// ledger re-gates a filing against its own rows, never the wall clock, so a
// fixed anchor keeps the seed deterministic.
var filingSeedAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// filingSeedBotUserID stands in for the App bot account a seeded filing's
// candidate search listed under. No seed reads it back against a live App.
const filingSeedBotUserID int64 = 4100

// filingSeedDigest is a well-formed digest standing in for content the seed
// never stores (a spec, an instruction set, a provenance source).
func filingSeedDigest(name string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte("follow-up-filing-seed/" + name)))
}

// FollowUpFiling names where a seeded follow-up filing lands: the project
// that proposes it and the repository its derived target names. Distinct
// project ids give distinct runs, so one store can hold several, in the same
// repository or in different ones.
type FollowUpFiling struct {
	ProjectID    domain.ProjectID
	Repo         string
	RepositoryID int64
}

// DispatchFollowUpFiling seeds an approved follow-up filing through the
// store's public API and records its intent and one started create attempt,
// with no response. That is the state a crash between sending the create and
// reading its answer leaves: the daemon may have created an issue in the
// repository and no ledger row says which. It returns the filing's proposal
// instance.
//
// A repository holds one outstanding intent at a time, so a second seed in
// the same repository needs the first resolved (LedgerFiledFollowUpIssue).
func DispatchFollowUpFiling(t testing.TB, st *store.Store, filing FollowUpFiling) domain.ProposalInstanceID {
	t.Helper()
	instance := approveFollowUpFiling(t, st, filing)
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		opened := filingSeedAt.Add(24 * time.Hour)
		if _, err := tx.OpenFollowUpFilingIntent(t.Context(), instance, opened); err != nil {
			return err
		}
		// An attempt needs the candidate search recorded first; it found nothing.
		if _, err := tx.RecordFollowUpFilingPreDispatch(
			t.Context(), instance, nil, filingSeedBotUserID, opened.Add(time.Minute)); err != nil {
			return err
		}
		_, err := tx.StartFollowUpFilingAttempt(t.Context(), instance, opened.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("dispatch follow-up filing for %q: %v", filing.ProjectID, err)
	}
	return instance
}

// LedgerFiledFollowUpIssue is DispatchFollowUpFiling whose create returned
// issueNumber: the intent resolves as ledgered and the filing ledger holds
// the row that makes issueNumber a daemon-filed issue in the repository.
func LedgerFiledFollowUpIssue(t testing.TB, st *store.Store, filing FollowUpFiling, issueNumber int) {
	t.Helper()
	instance := DispatchFollowUpFiling(t, st, filing)
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, err := tx.LedgerFollowUpFilingSuccess(
			t.Context(), instance, issueNumber, filingSeedAt.Add(24*time.Hour+3*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("ledger follow-up issue %d for %q: %v", issueNumber, filing.ProjectID, err)
	}
}

// approveFollowUpFiling seeds everything the filing gate re-derives (the
// project, its run and resolved policy with the filing keys, the declaration,
// and a review round whose adjudication defers one finding with its
// disposition recorded), admits the filing proposal for that finding, opens
// its card, and records the approve decision.
func approveFollowUpFiling(t testing.TB, st *store.Store, filing FollowUpFiling) domain.ProposalInstanceID {
	t.Helper()
	ctx := t.Context()
	fail := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed follow-up filing for %q: %s: %v", filing.ProjectID, step, err)
		}
	}
	runID := domain.RunID(string(filing.ProjectID) + "-run")
	provenance := domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: filingSeedDigest("provenance")}
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{
		{Key: "paths", Value: "daemon/", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingLabels, Value: "deferral", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingMilestone, Value: "1B", Provenance: provenance},
	})
	fail("resolve policy", err)
	project := domain.Project{ID: filing.ProjectID, Repo: filing.Repo, RepositoryID: filing.RepositoryID}
	target, err := domain.DeriveFollowUpFilingTarget(project, policy)
	fail("derive target", err)

	specDigest, instructionDigest := filingSeedDigest("spec"), filingSeedDigest("instruction")
	finding := domain.Finding{
		ID: domain.FindingID("finding-deferred-" + string(filing.ProjectID)), RunID: runID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "unbounded retry", RawText: "unbounded retry", CreatedAt: filingSeedAt,
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + string(runID) + "-1"),
		RunID:        runID, Round: 1, Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: filingSeedDigest("configuration"), InstructionDigest: instructionDigest,
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head-1", CompletedAt: filingSeedAt,
		CompletionEvidence: filingSeedDigest("completion"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	fail("review record", err)
	entry, err := domain.NewModelAdjudicationEntry(
		finding.ID, domain.GoalAdjacent, nil, domain.RouteDefer, domain.ConfidenceHigh,
		"routes the finding", nil, nil, nil, nil, nil)
	fail("adjudication entry", err)
	adjudication, err := domain.NewFindingAdjudication(
		runID, 1, specDigest, instructionDigest, policy.Digest,
		[]domain.FindingAdjudicationEntry{entry}, "", filingSeedAt.Add(time.Minute))
	fail("adjudication", err)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, runID, filing.ProjectID, filingSeedAt.Add(-time.Hour))
	fail("declaration", err)
	proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID)),
		Target:        target,
		Source: domain.FollowUpFilingSource{
			FindingID: finding.ID, AdjudicationDigest: adjudication.Digest,
			Kind: domain.FollowUpSourceDeferredDisposition,
		},
		Title: passedIssueText("Bound the retry budget in the sync client"),
		Body:  passedIssueText("The sync client retries without a budget.\n\nDeferred from review round 1."),
	}, policy)
	fail("proposal", err)

	var instance domain.ProposalInstance
	fail("store rows", st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: filing.ProjectID, SpecDigest: specDigest, PolicyDigest: policy.Digest,
			Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: finding.ID, RunID: runID, Round: 1,
			Disposition: domain.ReviewDispositionDeferred, Reason: "tracked as a follow-up",
			AdjudicationDigest: adjudication.Digest, CreatedAt: filingSeedAt.Add(2 * time.Minute),
		}); err != nil {
			return err
		}
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{
				Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-" + string(filing.ProjectID),
			},
			domain.ProposalBatchID("batch-"+string(filing.ProjectID)), proposal, filingSeedAt.Add(time.Hour))
		if err != nil {
			return err
		}
		command, err := openFollowUpFilingCard(t, tx, filing.ProjectID, instance)
		if err != nil {
			return err
		}
		return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, command.Action,
			&instance.Proposal.Digest, filingSeedAt.Add(6*time.Hour))
	}))
	return instance.ID
}

// openFollowUpFilingCard opens the effect_proposal item over an admitted
// filing instance and stores the approve command against it, the two rows an
// approve decision must name.
func openFollowUpFilingCard(
	t testing.TB, tx *store.WriteTx, projectID domain.ProjectID, instance domain.ProposalInstance,
) (domain.Command, error) {
	ctx := t.Context()
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		return domain.Command{}, err
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return domain.Command{}, err
	}
	subject := domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)}
	names, err := tx.DisplayNamesFor(ctx, projectID, subject)
	if err != nil {
		return domain.Command{}, err
	}
	createdAt := filingSeedAt.Add(time.Hour)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(string(instance.ID) + "/effect"), ProjectID: projectID, CreatedAt: &createdAt,
		Subject: subject, DisplayNames: names,
		Type: domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
		Reason:            "Decide whether to file the follow-up issue",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
		EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.Command{}, err
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.Command{}, err
	}
	if err := tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, nil); err != nil {
		return domain.Command{}, err
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-approve-" + string(instance.ID), DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionApprove,
	})
	if err != nil {
		return domain.Command{}, err
	}
	return command, tx.PutCommand(ctx, command)
}

func passedIssueText(text string) domain.ScreenedIssueText {
	return domain.ScreenedIssueText{
		Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
	}
}
