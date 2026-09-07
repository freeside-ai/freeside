package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func (tx *ReadTx) gateScopeConflictQuestion(ctx context.Context, item domain.AttentionItem) error {
	facts := item.AgentQuestion
	if item.Type != domain.AttentionAgentQuestion || item.Subject.RunID == nil ||
		item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(*item.Subject.RunID) ||
		item.ID != domain.ItemID("question-"+string(facts.InvocationID)) ||
		!slices.Equal(item.RequestedDecision, []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop}) {
		return domain.ErrParentKeyMismatch
	}
	run, err := tx.GetRun(ctx, *item.Subject.RunID)
	if err != nil {
		return err
	}
	stage, attempt, found := questionInvocationAttempt(run, facts.InvocationID, domain.StageNameImplementation)
	if !found || run.ProjectID != item.ProjectID {
		return domain.ErrParentKeyMismatch
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, facts.InvocationID)
	if err != nil {
		return err
	}
	record, err := tx.GetExecutionExportRecord(ctx, facts.InvocationID)
	if err != nil {
		return err
	}
	if admission.RunID != run.ID || admission.StageID != stage || admission.AttemptID != attempt ||
		record.AdmissionID != admission.ID || record.HeadSHA != item.PRHeadSHA || record.HeadSHA != facts.ScopeConflict.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	policy, err := tx.GetResolvedPolicy(ctx, run.ID)
	if err != nil {
		return err
	}
	paths := domain.CanonicalDeclaredPaths(policy)
	if policy.Digest != run.PolicyDigest || !slices.Equal(paths, facts.ScopeConflict.DeclaredPaths) {
		return domain.ErrParentKeyMismatch
	}
	for _, p := range facts.ScopeConflict.Paths {
		if importer.MatchesAllowlist(paths, p) {
			return domain.ErrParentKeyMismatch
		}
	}
	question, _, err := tx.agentQuestionArtifact(ctx, item)
	if err != nil {
		return err
	}
	claims, err := tx.GetAgentClaims(ctx, facts.InvocationID)
	if err != nil {
		return err
	}
	count := 0
	for _, claim := range claims {
		if claim.Label == export.BlockedEvidenceLabel {
			return domain.ErrParentKeyMismatch
		}
		if claim.Label != export.ScopeConflictEvidenceLabel {
			continue
		}
		count++
		claim.Label = domain.AgentQuestionClaimLabel
		if !reflect.DeepEqual(claim, question) {
			return domain.ErrParentKeyMismatch
		}
	}
	if count != 1 {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// ScopeConflictCommand reconstructs the one concluding command against the
// authenticated question. Non-concluding commands cannot stand in for consent.
func (tx *ReadTx) ScopeConflictCommand(ctx context.Context, item domain.AttentionItem) (domain.Command, error) {
	if item.AgentQuestion == nil || item.AgentQuestion.ScopeConflict == nil || item.Status != domain.StatusResolved || item.DecidedAt == nil {
		return domain.Command{}, domain.ErrParentKeyMismatch
	}
	if err := tx.gateScopeConflictQuestion(ctx, item); err != nil {
		return domain.Command{}, err
	}
	commands, err := tx.ListCommandsForItem(ctx, item.ID)
	if err != nil {
		return domain.Command{}, err
	}
	var matched []domain.Command
	for _, command := range commands {
		if command.ItemVersion+1 == item.ItemVersion && command.PRHeadSHA == item.PRHeadSHA &&
			slices.Equal(command.ArtifactDigests, item.ArtifactDigests) &&
			(command.Action == domain.ActionAnswerWithoutRetry || command.Action == domain.ActionStop) {
			matched = append(matched, command)
		}
	}
	if len(matched) != 1 {
		return domain.Command{}, domain.ErrParentKeyMismatch
	}
	return matched[0], nil
}

// ScopeDecisionForCandidate authenticates any scope claim on this run's exact
// candidate, including omission of a required decision by a publisher caller.
func (tx *ReadTx) ScopeDecisionForCandidate(ctx context.Context, runID domain.RunID, head string) (*domain.ScopeDecisionFacts, error) {
	if runID == "" {
		return nil, nil
	}
	run, err := tx.GetRun(ctx, runID)
	// Legacy and development cards may name no persisted run. They cannot
	// acquire a scope decision: the caller still compares the absent fact.
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Stages and attempts are durable workflow order. Stop at this candidate
	// so later attempts cannot rewrite historical cards.
	var exports []domain.ExecutionExport
	target := -1
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			record, err := tx.GetExecutionExportRecord(ctx, attempt.InvocationID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			exports = append(exports, record)
			if record.HeadSHA == head {
				target = len(exports) - 1
			}
		}
	}
	if target < 0 {
		return nil, nil
	}
	var decision *domain.ScopeDecisionFacts
	var requiredPaths []string
	for _, record := range exports[:target+1] {
		claims, err := tx.GetAgentClaims(ctx, record.InvocationID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, claim := range claims {
			if claim.Label != export.ScopeConflictEvidenceLabel {
				continue
			}
			item, err := tx.GetAttentionItemRecord(ctx, domain.ItemID("question-"+string(record.InvocationID)))
			if err != nil {
				return nil, err
			}
			if item.AgentQuestion == nil || item.AgentQuestion.ScopeConflict == nil {
				return nil, domain.ErrParentKeyMismatch
			}
			requiredPaths = append(requiredPaths, item.AgentQuestion.ScopeConflict.Paths...)
			if record.HeadSHA != head {
				continue
			}
			command, err := tx.ScopeConflictCommand(ctx, item)
			if err != nil {
				return nil, err
			}
			if command.Action != domain.ActionAnswerWithoutRetry || decision != nil {
				return nil, domain.ErrParentKeyMismatch
			}
			facts := item.AgentQuestion.ScopeConflict
			decision = &domain.ScopeDecisionFacts{
				Paths: slices.Clone(facts.Paths), DeclaredPaths: slices.Clone(facts.DeclaredPaths),
				HeadSHA: head, CommandID: command.CommandID, Answer: command.Message, DecidedAt: *item.DecidedAt,
			}
			if err := decision.Validate(); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range requiredPaths {
		if decision == nil || !slices.Contains(decision.Paths, p) {
			return nil, fmt.Errorf("candidate has an unresolved earlier scope conflict: %w", domain.ErrParentKeyMismatch)
		}
	}
	return decision, nil
}
