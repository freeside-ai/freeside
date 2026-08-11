package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordCodexReenrollmentRecoverySQL = `
INSERT INTO codex_reenrollment_recovery_transitions
    (auth_identity_id, lease_fence, auth_store_digest, access_token_expires_at,
     command_id, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	latestCodexReenrollmentRecoverySQL = `
SELECT auth_identity_id, lease_fence, auth_store_digest, access_token_expires_at,
       command_id, reason, occurred_at
FROM codex_reenrollment_recovery_transitions
WHERE auth_identity_id = ? ORDER BY id DESC LIMIT 1`
)

// RecordCodexReenrollmentRecoveryTransition appends one command-backed
// resolution after re-deriving its carrier and latest verified operation.
func (tx *InternalTx) RecordCodexReenrollmentRecoveryTransition(
	ctx context.Context, transition domain.CodexReenrollmentRecoveryTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record codex re-enrollment recovery transition: %w", err)
	}
	if err := tx.requireCodexReenrollmentRecoveryCommand(ctx, transition); err != nil {
		return fmt.Errorf("record codex re-enrollment recovery transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordCodexReenrollmentRecoverySQL,
		transition.AuthIdentityID, transition.LeaseFence, transition.AuthStoreDigest,
		formatTime(transition.AccessTokenExpiresAt), *transition.CommandID,
		transition.Reason, formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record codex re-enrollment recovery transition: %w", err)
	}
	return nil
}

func (tx *ReadTx) requireCodexReenrollmentRecoveryCommand(
	ctx context.Context, transition domain.CodexReenrollmentRecoveryTransition,
) error {
	if err := transition.Validate(); err != nil {
		return err
	}
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return fmt.Errorf("codex re-enrollment recovery command %q: %w", *transition.CommandID, err)
	}
	if command.Action != transition.AuthorizingAction() {
		return fmt.Errorf("codex re-enrollment recovery backed by command %q with action %q: %w",
			command.CommandID, command.Action, domain.ErrTransitionCommandMismatch)
	}
	item, _, err := tx.GetAttentionItemSnapshot(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("codex re-enrollment recovery command %q item %q: %w",
			command.CommandID, command.ItemID, err)
	}
	if item.CodexReenrollmentRecoveryBinding == nil ||
		*item.CodexReenrollmentRecoveryBinding != transition.Binding() {
		return fmt.Errorf("codex re-enrollment recovery command %q item %q binding: %w",
			command.CommandID, item.ID, domain.ErrCodexReenrollmentBindingMismatch)
	}
	occurrence, err := CodexReenrollmentMarkerOccurrence(item, transition.AuthIdentityID)
	if err != nil {
		return err
	}
	if occurrence == 0 {
		return domain.ErrCodexReenrollmentMarkerMismatch
	}
	if err := validateCodexReenrollmentRecoveryCarrier(item, transition); err != nil {
		return err
	}
	latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, transition.AuthIdentityID)
	if err != nil {
		return err
	}
	if !found {
		return ErrCodexReenrollmentNotVerified
	}
	binding, err := latest.RecoveryBinding()
	if err != nil {
		return err
	}
	if transition.OccurredAt.Before(latest.Terminal.CompletedAt) {
		return fmt.Errorf("codex re-enrollment recovery predates verification: %w",
			ErrCodexReenrollmentNotVerified)
	}
	if binding != transition.Binding() {
		return domain.ErrCodexReenrollmentBindingMismatch
	}
	if command.ItemID != latest.MarkerItemID {
		return domain.ErrCodexReenrollmentMarkerMismatch
	}
	return nil
}

func validateCodexReenrollmentRecoveryCarrier(
	item domain.AttentionItem, transition domain.CodexReenrollmentRecoveryTransition,
) error {
	if item.Status != domain.StatusResolved || item.DecidedAt == nil ||
		!item.DecidedAt.Equal(transition.OccurredAt) {
		return fmt.Errorf("codex re-enrollment recovery carrier decision: %w",
			domain.ErrCodexReenrollmentMarkerMismatch)
	}
	return nil
}

// LatestCodexReenrollmentRecoveryTransition reconstructs and re-gates the
// newest accepted resolution for one identity.
func (tx *ReadTx) LatestCodexReenrollmentRecoveryTransition(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.CodexReenrollmentRecoveryTransition, bool, error) {
	var storedID, digest, expiry string
	var fence int64
	var commandID sql.NullString
	var reason, occurredAt string
	err := tx.tx.QueryRowContext(ctx, latestCodexReenrollmentRecoverySQL, id).Scan(
		&storedID, &fence, &digest, &expiry, &commandID, &reason, &occurredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CodexReenrollmentRecoveryTransition{}, false, nil
	}
	if err != nil {
		return domain.CodexReenrollmentRecoveryTransition{}, false, err
	}
	expiresAt, err := parseTime(expiry)
	if err != nil {
		return domain.CodexReenrollmentRecoveryTransition{}, false, err
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return domain.CodexReenrollmentRecoveryTransition{}, false, err
	}
	transition := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: domain.AuthIdentityID(storedID), LeaseFence: fence,
		AuthStoreDigest: domain.Digest(digest), AccessTokenExpiresAt: expiresAt,
		Reason: reason, OccurredAt: at,
	}
	if commandID.Valid {
		transition.CommandID = &commandID.String
	}
	if transition.AuthIdentityID != id {
		return domain.CodexReenrollmentRecoveryTransition{}, false, errRowInconsistent
	}
	if err := transition.Validate(); err != nil {
		return domain.CodexReenrollmentRecoveryTransition{}, false, err
	}
	if err := tx.requireCodexReenrollmentRecoveryCommand(ctx, transition); err != nil {
		return domain.CodexReenrollmentRecoveryTransition{}, false, err
	}
	return transition, true, nil
}

// CodexReenrollmentRecoveryCarrier returns the exact AttentionItem whose
// accepted command backs transition, after repeating the full authority gate.
func (tx *ReadTx) CodexReenrollmentRecoveryCarrier(
	ctx context.Context, transition domain.CodexReenrollmentRecoveryTransition,
) (domain.ItemID, error) {
	if err := tx.requireCodexReenrollmentRecoveryCommand(ctx, transition); err != nil {
		return "", err
	}
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return "", err
	}
	return command.ItemID, nil
}
