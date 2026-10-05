package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	filingDeferredFinding = domain.FindingID("finding-deferred")
	filingParkedFinding   = domain.FindingID("finding-parked")
	filingDeclinedFinding = domain.FindingID("finding-declined")
	filingPendingFinding  = domain.FindingID("finding-pending")
)

var filingAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// filingFixture is one work unit with everything the filing gate re-derives:
// a registered project, a resolved policy naming labels and a milestone, the
// run and its declaration, and a review round whose adjudication defers one
// finding, parks one as separate work, declines one, and routes a fourth to
// defer without its disposition row yet recorded.
type filingFixture struct {
	projectID    domain.ProjectID
	runID        domain.RunID
	policy       domain.ResolvedPolicy
	handle       domain.OpaqueSubjectHandle
	target       domain.FollowUpFilingTarget
	adjudication domain.FindingAdjudication
	findings     []domain.Finding
}

func filingPolicy(t *testing.T, runID domain.RunID, extra map[string]string) domain.ResolvedPolicy {
	t.Helper()
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
	}
	keys := []domain.PolicyKey{{Key: "paths", Value: "daemon/", Provenance: provenance}}
	for key, value := range extra {
		keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
	}
	policy, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func modelEntry(
	t *testing.T, id domain.FindingID, goal domain.GoalRelationship,
	compat *domain.ProposedCompatibility, route domain.AdjudicationRoute,
) domain.FindingAdjudicationEntry {
	t.Helper()
	entry, err := domain.NewModelAdjudicationEntry(
		id, goal, compat, route, domain.ConfidenceHigh, "routes the finding", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("adjudication entry %q: %v", id, err)
	}
	return entry
}

// seedFiling registers one project's filing fixture in st. Distinct project
// ids give distinct runs, so two fixtures can share a store.
func seedFiling(t *testing.T, ctx context.Context, st *store.Store, projectID domain.ProjectID) filingFixture {
	t.Helper()
	runID := domain.RunID(string(projectID) + "-run")
	policy := filingPolicy(t, runID, map[string]string{
		domain.PolicyFollowUpFilingLabels:    "lane:spine, deferral",
		domain.PolicyFollowUpFilingMilestone: "1B",
	})
	project := domain.Project{ID: projectID, Repo: "owner/repo", RepositoryID: 123}
	target, err := domain.DeriveFollowUpFilingTarget(project, policy)
	if err != nil {
		t.Fatal(err)
	}
	findings := []domain.Finding{
		adjudicationFinding(filingDeferredFinding+domain.FindingID("-"+string(projectID)), runID, "daemon/a.go", filingAt),
		adjudicationFinding(filingParkedFinding+domain.FindingID("-"+string(projectID)), runID, "daemon/b.go", filingAt),
		adjudicationFinding(filingDeclinedFinding+domain.FindingID("-"+string(projectID)), runID, "daemon/c.go", filingAt),
		adjudicationFinding(filingPendingFinding+domain.FindingID("-"+string(projectID)), runID, "daemon/d.go", filingAt),
	}
	separate := domain.ProposedSeparateWork
	adjudication, err := domain.NewFindingAdjudication(
		runID, 1, adjSpecDigest, adjInstructionDigest, policy.Digest,
		[]domain.FindingAdjudicationEntry{
			modelEntry(t, findings[0].ID, domain.GoalAdjacent, nil, domain.RouteDefer),
			modelEntry(t, findings[1].ID, domain.GoalRequired, &separate, domain.RouteParkSeparateWork),
			modelEntry(t, findings[2].ID, domain.GoalContradictory, nil, domain.RouteDecline),
			modelEntry(t, findings[3].ID, domain.GoalAdjacent, nil, domain.RouteDefer),
		}, "", filingAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ids := []domain.FindingID{findings[0].ID, findings[1].ID, findings[2].ID, findings[3].ID}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: projectID, SpecDigest: adjSpecDigest, PolicyDigest: policy.Digest,
			Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, runID, projectID, filingAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, adjudicationReviewRecord(t, runID, 1, ids, filingAt), findings); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: findings[0].ID, RunID: runID, Round: 1,
			Disposition: domain.ReviewDispositionDeferred, Reason: "tracked as a follow-up",
			AdjudicationDigest: adjudication.Digest, CreatedAt: filingAt.Add(2 * time.Minute),
		})
	}); err != nil {
		t.Fatalf("seed filing fixture: %v", err)
	}
	return filingFixture{
		projectID: projectID, runID: runID, policy: policy,
		handle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID)),
		target: target, adjudication: adjudication, findings: findings,
	}
}

