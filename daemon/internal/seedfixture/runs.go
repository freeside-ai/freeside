package seedfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// base anchors every fixture instant, so seeded state and screenshots are
// stable. Task creation instants and task IDs are the exception: the store
// mints both itself.
var base = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// project pairs a project with the one repository it is registered against.
// Every admission and base revision in the project names this repository:
// recording an admission binds the project to its repository and refuses a
// different repository ID (#1535).
type project struct {
	id           domain.ProjectID
	repo         string
	repositoryID int64
}

// The repositories are deliberately not real, so no seeded PR link or
// reference resolves to someone's repository.
var (
	freesideProject = project{id: "freeside", repo: "example/freeside", repositoryID: 1001}
	orioleProject   = project{id: "oriole", repo: "example/oriole", repositoryID: 1002}
)

const (
	baseSHA = "deadbeef"
	headSHA = "cafebabe"
	baseRef = "refs/heads/main"
)

// Run identities mirror app/Sources/FreesideAPI/RunFixtures.swift where the
// store lets them. A campaign's specification run and retry run IDs are
// derived from its first implementation run, so those two are not literal.
const (
	awaitingImplementationRunID domain.RunID = "run-freeside-specification"
	retryParentRunID            domain.RunID = "run-freeside-656"
	readyRunID                  domain.RunID = "run-freeside-654"
	completedRunID              domain.RunID = "run-freeside-640"
	questionRunID               domain.RunID = "run-freeside-question"
	failedVerificationRunID     domain.RunID = "run-oriole-121"
)

type seeder struct {
	ctx context.Context
	st  *store.Store
}

func (s seeder) write(fn func(*store.WriteTx) error) error {
	return s.st.Write(s.ctx, fn)
}

func (s seeder) writeInternal(fn func(*store.InternalTx) error) error {
	return s.st.WriteInternal(s.ctx, fn)
}

func registerProjects(s seeder, projects ...project) error {
	return s.writeInternal(func(tx *store.InternalTx) error {
		for _, p := range projects {
			registered, err := domain.NewProject(p.id, p.repo, p.repositoryID)
			if err != nil {
				return err
			}
			if err := tx.RegisterProject(s.ctx, registered); err != nil {
				return err
			}
		}
		return nil
	})
}

func stageID(runID domain.RunID) domain.StageID { return domain.StageID("stage-" + string(runID)) }

func invocationID(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("inv-" + string(runID))
}

func attemptID(invocation domain.InvocationID) domain.AttemptID {
	return domain.AttemptID("attempt-" + string(invocation))
}

func publicationInvocationID(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("publish-production-" + string(runID))
}

// digest content-addresses a label, giving every fixture digest the valid
// sha256 shape the stricter records (review, adjudication) require.
func digest(label string) domain.Digest { return domain.Digest(contentaddr.Sum([]byte(label))) }

// singleStageRun is a run with one stage holding one attempt, the shape every
// non-specification fixture run takes.
func singleStageRun(p project, runID domain.RunID, stage string, policyDigest domain.Digest) domain.Run {
	invocation := invocationID(runID)
	return domain.Run{
		ID: runID, ProjectID: p.id, SpecDigest: digest("spec-" + string(runID)), PolicyDigest: policyDigest,
		Stages: []domain.Stage{{
			ID: stageID(runID), RunID: runID, Name: stage,
			Attempts: []domain.Attempt{{
				ID: attemptID(invocation), StageID: stageID(runID), Number: 1, InvocationID: invocation,
			}},
		}},
	}
}

// submit records the production dispatch intent and the run_submitted
// milestone it authorizes. The intent is marked dispatched at once, as the
// specification and publication intents are: a pending intent would be
// executed by the first engine a later driver start opens on this store.
func (s seeder) submit(tx *store.WriteTx, run domain.Run, at time.Time) error {
	attempt := run.Stages[0].Attempts[len(run.Stages[0].Attempts)-1]
	payload := fmt.Appendf(nil, `{"invocation_id":%q,"run_id":%q,"stage_id":%q}`,
		attempt.InvocationID, run.ID, attempt.StageID)
	if _, _, err := tx.EnqueueOutbox(s.ctx, string(attempt.InvocationID),
		string(domain.ProductionInvocationRequestedKind), payload); err != nil {
		return err
	}
	if err := tx.MarkOutboxDispatched(s.ctx, string(attempt.InvocationID)); err != nil {
		return err
	}
	return tx.AppendRunMilestone(s.ctx, domain.RunMilestone{
		RunID: run.ID, Kind: domain.MilestoneRunSubmitted, InvocationID: &attempt.InvocationID, RecordedAt: at,
	})
}

