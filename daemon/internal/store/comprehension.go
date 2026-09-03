package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Comprehension telemetry persistence (plan §8, §9; issue #924). Capability
// contracts and derived action surfaces are written through WriteTx; events and
// defects are written through the internal (non-synchronized) path, so
// recording one never advances the sync revision. The observation-only reads
// live on the dedicated ComprehensionReadTx (Store.ReadComprehension), which the
// ordinary policy-bearing transaction handles cannot reach, keeping the rows out
// of policy evaluation by construction (the §5.13 discipline, mirroring usage
// observations).

const (
	upsertDeviceCapabilityContractSQL = `
INSERT INTO device_capability_contracts (device_id, digest, body, registered_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (device_id) DO UPDATE SET
    digest        = excluded.digest,
    body          = excluded.body,
    registered_at = excluded.registered_at`
	selectDeviceCapabilityContractSQL = `
SELECT digest, body FROM device_capability_contracts WHERE device_id = ?`
	insertDecisionActionSurfaceSQL = `
INSERT INTO decision_action_surfaces
    (digest, device_id, item_id, item_decision_surface_digest,
     client_capability_digest, body, derived_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (digest) DO NOTHING`
	selectDecisionActionSurfaceBodySQL = `
SELECT body FROM decision_action_surfaces WHERE digest = ?`
	insertComprehensionEventSQL = `
INSERT INTO comprehension_events
    (device_id, event_id, item_id, kind, item_decision_surface_digest,
     decision_action_surface_digest, command_id, occurred_at, sequence,
     received_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (device_id, event_id) DO NOTHING`
	selectComprehensionEventBodySQL = `
SELECT body FROM comprehension_events WHERE device_id = ? AND event_id = ?`
	insertComprehensionDefectSQL = `
INSERT INTO comprehension_defects (item_id, claim_digest, recorded_at, reason)
VALUES (?, ?, ?, ?)
ON CONFLICT (item_id, claim_digest, recorded_at) DO NOTHING`
	listComprehensionEventsSQL = `
SELECT body FROM comprehension_events
ORDER BY item_id, occurred_at, device_id, event_id`
	listDecisionActionSurfacesSQL = `
SELECT body FROM decision_action_surfaces ORDER BY digest`
	listComprehensionDefectsSQL = `
SELECT item_id, claim_digest, recorded_at, reason
FROM comprehension_defects ORDER BY item_id, claim_digest, recorded_at`
	listDecidedCommandsSQL = `
SELECT c.body,
       json_extract(ai.body, '$.type'),
       json_extract(ai.body, '$.decided_at'),
       ai.subject_run_id
FROM commands c
JOIN attention_items ai ON ai.id = c.item_id
ORDER BY c.command_id`
)

// PutDeviceCapabilityContract records the actions a device's app build can
// present (plan §5.14). It is an upsert: a device that re-registers with a
// different action set replaces its row. registeredAt is store-stamped.
func (tx *WriteTx) PutDeviceCapabilityContract(
	ctx context.Context, contract domain.ClientCapabilityContract, registeredAt time.Time,
) error {
	body, err := encode(contract)
	if err != nil {
		return fmt.Errorf("put device capability contract %q: %w", contract.DeviceID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, upsertDeviceCapabilityContractSQL,
		contract.DeviceID, contract.Digest, body, formatTime(registeredAt)); err != nil {
		return fmt.Errorf("put device capability contract %q: %w", contract.DeviceID, err)
	}
	return nil
}

// GetDeviceCapabilityContract returns a device's current capability contract,
// or a wrapped ErrNotFound when the device has registered none. The decoded
// body re-validates and its digest must equal the extracted column.
func (tx *ReadTx) GetDeviceCapabilityContract(
	ctx context.Context, deviceID domain.DeviceID,
) (domain.ClientCapabilityContract, error) {
	var digest string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, selectDeviceCapabilityContractSQL, deviceID).Scan(&digest, &body)
	if err != nil {
		return domain.ClientCapabilityContract{}, fmt.Errorf(
			"get device capability contract %q: %w", deviceID, notFoundOr(err))
	}
	contract, err := decode[domain.ClientCapabilityContract](body)
	if err != nil {
		return domain.ClientCapabilityContract{}, fmt.Errorf(
			"get device capability contract %q: %w", deviceID, err)
	}
	if contract.DeviceID != deviceID || string(contract.Digest) != digest {
		return domain.ClientCapabilityContract{}, fmt.Errorf(
			"get device capability contract %q: %w", deviceID, errRowInconsistent)
	}
	return contract, nil
}

// PutDecisionActionSurface records a derived action surface, or returns the
// existing row when one already exists for its content-address digest. The
// surface is immutable, so an insert conflict is a byte-identical replay; a
// divergent stored body under the same digest fails closed.
func (tx *WriteTx) PutDecisionActionSurface(
	ctx context.Context, surface domain.DecisionActionSurface, derivedAt time.Time,
) (domain.DecisionActionSurface, error) {
	body, err := encode(surface)
	if err != nil {
		return domain.DecisionActionSurface{}, fmt.Errorf(
			"put decision action surface %q: %w", surface.Digest, err)
	}
	inserted, err := tx.putImmutableInserted(ctx, insertDecisionActionSurfaceSQL,
		[]any{
			surface.Digest, surface.DeviceID, surface.ItemID, surface.ItemDecisionSurfaceDigest,
			surface.ClientCapabilityDigest, body, formatTime(derivedAt),
		},
		selectDecisionActionSurfaceBodySQL, []any{surface.Digest}, body)
	if err != nil {
		return domain.DecisionActionSurface{}, fmt.Errorf(
			"put decision action surface %q: %w", surface.Digest, err)
	}
	if inserted {
		return surface, nil
	}
	return tx.GetDecisionActionSurface(ctx, surface.Digest)
}

// GetDecisionActionSurface returns the derived action surface with the given
// content-address digest, or a wrapped ErrNotFound.
func (tx *ReadTx) GetDecisionActionSurface(
	ctx context.Context, digest domain.Digest,
) (domain.DecisionActionSurface, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx, selectDecisionActionSurfaceBodySQL, digest).Scan(&body)
	if err != nil {
		return domain.DecisionActionSurface{}, fmt.Errorf(
			"get decision action surface %q: %w", digest, notFoundOr(err))
	}
	surface, err := decode[domain.DecisionActionSurface](body)
	if err != nil {
		return domain.DecisionActionSurface{}, fmt.Errorf(
			"get decision action surface %q: %w", digest, err)
	}
	if surface.Digest != digest {
		return domain.DecisionActionSurface{}, fmt.Errorf(
			"get decision action surface %q: %w", digest, errRowInconsistent)
	}
	return surface, nil
}

