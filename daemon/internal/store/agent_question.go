package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	agentQuestionSpecificationTerminalKind = "specification_stage_terminal"
	agentQuestionProductionTerminalKind    = "production_stage_terminal"
)

type agentQuestionSpecificationTerminal struct {
	InvocationID        domain.InvocationID `json:"invocation_id"`
	Iteration           int                 `json:"iteration"`
	Status              exec.Status         `json:"status"`
	ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
	SpecArtifactID      *domain.ArtifactID  `json:"spec_artifact_id,omitempty"`
	ApprovalItemID      *domain.ItemID      `json:"approval_item_id,omitempty"`
	SummaryDigest       *domain.Digest      `json:"summary_digest,omitempty"`
	DecisionArtifactID  *domain.ArtifactID  `json:"decision_artifact_id,omitempty"`
	QuestionItemID      *domain.ItemID      `json:"question_item_id,omitempty"`
}

type agentQuestionProductionTerminal struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	StageID      domain.StageID      `json:"stage_id"`
	Status       exec.Status         `json:"status"`
	HeadSHA      string              `json:"head_sha,omitempty"`
	Artifacts    []domain.Digest     `json:"artifacts,omitempty"`
	Summary      string              `json:"summary,omitempty"`
}

// gateAgentQuestionItem authenticates an agent_question presentation against
// the durable terminal that created it. The item body and its Question claim
// must not be able to authorize another invocation by agreeing only with each
// other.
func (tx *ReadTx) gateAgentQuestionItem(ctx context.Context, item domain.AttentionItem) error {
	if item.AgentQuestion == nil {
		return nil
	}
	facts := item.AgentQuestion
	if item.Type != domain.AttentionAgentQuestion || item.Subject.RunID == nil ||
		item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(*item.Subject.RunID) ||
		item.ID != domain.ItemID("question-"+string(facts.InvocationID)) ||
		!slices.Equal(item.RequestedDecision,
			[]domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop}) {
		return domain.ErrParentKeyMismatch
	}
	run, err := tx.GetRun(ctx, *item.Subject.RunID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	stageID, attemptID, found := questionInvocationAttempt(run, facts.InvocationID, facts.Stage)
	if run.ProjectID != item.ProjectID || !found {
		return domain.ErrParentKeyMismatch
	}
	claim, artifact, err := tx.agentQuestionArtifact(ctx, item)
	if err != nil {
		return err
	}
	entry, err := tx.GetInbox(ctx, string(facts.InvocationID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	switch facts.Stage {
	case domain.StageNameSpecification:
		admission, err := tx.GetExecutionAdmissionRecord(ctx, facts.InvocationID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return domain.ErrParentKeyMismatch
			}
			return err
		}
		terminal, err := decodeAgentQuestionSpecificationTerminal(entry)
		if err != nil || admission.RunID != run.ID || admission.StageID != stageID ||
			admission.AttemptID != attemptID || terminal.InvocationID != facts.InvocationID ||
			terminal.InvocationID != domain.SpecificationInvocationID(run.ID, terminal.Iteration) ||
			terminal.DecisionArtifactID == nil || *terminal.DecisionArtifactID != artifact.ID ||
			terminal.QuestionItemID == nil || *terminal.QuestionItemID != item.ID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
	case domain.StageNameImplementation:
		claims, err := tx.GetAgentClaims(ctx, facts.InvocationID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return domain.ErrParentKeyMismatch
			}
			return err
		}
		var blocked *domain.AgentClaim
		for index := range claims {
			if claims[index].Label != export.BlockedEvidenceLabel {
				continue
			}
			if blocked != nil || claims[index].Provenance.ProducerInvocationID != facts.InvocationID {
				return domain.ErrParentKeyMismatch
			}
			blocked = &claims[index]
		}
		if blocked == nil {
			return domain.ErrParentKeyMismatch
		}
		expectedClaim := *blocked
		expectedClaim.Label = domain.AgentQuestionClaimLabel
		outcome, err := tx.GetExecutionOutcomeRecord(ctx, facts.InvocationID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return domain.ErrParentKeyMismatch
			}
			return err
		}
		terminal, err := decodeAgentQuestionProductionTerminal(entry)
		if err != nil || terminal.InvocationID != facts.InvocationID || terminal.RunID != run.ID ||
			terminal.Status != exec.StatusBlocked || terminal.HeadSHA != "" ||
			terminal.StageID != stageID || outcome.Status != domain.ExecutionOutcomeBlocked ||
			outcome.Summary != terminal.Summary || !reflect.DeepEqual(expectedClaim, claim) ||
			!slices.Contains(terminal.Artifacts, claim.Digest) ||
			terminal.Summary != exec.TruncateSummary(facts.Decisions[0].Question) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
	default:
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) agentQuestionArtifact(
	ctx context.Context, item domain.AttentionItem,
) (domain.AgentClaim, domain.Artifact, error) {
	facts := item.AgentQuestion
	expectedDigest, err := facts.ComputeDigest()
	if err != nil {
		return domain.AgentClaim{}, domain.Artifact{}, err
	}
	var matched *domain.AgentClaim
	for index := range item.AgentClaims {
		claim := &item.AgentClaims[index]
		if claim.Label != domain.AgentQuestionClaimLabel ||
			claim.Provenance.ProducerInvocationID != facts.InvocationID {
			continue
		}
		if matched != nil {
			return domain.AgentClaim{}, domain.Artifact{}, domain.ErrParentKeyMismatch
		}
		matched = claim
	}
	if matched == nil || matched.Digest != expectedDigest {
		return domain.AgentClaim{}, domain.Artifact{}, domain.ErrParentKeyMismatch
	}
	artifact, err := tx.GetArtifact(ctx, matched.Artifact)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.AgentClaim{}, domain.Artifact{}, domain.ErrParentKeyMismatch
		}
		return domain.AgentClaim{}, domain.Artifact{}, err
	}
	if artifact.Type != domain.ArtifactKindEvidence || artifact.Digest != matched.Digest ||
		artifact.Provenance != matched.Provenance {
		return domain.AgentClaim{}, domain.Artifact{}, domain.ErrParentKeyMismatch
	}
	return *matched, artifact, nil
}