// admit records an attended_dev admission for the run's last attempt, which
// also appends its invocation_admitted milestone.
func (s seeder) admit(tx *store.WriteTx, p project, run domain.Run, at time.Time) (domain.ExecutionAdmission, error) {
	stage := run.Stages[len(run.Stages)-1]
	attempt := stage.Attempts[len(stage.Attempts)-1]
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: stage.ID, AttemptID: attempt.ID,
		Backend:       "fresh_vm_read_only_volume_handoff",
		Capabilities:  AdmissionCapabilities,
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialLocalTrusted,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/example/agent@" + string(digest("agent-image"))),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest,
		InputDigest: digest("input-" + string(attempt.InvocationID)),
		Base: domain.BaseRevision{
			Repo: p.repo, RepositoryID: p.repositoryID, BaseRef: baseRef, BaseSHA: baseSHA,
		},
		Workspace: "workspace-" + string(run.ID), AdmittedAt: at,
	})
	if err != nil {
		return domain.ExecutionAdmission{}, err
	}
	return admission, tx.RecordExecutionAdmission(s.ctx, admission)
}

func (s seeder) export(tx *store.WriteTx, admission domain.ExecutionAdmission, at time.Time) error {
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: headSHA,
		ManifestDigest: digest("manifest-" + string(admission.InvocationID)), RecordedAt: at,
	})
	if err != nil {
		return err
	}
	return tx.RecordExecutionExportRecord(s.ctx, export)
}

// fail records a failed execution outcome, its terminal milestone, and the
// matching concluded observation.
func (s seeder) fail(tx *store.WriteTx, admission domain.ExecutionAdmission, summary string, at time.Time) error {
	if err := tx.RecordExecutionOutcome(s.ctx, domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: summary, RecordedAt: at,
	}); err != nil {
		return err
	}
	failed := domain.ObservedStatusFailed
	if err := tx.AppendRunMilestone(s.ctx, domain.RunMilestone{
		RunID: admission.RunID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &admission.InvocationID,
		Terminal: &failed, RecordedAt: at.Add(time.Minute),
	}); err != nil {
		return err
	}
	return tx.RecordInvocationObservation(s.ctx, domain.InvocationObservation{
		InvocationID: admission.InvocationID, RunID: admission.RunID,
		Status: domain.ObservedStatusFailed, ObservedAt: at.Add(time.Minute),
	})
}

// name gives the run's task a display name and records its first start.
func (s seeder) name(tx *store.WriteTx, runID domain.RunID, name domain.DisplayName, startedAt time.Time) error {
	run, err := tx.GetRun(s.ctx, runID)
	if err != nil {
		return err
	}
	if err := tx.RecordTaskStart(s.ctx, runID, startedAt); err != nil {
		return err
	}
	return tx.SetTaskName(s.ctx, run.TaskID, name)
}