// RecordComprehensionEvent appends one event on the internal (non-synchronized)
// path, or returns the recorded row unchanged when the (device_id, event_id)
// idempotency key already exists. It follows the delivery-receipt discipline:
// it records the fact and has no version precondition. The item and device
// foreign keys are a backstop; the ingestion boundary gates them first.
func (tx *InternalTx) RecordComprehensionEvent(
	ctx context.Context, event domain.ComprehensionEvent,
) (domain.ComprehensionEvent, error) {
	body, err := encode(event)
	if err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf(
			"record comprehension event %q/%q: %w", event.DeviceID, event.EventID, err)
	}
	var surfaceDigest, commandID any
	if event.DecisionActionSurfaceDigest != nil {
		surfaceDigest = string(*event.DecisionActionSurfaceDigest)
	}
	if event.CommandID != "" {
		commandID = event.CommandID
	}
	if _, err := tx.tx.ExecContext(ctx, insertComprehensionEventSQL,
		event.DeviceID, event.EventID, event.ItemID, string(event.Kind),
		event.ItemDecisionSurfaceDigest, surfaceDigest, commandID,
		formatTime(event.OccurredAt), event.Sequence, formatTime(event.ReceivedAt), body,
	); err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf(
			"record comprehension event %q/%q: %w", event.DeviceID, event.EventID, err)
	}
	var storedBody []byte
	if err := tx.tx.QueryRowContext(ctx, selectComprehensionEventBodySQL,
		event.DeviceID, event.EventID).Scan(&storedBody); err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf(
			"record comprehension event %q/%q: %w", event.DeviceID, event.EventID, err)
	}
	stored, err := decode[domain.ComprehensionEvent](storedBody)
	if err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf(
			"record comprehension event %q/%q: %w", event.DeviceID, event.EventID, err)
	}
	if stored.DeviceID != event.DeviceID || stored.EventID != event.EventID {
		return domain.ComprehensionEvent{}, fmt.Errorf(
			"record comprehension event %q/%q: %w", event.DeviceID, event.EventID, errRowInconsistent)
	}
	return stored, nil
}