func questionInvocationAttempt(
	run domain.Run, invocationID domain.InvocationID, role domain.StageName,
) (domain.StageID, domain.AttemptID, bool) {
	for _, stage := range run.Stages {
		mapped, err := domain.CanonicalStageRole(stage.Name)
		if err != nil || mapped != role {
			continue
		}
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID == invocationID && attempt.StageID == stage.ID {
				return stage.ID, attempt.ID, true
			}
		}
	}
	return "", "", false
}

func decodeAgentQuestionSpecificationTerminal(
	entry QueueEntry,
) (agentQuestionSpecificationTerminal, error) {
	if entry.Kind != agentQuestionSpecificationTerminalKind {
		return agentQuestionSpecificationTerminal{}, domain.ErrParentKeyMismatch
	}
	var terminal agentQuestionSpecificationTerminal
	if err := strictjson.Decode(entry.Payload, &terminal, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return agentQuestionSpecificationTerminal{}, err
	}
	if terminal.InvocationID == "" || terminal.Iteration < 1 || terminal.Status != exec.StatusCompleted ||
		terminal.ResearchArtifactIDs == nil || len(terminal.ResearchArtifactIDs) != 0 ||
		terminal.SpecArtifactID != nil || terminal.ApprovalItemID != nil || terminal.SummaryDigest != nil ||
		terminal.DecisionArtifactID == nil || *terminal.DecisionArtifactID == "" ||
		terminal.QuestionItemID == nil || *terminal.QuestionItemID == "" ||
		string(terminal.InvocationID) != entry.IdempotencyKey {
		return agentQuestionSpecificationTerminal{}, domain.ErrParentKeyMismatch
	}
	canonical, err := json.Marshal(terminal)
	if err != nil || !bytes.Equal(canonical, entry.Payload) {
		return agentQuestionSpecificationTerminal{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return terminal, nil
}

func decodeAgentQuestionProductionTerminal(
	entry QueueEntry,
) (agentQuestionProductionTerminal, error) {
	if entry.Kind != agentQuestionProductionTerminalKind {
		return agentQuestionProductionTerminal{}, domain.ErrParentKeyMismatch
	}
	var terminal agentQuestionProductionTerminal
	if err := strictjson.Decode(entry.Payload, &terminal, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return agentQuestionProductionTerminal{}, err
	}
	if terminal.InvocationID == "" || terminal.RunID == "" || terminal.StageID == "" ||
		string(terminal.InvocationID) != entry.IdempotencyKey {
		return agentQuestionProductionTerminal{}, domain.ErrParentKeyMismatch
	}
	return terminal, nil
}
