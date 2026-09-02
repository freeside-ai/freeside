package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// productionQuestionItemID names the agent_question item a blocked
// implementation terminal creates.
func productionQuestionItemID(invocationID domain.InvocationID) domain.ItemID {
	return domain.ItemID("question-" + string(invocationID))
}

// recordProductionQuestion turns a blocked implementation terminal into one
// agent_question item inside the terminal's recording transaction. The
// decisions are re-read from the invocation's persisted claims and blob,
// never from the result, and the outcome record the observation projection
// authenticates against is written when the driver did not already write
// it. No publication candidate, task, or execution_failure item exists for
// a blocked terminal.
func (e *Engine) recordProductionQuestion(
	ctx context.Context, tx *store.WriteTx, run domain.Run,
	terminal productionTerminalRecord, createdAt time.Time,
) error {
	if e.artifacts == nil {
		return errors.New("blocked implementation terminal requires an artifact store")
	}
	claims, err := tx.GetAgentClaims(ctx, terminal.InvocationID)
	if err != nil {
		return fmt.Errorf("blocked terminal %q claims: %w", terminal.InvocationID, err)
	}
	var blockedClaim *domain.AgentClaim
	for index := range claims {
		claim := claims[index]
		if claim.Label != export.BlockedEvidenceLabel {
			continue
		}
		if blockedClaim != nil || claim.Provenance.ProducerInvocationID != terminal.InvocationID {
			return fmt.Errorf("blocked terminal %q claims are ambiguous: %w",
				terminal.InvocationID, domain.ErrParentKeyMismatch)
		}
		blockedClaim = &claim
	}
	if blockedClaim == nil {
		return fmt.Errorf("blocked terminal %q has no %s claim: %w",
			terminal.InvocationID, export.BlockedEvidenceLabel, domain.ErrParentKeyMismatch)
	}
	body, err := e.readArtifactBlob(blockedClaim.Digest, int64(domain.MaxBlockedOutcomeBytes))
	if err != nil {
		return fmt.Errorf("blocked terminal %q outcome: %w", terminal.InvocationID, err)
	}
	blocked, err := domain.DecodeBlockedOutcome(body)
	if err != nil {
		return fmt.Errorf("blocked terminal %q outcome: %w", terminal.InvocationID, err)
	}
	if terminal.Summary != exec.TruncateSummary(blocked.Decisions[0].Question) {
		return fmt.Errorf("blocked terminal %q summary disagrees with its outcome: %w",
			terminal.InvocationID, domain.ErrParentKeyMismatch)
	}
	if err := ensureBlockedOutcomeRecord(ctx, tx, terminal, createdAt); err != nil {
		return err
	}
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID}
	names, err := tx.DisplayNamesFor(ctx, run.ProjectID, subject)
	if err != nil {
		return err
	}
	questionClaim := *blockedClaim
	questionClaim.Label = domain.AgentQuestionClaimLabel
	kind := blocked.Kind
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionQuestionItemID(terminal.InvocationID), ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            blocked.Decisions[0].Question,
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop},
		AgentClaims:       []domain.AgentClaim{questionClaim},
		AgentQuestion: &domain.AgentQuestionFacts{
			Stage: domain.StageNameImplementation, InvocationID: terminal.InvocationID,
			Kind: &kind, Decisions: blocked.Decisions,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		CreatedAt: &createdAt, DisplayNames: names,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

// ensureBlockedOutcomeRecord converges on the driver's write-once blocked
// outcome, or writes it when the driver in use records none, so the
// observation projection and the signet run authentication see one
// authority for the terminal.
func ensureBlockedOutcomeRecord(
	ctx context.Context, tx *store.WriteTx, terminal productionTerminalRecord, recordedAt time.Time,
) error {
	stored, err := tx.GetExecutionOutcomeRecord(ctx, terminal.InvocationID)
	if err == nil {
		if stored.Status != domain.ExecutionOutcomeBlocked || stored.Summary != terminal.Summary {
			return fmt.Errorf("blocked terminal %q disagrees with outcome record %q: %w",
				terminal.InvocationID, stored.Status, domain.ErrParentKeyMismatch)
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, terminal.InvocationID)
	if err != nil {
		return err
	}
	return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: terminal.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeBlocked, Summary: terminal.Summary, RecordedAt: recordedAt,
	})
}

// readArtifactBlob reads one persisted evidence blob within a byte bound and
// re-verifies its content address before the bytes are interpreted.
func (e *Engine) readArtifactBlob(digest domain.Digest, limit int64) ([]byte, error) {
	reader, err := e.artifacts.Open(digest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("artifact %s exceeds %d bytes", digest, limit)
	}
	if domain.Digest(contentaddr.Sum(body)) != digest {
		return nil, fmt.Errorf("artifact %s does not match its digest", digest)
	}
	return body, nil
}