func (f filingFixture) deferredSource() domain.FollowUpFilingSource {
	return domain.FollowUpFilingSource{
		FindingID: f.findings[0].ID, AdjudicationDigest: f.adjudication.Digest,
		Kind: domain.FollowUpSourceDeferredDisposition,
	}
}

func (f filingFixture) parkedSource() domain.FollowUpFilingSource {
	return domain.FollowUpFilingSource{
		FindingID: f.findings[1].ID, AdjudicationDigest: f.adjudication.Digest,
		Kind: domain.FollowUpSourceSeparateWorkVerdict,
	}
}

func passedIssueText(text string) domain.ScreenedIssueText {
	return domain.ScreenedIssueText{
		Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
	}
}

func (f filingFixture) input(source domain.FollowUpFilingSource) domain.FollowUpFilingInput {
	return domain.FollowUpFilingInput{
		SubjectHandle: f.handle, Target: f.target, Source: source,
		Title: passedIssueText("Bound the retry budget in the sync client"),
		Body:  passedIssueText("The sync client retries without a budget.\n\nDeferred from review round 1."),
	}
}

func (f filingFixture) proposal(t *testing.T, input domain.FollowUpFilingInput) domain.EffectProposal {
	t.Helper()
	proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, input, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func allocateFiling(
	ctx context.Context, st *store.Store, proposal domain.EffectProposal, event string,
) (domain.ProposalInstance, error) {
	var instance domain.ProposalInstance
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: event},
			"batch-filing", proposal, filingAt.Add(time.Hour))
		return err
	})
	return instance, err
}

func getFiling(ctx context.Context, st *store.Store, id domain.ProposalInstanceID) (domain.ProposalInstance, error) {
	var instance domain.ProposalInstance
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		instance, err = tx.GetProposalInstance(ctx, id)
		return err
	})
	return instance, err
}

func TestFollowUpFilingAdmissionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")

	for name, source := range map[string]domain.FollowUpFilingSource{
		"deferred": fx.deferredSource(), "separate-work": fx.parkedSource(),
	} {
		proposal := fx.proposal(t, fx.input(source))
		instance, err := allocateFiling(ctx, st, proposal, "event-"+name)
		if err != nil {
			t.Fatalf("%s: allocate: %v", name, err)
		}
		// The proposal-instance id, not the proposal digest, is the effect
		// identity: a retried admission converges on the same instance.
		replay, err := allocateFiling(ctx, st, proposal, "event-"+name)
		if err != nil || replay.ID != instance.ID {
			t.Fatalf("%s: replay = %q, %v, want instance %q", name, replay.ID, err, instance.ID)
		}
		got, err := getFiling(ctx, st, instance.ID)
		if err != nil {
			t.Fatalf("%s: get: %v", name, err)
		}
		filing := got.Proposal.FilingProposal
		if got.Proposal.Kind != domain.EffectFollowUpFiling || filing == nil || filing.Source != source {
			t.Fatalf("%s: reconstructed = %#v", name, got.Proposal)
		}
		if filing.Repository.RepositoryID != 123 || !slices.Equal(filing.Labels, []string{"deferral", "lane:spine"}) ||
			filing.Milestone == nil || *filing.Milestone != "1B" {
			t.Fatalf("%s: target = %#v", name, filing)
		}
	}
}