// specificationCampaign records a campaign's first attempt through its
// specification run's completed terminal and the open spec_approval item, the
// state an approval gate leaves a campaign in. It returns the campaign, the
// specification run, and the approval item.
func (s seeder) specificationCampaign(
	p project, implementationRunID domain.RunID, at time.Time,
) (domain.CampaignID, domain.Run, domain.AttentionItem, error) {
	campaign, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	specificationRunID, err := engine.SpecificationRunIDForImplementation(implementationRunID)
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	invocation := domain.InvocationID("inv-specify-" + string(specificationRunID) + "-1")
	specStage := domain.StageID("specify-" + string(specificationRunID))
	provenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocation,
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	metadata := domain.EvidenceMetadata{
		MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1, CreatedAt: at,
		Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
	}
	source, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("source-" + string(implementationRunID)), Type: domain.ArtifactKindSpecification,
		Digest: digest("source-" + string(implementationRunID)), Provenance: provenance, Metadata: metadata,
	}, nil)
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	specification, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("spec-" + string(implementationRunID) + "-1"), Type: domain.ArtifactKindSpecification,
		Digest: digest("spec-" + string(implementationRunID)), Provenance: provenance, Metadata: metadata,
	}, nil)
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	policy, err := domain.NewResolvedPolicy(specificationRunID, []domain.PolicyKey{{
		Key: "gates.spec_approval", Value: "true",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: digest("policy-source")},
	}})
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	publication := digest("publication-" + string(implementationRunID))
	request, err := json.Marshal(map[string]any{
		"version": "freeside.specification-request/v1", "specification_run_id": specificationRunID,
		"implementation_run_id": implementationRunID, "project_id": p.id,
		"invocation_id": invocation, "iteration": 1, "campaign_id": campaign, "attempt_number": 1,
		"publication_digest": publication, "input_artifact_ids": []domain.ArtifactID{source.ID},
	})
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	specificationRun := domain.Run{
		ID: specificationRunID, ProjectID: p.id, SpecDigest: source.Digest, PolicyDigest: policy.Digest,
		CampaignID: campaign, AttemptNumber: 1,
		Stages: []domain.Stage{{
			ID: specStage, RunID: specificationRunID, Name: "specification",
			Attempts: []domain.Attempt{{ID: attemptID(invocation), StageID: specStage, Number: 1, InvocationID: invocation}},
		}},
	}
	approvalID := domain.ItemID("spec-approval-" + string(implementationRunID) + "-1")
	terminal, err := json.Marshal(struct {
		InvocationID        domain.InvocationID `json:"invocation_id"`
		Iteration           int                 `json:"iteration"`
		Status              string              `json:"status"`
		ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
		SpecArtifactID      *domain.ArtifactID  `json:"spec_artifact_id,omitempty"`
		ApprovalItemID      *domain.ItemID      `json:"approval_item_id,omitempty"`
	}{
		InvocationID: invocation, Iteration: 1, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{}, SpecArtifactID: &specification.ID, ApprovalItemID: &approvalID,
	})
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	createdAt := at.Add(20 * time.Minute)
	claimMetadata := metadata
	claimMetadata.Source = domain.EvidenceSourceClaim
	approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: approvalID, ProjectID: p.id,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(specificationRunID), RunID: &specificationRunID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "The specification is ready for approval.",
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
		},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: specification.ID, Digest: specification.Digest,
			Provenance: specification.Provenance, Metadata: claimMetadata,
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen, CreatedAt: &createdAt,
	}, nil)
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, err
	}
	err = s.write(func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(s.ctx, source); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(s.ctx, domain.ProductionAttempt{
			CampaignID: campaign, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: source.Digest, PublicationDigest: publication,
			SpecificationRunID: specificationRunID, ImplementationRunID: implementationRunID,
		}); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(s.ctx, string(invocation),
			string(domain.SpecificationInvocationRequestedKind), request); err != nil {
			return err
		}
		if err := tx.PutRun(s.ctx, specificationRun); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(s.ctx, policy); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(s.ctx, string(invocation)); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(s.ctx, domain.RunMilestone{
			RunID: specificationRunID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: at,
		}); err != nil {
			return err
		}
		if err := tx.PutArtifact(s.ctx, specification); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(s.ctx, string(invocation), "specification_stage_terminal", terminal); err != nil {
			return err
		}
		return tx.PutAttentionItem(s.ctx, approval)
	})
	if err != nil {
		return "", domain.Run{}, domain.AttentionItem{}, fmt.Errorf("specification campaign %s: %w", implementationRunID, err)
	}
	return campaign, specificationRun, approval, nil
}

// resolve concludes an open item with the given action the way a device
// decision does: the recorded command, then the item resolved at its next
// version with the decision instant.
func (s seeder) resolve(tx *store.WriteTx, item domain.AttentionItem, action domain.Action, at time.Time) error {
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: string(action) + "-" + string(item.ID), DeviceID: "device-seed-fixture", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		Action: action,
	})
	if err != nil {
		return err
	}
	if err := tx.PutCommand(s.ctx, command); err != nil {
		return err
	}
	item.Status = domain.StatusResolved
	item.ItemVersion++
	item, err = item.WithDecidedAt(at)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(s.ctx, item)
}