// RecordComprehensionDefect appends one operator-recorded defect on the
// internal path. It is idempotent on its (item_id, claim_digest, recorded_at)
// key.
func (tx *InternalTx) RecordComprehensionDefect(
	ctx context.Context, defect domain.ComprehensionDefect,
) error {
	if err := defect.Validate(); err != nil {
		return fmt.Errorf("record comprehension defect %q: %w", defect.ItemID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertComprehensionDefectSQL,
		defect.ItemID, defect.ClaimDigest, formatTime(defect.RecordedAt), defect.Reason,
	); err != nil {
		return fmt.Errorf("record comprehension defect %q: %w", defect.ItemID, err)
	}
	return nil
}

// DecidedCommand is one accepted command joined to its item's type, decided_at,
// and subject run, the input the §9 measures compute over. DecidedAt is the
// zero time when the item is not decided; SubjectRunID is empty when the item
// carries no run subject.
type DecidedCommand struct {
	Command      domain.Command
	ItemType     domain.AttentionType
	DecidedAt    time.Time
	SubjectRunID domain.RunID
}

// ComprehensionReadTx is the dedicated observation-only read surface. It does
// not embed ReadTx, so admission and policy callers cannot reach these rows
// through their ordinary transaction handle.
type ComprehensionReadTx struct {
	tx *sql.Tx
}

// ReadComprehension runs fn against the observation-only comprehension surface,
// mirroring ReadUsage. Ordinary Read callbacks cannot access these rows.
func (s *Store) ReadComprehension(ctx context.Context, fn func(*ComprehensionReadTx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin comprehension read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&ComprehensionReadTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit comprehension read: %w", err)
	}
	return nil
}

// ListComprehensionEvents lists every recorded event, ordered by item then
// occurrence, for the §9 measures.
func (tx *ComprehensionReadTx) ListComprehensionEvents(ctx context.Context) ([]domain.ComprehensionEvent, error) {
	rows, err := tx.tx.QueryContext(ctx, listComprehensionEventsSQL)
	if err != nil {
		return nil, fmt.Errorf("list comprehension events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []domain.ComprehensionEvent
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("list comprehension events: %w", err)
		}
		event, err := decode[domain.ComprehensionEvent](body)
		if err != nil {
			return nil, fmt.Errorf("list comprehension events: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list comprehension events: %w", err)
	}
	return events, nil
}

// ListDecisionActionSurfaces lists every derived action surface.
func (tx *ComprehensionReadTx) ListDecisionActionSurfaces(ctx context.Context) ([]domain.DecisionActionSurface, error) {
	rows, err := tx.tx.QueryContext(ctx, listDecisionActionSurfacesSQL)
	if err != nil {
		return nil, fmt.Errorf("list decision action surfaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var surfaces []domain.DecisionActionSurface
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("list decision action surfaces: %w", err)
		}
		surface, err := decode[domain.DecisionActionSurface](body)
		if err != nil {
			return nil, fmt.Errorf("list decision action surfaces: %w", err)
		}
		surfaces = append(surfaces, surface)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decision action surfaces: %w", err)
	}
	return surfaces, nil
}

// ListComprehensionDefects lists every operator-recorded defect.
func (tx *ComprehensionReadTx) ListComprehensionDefects(ctx context.Context) ([]domain.ComprehensionDefect, error) {
	rows, err := tx.tx.QueryContext(ctx, listComprehensionDefectsSQL)
	if err != nil {
		return nil, fmt.Errorf("list comprehension defects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var defects []domain.ComprehensionDefect
	for rows.Next() {
		var itemID, claimDigest, recordedAt, reason string
		if err := rows.Scan(&itemID, &claimDigest, &recordedAt, &reason); err != nil {
			return nil, fmt.Errorf("list comprehension defects: %w", err)
		}
		parsed, err := parseTime(recordedAt)
		if err != nil {
			return nil, fmt.Errorf("list comprehension defects: %w", err)
		}
		defect := domain.ComprehensionDefect{
			ItemID: domain.ItemID(itemID), ClaimDigest: domain.Digest(claimDigest),
			RecordedAt: parsed, Reason: reason,
		}
		if err := defect.Validate(); err != nil {
			return nil, fmt.Errorf("list comprehension defects: %w", err)
		}
		defects = append(defects, defect)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list comprehension defects: %w", err)
	}
	return defects, nil
}

// ListDecidedCommands lists every accepted command joined to its item's type,
// decided_at, and subject run, for the §9 open-to-decision, reversal, and
// override measures.
func (tx *ComprehensionReadTx) ListDecidedCommands(ctx context.Context) ([]DecidedCommand, error) {
	rows, err := tx.tx.QueryContext(ctx, listDecidedCommandsSQL)
	if err != nil {
		return nil, fmt.Errorf("list decided commands: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var decided []DecidedCommand
	for rows.Next() {
		var (
			body       []byte
			itemType   string
			decidedAt  sql.NullString
			subjectRun sql.NullString
		)
		if err := rows.Scan(&body, &itemType, &decidedAt, &subjectRun); err != nil {
			return nil, fmt.Errorf("list decided commands: %w", err)
		}
		command, _, _, err := decodeStoredCommand(body)
		if err != nil {
			return nil, fmt.Errorf("list decided commands: %w", err)
		}
		row := DecidedCommand{
			Command:      command,
			ItemType:     domain.AttentionType(itemType),
			SubjectRunID: domain.RunID(subjectRun.String),
		}
		if decidedAt.Valid && decidedAt.String != "" {
			parsed, err := parseTime(decidedAt.String)
			if err != nil {
				return nil, fmt.Errorf("list decided commands: %w", err)
			}
			row.DecidedAt = parsed
		}
		decided = append(decided, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decided commands: %w", err)
	}
	return decided, nil
}