// TestFollowUpFilingAdmissionRejectsUntrustedProposals covers the admission
// gate: a self-consistent proposal (valid digest, passed verdicts) is refused
// when its target differs from the current derivation, its text fails the
// screen it claims to have passed, or its source link names rows that do not
// back it.
func TestFollowUpFilingAdmissionRejectsUntrustedProposals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	other := seedFiling(t, ctx, st, "project-other")

	cases := []struct {
		name   string
		mutate func(*domain.FollowUpFilingInput)
		want   error
	}{
		{"another repository", func(in *domain.FollowUpFilingInput) {
			in.Target.Repo, in.Target.RepositoryID = "attacker/repo", 999
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"labels policy does not name", func(in *domain.FollowUpFilingInput) {
			in.Target.Labels = []string{"deferral", "lane:spine", "priority:critical"}
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"no labels", func(in *domain.FollowUpFilingInput) {
			in.Target.Labels = nil
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"another milestone", func(in *domain.FollowUpFilingInput) {
			in.Target.Milestone = "2A"
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"no milestone", func(in *domain.FollowUpFilingInput) {
			in.Target.Milestone = ""
		}, domain.ErrFollowUpFilingTargetMismatch},

		{"body with a close directive", func(in *domain.FollowUpFilingInput) {
			in.Body = passedIssueText("Fixes #12 once this lands.")
		}, domain.ErrFollowUpFilingTextRejected},
		{"body with a mention", func(in *domain.FollowUpFilingInput) {
			in.Body = passedIssueText("Ask @octocat about the budget.")
		}, domain.ErrFollowUpFilingTextRejected},
		{"body with a command line", func(in *domain.FollowUpFilingInput) {
			in.Body = passedIssueText("Context first.\n/assign me")
		}, domain.ErrFollowUpFilingTextRejected},
		{"body with the reserved marker", func(in *domain.FollowUpFilingInput) {
			in.Body = passedIssueText("<!-- freeside:unknown -->")
		}, domain.ErrFollowUpFilingTextRejected},
		{"title with a mention", func(in *domain.FollowUpFilingInput) {
			in.Title = passedIssueText("Ping @octocat about retries")
		}, domain.ErrFollowUpFilingTextRejected},

		{"finding the artifact declines", func(in *domain.FollowUpFilingInput) {
			in.Source.FindingID = fx.findings[2].ID
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"deferred route with no disposition row", func(in *domain.FollowUpFilingInput) {
			in.Source.FindingID = fx.findings[3].ID
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"deferred finding claimed as separate work", func(in *domain.FollowUpFilingInput) {
			in.Source.Kind = domain.FollowUpSourceSeparateWorkVerdict
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"parked finding claimed as deferred", func(in *domain.FollowUpFilingInput) {
			in.Source.FindingID = fx.findings[1].ID
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"finding absent from the artifact", func(in *domain.FollowUpFilingInput) {
			in.Source.FindingID = "finding-unknown"
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"artifact that does not exist", func(in *domain.FollowUpFilingInput) {
			in.Source.AdjudicationDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"another run's artifact", func(in *domain.FollowUpFilingInput) {
			in.Source = other.deferredSource()
		}, domain.ErrFollowUpFilingSourceMismatch},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fx.input(fx.deferredSource())
			tc.mutate(&input)
			_, err := allocateFiling(ctx, st, fx.proposal(t, input), "event-rejected-"+strconv.Itoa(i))
			if !errors.Is(err, tc.want) {
				t.Fatalf("admission error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestFollowUpFilingReconstructionReGates tampers with an admitted filing row
// (target, source, and text in turn) and shows the read fails: reconstruction
// re-runs the gate against current rows instead of trusting the decoded body.
// Each replacement body is self-consistent, with a valid content address, so
// only the re-gate can catch it.
func TestFollowUpFilingReconstructionReGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(filingFixture, *domain.FollowUpFilingInput)
		want   error
	}{
		{"target", func(_ filingFixture, in *domain.FollowUpFilingInput) {
			in.Target.Repo, in.Target.RepositoryID = "attacker/repo", 999
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"labels", func(_ filingFixture, in *domain.FollowUpFilingInput) {
			in.Target.Labels = []string{"priority:critical"}
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"milestone", func(_ filingFixture, in *domain.FollowUpFilingInput) {
			in.Target.Milestone = "2A"
		}, domain.ErrFollowUpFilingTargetMismatch},
		{"source", func(fx filingFixture, in *domain.FollowUpFilingInput) {
			in.Source.FindingID = fx.findings[2].ID
		}, domain.ErrFollowUpFilingSourceMismatch},
		{"text", func(_ filingFixture, in *domain.FollowUpFilingInput) {
			in.Body = passedIssueText("Closes #12. Ask @octocat.")
		}, domain.ErrFollowUpFilingTextRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			st := storetest.Open(t, path, store.Options{})
			fx := seedFiling(t, ctx, st, "project-filing")
			instance, err := allocateFiling(ctx, st, fx.proposal(t, fx.input(fx.deferredSource())), "event-tamper")
			if err != nil {
				t.Fatal(err)
			}
			forgedInput := fx.input(fx.deferredSource())
			tc.mutate(fx, &forgedInput)
			forged := instance
			forged.Proposal = fx.proposal(t, forgedInput)
			body, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := raw.Exec(`UPDATE effect_proposal_instances SET content_digest = ?, body = ?
				WHERE instance_id = ?`, forged.Proposal.Digest, string(body), instance.ID)
			if err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("tamper changed %d rows, %v", changed, err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			if _, err := getFiling(ctx, reopened, instance.ID); !errors.Is(err, tc.want) {
				t.Fatalf("reconstruction error = %v, want %v", err, tc.want)
			}
		})
	}
}

// openFilingCard admits one filing under event and opens its item with a
// command carrying action.
func openFilingCard(
	t *testing.T, ctx context.Context, st *store.Store, fx filingFixture,
	proposal domain.EffectProposal, event string, action domain.Action,
) (domain.ProposalInstance, domain.Command) {
	t.Helper()
	instance, err := allocateFiling(ctx, st, proposal, event)
	if err != nil {
		t.Fatal(err)
	}
	var command domain.Command
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		command, err = filingItem(ctx, tx, fx, instance, action, nil)
		return err
	}); err != nil {
		t.Fatalf("open the %s filing item: %v", action, err)
	}
	return instance, command
}

func decideFiling(
	ctx context.Context, st *store.Store, instance domain.ProposalInstance, command domain.Command,
) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		var selected *domain.Digest
		if command.Action != domain.ActionDecline {
			selected = &instance.Proposal.Digest
		}
		return tx.RecordProposalDecision(
			ctx, instance.ID, command.CommandID, command.Action, selected, filingAt.Add(6*time.Hour))
	})
}

// assertStaleFilingStaysReadable pins what a stale source does and does not
// break. The gate that runs on every read checks only append-only rows, so
// the instance and the whole item list still reconstruct; the filing can be
// declined; and it can no longer be approved.
func assertStaleFilingStaysReadable(
	t *testing.T, ctx context.Context, st *store.Store,
	approve, decline domain.ProposalInstance, approveCommand, declineCommand domain.Command,
) {
	t.Helper()
	if _, err := getFiling(ctx, st, approve.ID); err != nil {
		t.Fatalf("read of a stale filing: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, id := range []domain.ItemID{approveCommand.ItemID, declineCommand.ItemID} {
			listed := slices.ContainsFunc(items, func(item store.Snapshotted[domain.AttentionItem]) bool {
				return item.Value.ID == id
			})
			if !listed {
				t.Errorf("item %q missing from the list", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list items holding a stale filing: %v", err)
	}
	if err := decideFiling(ctx, st, approve, approveCommand); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
		t.Fatalf("approve a stale filing = %v, want ErrFollowUpFilingSourceStale", err)
	}
	if err := decideFiling(ctx, st, decline, declineCommand); err != nil {
		t.Fatalf("decline a stale filing: %v", err)
	}
}

// TestFollowUpFilingDeferredSourceMustStayEffective shows the source link is
// checked against current rows where a filing gains authority: once a later
// round fixes the finding, its deferred disposition is no longer the
// effective one, so a new filing is refused and an open one cannot be
// approved. Reads stay open.
func TestFollowUpFilingDeferredSourceMustStayEffective(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	proposal := fx.proposal(t, fx.input(fx.deferredSource()))
	approve, approveCommand := openFilingCard(t, ctx, st, fx, proposal, "event-approve", domain.ActionApprove)
	decline, declineCommand := openFilingCard(t, ctx, st, fx, proposal, "event-decline", domain.ActionDecline)
	parked, parkedCommand := openFilingCard(
		t, ctx, st, fx, fx.proposal(t, fx.input(fx.parkedSource())), "event-parked", domain.ActionApprove)

	deferred := fx.findings[0]
	secondReview := adjudicationReviewRecord(
		t, fx.runID, 2, []domain.FindingID{deferred.ID}, filingAt.Add(3*time.Hour))
	remediation := dispositionReviewRecord(t, fx.runID, 3, nil, filingAt.Add(4*time.Hour))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, secondReview, []domain.Finding{deferred}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, remediation, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: deferred.ID, RunID: fx.runID, Round: 2,
			Disposition: domain.ReviewDispositionFixed, Reason: "fixed in the remediation head",
			RemediationInvocationID: remediation.InvocationID, CreatedAt: filingAt.Add(5 * time.Hour),
		})
	}); err != nil {
		t.Fatalf("record the later fix: %v", err)
	}

	assertStaleFilingStaysReadable(t, ctx, st, approve, decline, approveCommand, declineCommand)
	if _, err := allocateFiling(ctx, st, proposal, "event-after-fix"); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
		t.Fatalf("admission after the finding was fixed = %v, want ErrFollowUpFilingSourceStale", err)
	}
	// The separate-work filing rests on its own entry and is unaffected.
	if err := decideFiling(ctx, st, parked, parkedCommand); err != nil {
		t.Fatalf("approve the separate-work filing after an unrelated fix: %v", err)
	}
}

// TestFollowUpFilingSeparateWorkSourceMustBeCurrentRevision shows a
// separate-work verdict backs a filing only while its artifact is the round's
// head: no disposition row pins the revision, so a successor that amends the
// adjudication retires the verdict it replaced. As with a deferred source,
// that refuses admission and approval and leaves reads open.
func TestFollowUpFilingSeparateWorkSourceMustBeCurrentRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	// putAdjudicationFeedbackAuthority opens its item under project-1.
	fx := seedFiling(t, ctx, st, "project-1")
	proposal := fx.proposal(t, fx.input(fx.parkedSource()))
	approve, approveCommand := openFilingCard(t, ctx, st, fx, proposal, "event-approve", domain.ActionApprove)
	decline, declineCommand := openFilingCard(t, ctx, st, fx, proposal, "event-decline", domain.ActionDecline)

	conversation := adjudicationConversation(t, "conversation-filing", []string{"reconsider"}, filingAt)
	invocation, feedback := adjudicationFeedback(t, conversation, "invocation-filing-revision", 1)
	successor := successorAdjudication(
		t, fx.adjudication, feedback, "amended after discussion", filingAt.Add(2*time.Hour))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, fx.adjudication, conversation, invocation, "item-filing-revision", 2,
		); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, successor)
	}); err != nil {
		t.Fatalf("append the successor revision: %v", err)
	}

	assertStaleFilingStaysReadable(t, ctx, st, approve, decline, approveCommand, declineCommand)
	if _, err := allocateFiling(ctx, st, proposal, "event-old-revision"); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
		t.Fatalf("admission under a superseded revision = %v, want ErrFollowUpFilingSourceStale", err)
	}
	current := fx.input(fx.parkedSource())
	current.Source.AdjudicationDigest = successor.Digest
	if _, err := allocateFiling(ctx, st, fx.proposal(t, current), "event-current-revision"); err != nil {
		t.Fatalf("admission under the current revision: %v", err)
	}
}