// approveSpecification resolves the campaign's spec_approval item and
// approves attempt 1 with the specification digest, the writes the engine
// commits when a specification is approved.
func (s seeder) approveSpecification(
	tx *store.WriteTx, campaign domain.CampaignID, approval domain.AttentionItem,
	specDigest domain.Digest, at time.Time,
) error {
	if err := s.resolve(tx, approval, domain.ActionApprove, at); err != nil {
		return err
	}
	_, err := tx.ApproveProductionAttempt(s.ctx, campaign, 1, specDigest)
	return err
}

// seedAwaitingApproval is a campaign stopped at its specification gate.
func seedAwaitingApproval(s seeder) (domain.Run, domain.AttentionItem, error) {
	at := base
	_, specificationRun, approval, err := s.specificationCampaign(freesideProject, awaitingImplementationRunID, at)
	if err != nil {
		return domain.Run{}, domain.AttentionItem{}, err
	}
	err = s.write(func(tx *store.WriteTx) error {
		return s.name(tx, specificationRun.ID, domain.DisplayName{
			Text: "Time grammar for inbox rows", Source: domain.DisplayNameSourceAgent,
		}, at)
	})
	return specificationRun, approval, err
}

// seedRetryCampaign is an implementation campaign whose first attempt failed
// and whose second attempt is running, held on verification findings.
func seedRetryCampaign(s seeder) (failed, active domain.Run, err error) {
	at := base.Add(time.Hour)
	p := freesideProject
	campaign, specificationRun, approval, err := s.specificationCampaign(p, retryParentRunID, at)
	if err != nil {
		return domain.Run{}, domain.Run{}, err
	}
	failed = singleStageRun(p, retryParentRunID, "implementation", digest("policy-"+string(retryParentRunID)))
	failed.SpecDigest = digest("spec-" + string(retryParentRunID))
	failed.CampaignID, failed.AttemptNumber = campaign, 1
	activeID, err := engine.ProductionAttemptRunID(campaign, 2)
	if err != nil {
		return domain.Run{}, domain.Run{}, err
	}
	active = singleStageRun(p, activeID, "implementation", failed.PolicyDigest)
	active.SpecDigest = failed.SpecDigest
	active.CampaignID, active.AttemptNumber = campaign, 2
	active.AttemptReason, active.ParentRunID = "Retry after repairing the acceptance rig", failed.ID
	err = s.write(func(tx *store.WriteTx) error {
		if err := s.approveSpecification(tx, campaign, approval, failed.SpecDigest, at.Add(30*time.Minute)); err != nil {
			return err
		}
		if err := tx.PutRun(s.ctx, failed); err != nil {
			return err
		}
		if err := s.submit(tx, failed, at.Add(31*time.Minute)); err != nil {
			return err
		}
		admission, err := s.admit(tx, p, failed, at.Add(32*time.Minute))
		if err != nil {
			return err
		}
		if err := s.fail(tx, admission, "attempt did not converge", at.Add(50*time.Minute)); err != nil {
			return err
		}
		if err := s.name(tx, specificationRun.ID, domain.DisplayName{
			Text: "Repair the acceptance rig", Source: domain.DisplayNameSourceOperator,
		}, at); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(s.ctx, domain.ProductionAttempt{
			CampaignID: campaign, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
			Reason: active.AttemptReason, ParentRunID: failed.ID,
			SourceDigest: specificationRun.SpecDigest, PublicationDigest: digest("publication-" + string(retryParentRunID)),
			ApprovedSpecDigest: failed.SpecDigest,
			SpecificationRunID: specificationRun.ID, ImplementationRunID: active.ID,
		}); err != nil {
			return err
		}
		if err := tx.PutRun(s.ctx, active); err != nil {
			return err
		}
		if err := s.submit(tx, active, at.Add(55*time.Minute)); err != nil {
			return err
		}
		admission, err = s.admit(tx, p, active, at.Add(56*time.Minute))
		if err != nil {
			return err
		}
		if err := tx.RecordInvocationObservation(s.ctx, domain.InvocationObservation{
			InvocationID: admission.InvocationID, RunID: active.ID,
			Status: domain.ObservedStatusRunning, Live: true, ObservedAt: at.Add(80 * time.Minute),
		}); err != nil {
			return err
		}
		// A milestone clears the run's hold, so the hold is recorded last.
		return tx.RecordRunHold(s.ctx, domain.RunHoldObservation{
			RunID: active.ID, InvocationID: &admission.InvocationID, Reason: domain.HoldVerificationFindings,
			FirstObservedAt: at.Add(70 * time.Minute), LastObservedAt: at.Add(80 * time.Minute),
		})
	})
	if err != nil {
		return domain.Run{}, domain.Run{}, fmt.Errorf("retry campaign: %w", err)
	}
	return failed, active, nil
}