// filingItem opens an effect_proposal item over an admitted instance and
// stores a command carrying action against it.
func filingItem(
	ctx context.Context, tx *store.WriteTx, fx filingFixture, instance domain.ProposalInstance,
	action domain.Action, merge *domain.ProspectiveMerge,
) (domain.Command, error) {
	// The item offers the action under test even where a filing card would
	// not, so a refusal comes from the decision gate, not from the command
	// never being offered.
	offered := []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze}
	if !slices.Contains(offered, action) {
		offered = append(offered, action)
	}
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		return domain.Command{}, err
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return domain.Command{}, err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(string(instance.ID) + "/effect"), ProjectID: fx.projectID,
		Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)},
		Type:    domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
		Reason:            "Decide whether to file the follow-up issue",
		RequestedDecision: offered,
		EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.Command{}, err
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.Command{}, err
	}
	if err := tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, merge); err != nil {
		return domain.Command{}, err
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-" + string(action) + "-" + string(instance.ID), DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion,
		ArtifactDigests: item.ArtifactDigests, Action: action,
	})
	if err != nil {
		return domain.Command{}, err
	}
	return command, tx.PutCommand(ctx, command)
}

// TestFollowUpFilingDecisions pins the kind's decision family: a filing
// instance records approve and decline, and refuses approve_with_changes and
// start, which belong to the closure and task kinds.
func TestFollowUpFilingDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		action domain.Action
		want   error
	}{
		{domain.ActionApprove, nil},
		{domain.ActionDecline, nil},
		{domain.ActionApproveWithChanges, domain.ErrTransitionCommandMismatch},
		{domain.ActionStart, domain.ErrTransitionCommandMismatch},
		{domain.ActionStartWithChanges, domain.ErrTransitionCommandMismatch},
	}
	for _, tc := range cases {
		t.Run(string(tc.action), func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
			fx := seedFiling(t, ctx, st, "project-filing")
			instance, err := allocateFiling(ctx, st, fx.proposal(t, fx.input(fx.deferredSource())), "event-decision")
			if err != nil {
				t.Fatal(err)
			}
			err = st.Write(ctx, func(tx *store.WriteTx) error {
				command, err := filingItem(ctx, tx, fx, instance, tc.action, nil)
				if err != nil {
					t.Fatalf("open the filing item: %v", err)
				}
				var selected *domain.Digest
				if tc.action != domain.ActionDecline {
					selected = &instance.Proposal.Digest
				}
				return tx.RecordProposalDecision(
					ctx, instance.ID, command.CommandID, tc.action, selected, filingAt.Add(2*time.Hour))
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("decision error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestApprovedFilingAuthorizesNothingElse pins the kind boundary from the
// other side: an approved filing is not a closure approval, cannot take a
// policy closure approval, and does not authenticate a task start.
func TestApprovedFilingAuthorizesNothingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	instance, command := openFilingCard(
		t, ctx, st, fx, fx.proposal(t, fx.input(fx.deferredSource())), "event-approved", domain.ActionApprove)
	if err := decideFiling(ctx, st, instance, command); err != nil {
		t.Fatalf("approve the filing: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if approval, err := tx.ClosureApprovalForInstance(ctx, instance.ID); err == nil {
			t.Errorf("closure approval for an approved filing = %+v, want a refusal", approval)
		}
		started, err := tx.AuthenticateStartDecision(ctx, instance.ID, instance.Proposal.Digest)
		if err != nil {
			return err
		}
		if started {
			t.Error("an approved filing authenticated a task start")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
		CandidateHeadSHA:    strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordPolicyClosureApproval(ctx, instance.ID, merge, filingAt.Add(7*time.Hour))
		return err
	}); err == nil {
		t.Fatal("a policy closure approval landed on a filing instance")
	}
}

// TestBindProposalItemFollowsEffectKind pins the merge rule per kind on an
// effect_proposal item: a filing merges nothing and so refuses a merge.
func TestBindProposalItemFollowsEffectKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-filing")
	instance, err := allocateFiling(ctx, st, fx.proposal(t, fx.input(fx.deferredSource())), "event-bind")
	if err != nil {
		t.Fatal(err)
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
		CandidateHeadSHA:    strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
	}
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := filingItem(ctx, tx, fx, instance, domain.ActionApprove, &merge)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "follow-up filing carries a merge") {
		t.Fatalf("bind a filing with a merge = %v, want a refusal", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := filingItem(ctx, tx, fx, instance, domain.ActionApprove, nil)
		return err
	}); err != nil {
		t.Fatalf("bind a filing without a merge: %v", err)
	}
}