// publishedRun is the state a run reaches once publication converges: its
// admission and export, the open ready item and its PR binding, the
// publication_ready milestone, and the work unit's declaration and PR binding.
type publishedRun struct {
	run         domain.Run
	declaration domain.WorkUnitDeclaration
	binding     domain.WorkUnitPRBinding
}

func (s seeder) publish(
	p project, runID domain.RunID, prNumber, boundIssue int, taskName domain.DisplayName, at time.Time,
) (publishedRun, error) {
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/,app/",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: digest("paths-override")},
	}})
	if err != nil {
		return publishedRun{}, err
	}
	run := singleStageRun(p, runID, "implementation", policy.Digest)
	producing := invocationID(runID)
	publication := publicationInvocationID(runID)
	pr := domain.PRReference{Repo: p.repo, Number: prNumber}
	readyCreatedAt := at.Add(2 * time.Hour)
	ready, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: p.id,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "Checks are green and the diff is ready for final review.",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         headSHA, PRReference: &pr,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &readyCreatedAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return publishedRun{}, err
	}
	identity := digest(fmt.Sprintf("publication-%d", prNumber))
	intent := publicationrecord.Intent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      identity, InvocationID: publication,
		Branch: publicationrecord.BranchName(identity),
		Repo:   p.repo, BaseRef: baseRef, SourceHeadSHA: headSHA,
		AuthorizationID:       digest("publication-authorization"),
		ProducingInvocationID: producing, ReservationRunID: runID,
	}
	intentPayload, err := intent.Encode()
	if err != nil {
		return publishedRun{}, err
	}
	outcomePayload, err := json.Marshal(publicationrecord.Outcome{
		Identity: identity, Repo: p.repo, BaseRef: baseRef, HeadSHA: headSHA,
		Branch: publicationrecord.BranchName(identity), PRNumber: prNumber, EvidenceEligible: true,
	})
	if err != nil {
		return publishedRun{}, err
	}
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, runID, p.id, at)
	if err != nil {
		return publishedRun{}, err
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: declaration.ID, Repo: p.repo, RepositoryID: p.repositoryID,
		PRNumber: prNumber, BaseRef: baseRef, HeadSHA: headSHA, RecordedAt: at.Add(2 * time.Hour),
	}
	intentKey := "publish/" + string(publication) + "/publish.publication"
	err = s.write(func(tx *store.WriteTx) error {
		if err := tx.PutRun(s.ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(s.ctx, policy); err != nil {
			return err
		}
		if err := s.submit(tx, run, at); err != nil {
			return err
		}
		admission, err := s.admit(tx, p, run, at.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := s.export(tx, admission, at.Add(time.Hour)); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(s.ctx, ready); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(s.ctx, intentKey, "publish.publication", intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(s.ctx, intentKey); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(s.ctx, "publish.outcome/"+string(identity), "publish.outcome", outcomePayload); err != nil {
			return err
		}
		if err := tx.RecordReadyItemPRBinding(s.ctx, domain.ReadyItemPRBinding{
			ItemID: ready.ID, RunID: runID, ProducingInvocationID: producing,
			PublicationInvocationID: publication, PublicationIdentity: identity,
			Repo: p.repo, RepositoryID: p.repositoryID, PRNumber: prNumber,
			BaseRef: baseRef, HeadSHA: headSHA, RecordedAt: at.Add(2 * time.Hour),
		}); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(s.ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: &publication,
			RecordedAt: at.Add(3 * time.Hour),
		}); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(s.ctx, declaration); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitPRBinding(s.ctx, binding); err != nil {
			return err
		}
		return s.name(tx, runID, taskName, at)
	})
	if err != nil {
		return publishedRun{}, fmt.Errorf("published run %s: %w", runID, err)
	}
	return publishedRun{run: run, declaration: declaration, binding: binding}, nil
}

// seedCompletedRun publishes a run, resolves its ready item as the operator's
// open_pr decision, and records its merged PR closing the bound issue, the
// evaluated completion, and the task's completion fact.
func seedCompletedRun(s seeder) (publishedRun, error) {
	at := base.Add(-48 * time.Hour)
	p := freesideProject
	published, err := s.publish(p, completedRunID, 640, 612, domain.DisplayName{
		Text: "Drain failed startups on launch", Source: domain.DisplayNameSourceOperator,
	}, at)
	if err != nil {
		return publishedRun{}, err
	}
	pull := domain.PullMergeFact{
		Repo: p.repo, RepositoryID: p.repositoryID, PRNumber: published.binding.PRNumber,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "feedface", BaseRef: baseRef, HeadSHA: headSHA, ObservedAt: at.Add(4 * time.Hour),
	}
	issue := domain.IssueStateFact{
		Repo: p.repo, RepositoryID: p.repositoryID, IssueNumber: *published.declaration.BoundIssue,
		State: domain.IssueClosed, ClosedByCommitSHA: "feedface", ObservedAt: at.Add(4 * time.Hour),
	}
	completion, ok := domain.EvaluateWorkUnitCompletion(published.declaration, published.binding, pull, &issue)
	if !ok {
		return publishedRun{}, fmt.Errorf("completed run %s: completion facts do not evaluate as completed", completedRunID)
	}
	if err := s.writeInternal(func(tx *store.InternalTx) error {
		if _, err := tx.AppendPullMergeFact(s.ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(s.ctx, issue); err != nil {
			return err
		}
		return tx.RecordWorkUnitCompletion(s.ctx, completion)
	}); err != nil {
		return publishedRun{}, fmt.Errorf("completed run %s: %w", completedRunID, err)
	}
	publication := publicationInvocationID(completedRunID)
	err = s.write(func(tx *store.WriteTx) error {
		if err := tx.AppendRunMilestone(s.ctx, domain.RunMilestone{
			RunID: completedRunID, Kind: domain.MilestoneWorkUnitCompleted, InvocationID: &publication,
			RecordedAt: completion.RecordedAt,
		}); err != nil {
			return err
		}
		ready, err := tx.GetAttentionItemRecord(s.ctx, domain.ProductionReadyItemID(completedRunID))
		if err != nil {
			return err
		}
		if err := s.resolve(tx, ready, domain.ActionOpenPR, at.Add(3*time.Hour+30*time.Minute)); err != nil {
			return err
		}
		return tx.RecordTaskCompletion(s.ctx, completedRunID, completion.UnitID, completion.RecordedAt)
	})
	if err != nil {
		return publishedRun{}, fmt.Errorf("completed run %s: %w", completedRunID, err)
	}
	return published, nil
}

// seedFailedVerification is a verification run in the second project whose
// only attempt failed.
func seedFailedVerification(s seeder) (domain.Run, error) {
	at := base.Add(-6 * time.Hour)
	p := orioleProject
	run := singleStageRun(p, failedVerificationRunID, "verification", digest("policy-"+string(failedVerificationRunID)))
	err := s.write(func(tx *store.WriteTx) error {
		if err := tx.PutRun(s.ctx, run); err != nil {
			return err
		}
		if err := s.submit(tx, run, at); err != nil {
			return err
		}
		admission, err := s.admit(tx, p, run, at.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := s.fail(tx, admission, "verification did not pass", at.Add(20*time.Minute)); err != nil {
			return err
		}
		return s.name(tx, run.ID, domain.DisplayName{
			Text: "Verify the release checklist", Source: domain.DisplayNameSourceOperator,
		}, at)
	})
	if err != nil {
		return domain.Run{}, fmt.Errorf("failed verification run: %w", err)
	}
	return run, nil
}
