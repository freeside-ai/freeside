package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// CodexReviewOpaqueRecord is deliberately vocabulary-free: ward owns and
// validates the JSON body, while store owns immutable bytes and transitions.
type CodexReviewOpaqueRecord struct {
	Key        string
	State      string
	BodyDigest string
	Body       []byte
}

func codexReviewBodyDigest(body []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

func codexReviewBodyAuthority(body []byte) string {
	return codexReviewBodyDigest(body) + string(body)
}

func codexReviewStateBodyDigest(state string, body []byte) string {
	authority := make([]byte, 0, len(state)+1+len(body))
	authority = append(authority, state...)
	authority = append(authority, 0)
	authority = append(authority, body...)
	return codexReviewBodyDigest(authority)
}

func (tx *InternalTx) RecordCodexReviewWorkspace(
	ctx context.Context, sourceRunID, volume string, body []byte,
) error {
	if sourceRunID == "" || volume == "" || len(body) == 0 {
		return fmt.Errorf("put Codex review workspace: %w", ErrImmutableConflict)
	}
	bodyDigest := codexReviewBodyDigest(body)
	return tx.putImmutable(ctx, `INSERT INTO codex_review_workspaces
		(source_run_id, volume, body_digest, body) VALUES (?, ?, ?, ?)
		ON CONFLICT (source_run_id) DO NOTHING`,
		[]any{sourceRunID, volume, bodyDigest, string(body)},
		`SELECT body_digest || body FROM codex_review_workspaces WHERE source_run_id = ?`,
		[]any{sourceRunID}, codexReviewBodyAuthority(body))
}

func (tx *ReadTx) GetCodexReviewWorkspace(
	ctx context.Context, sourceRunID string,
) (CodexReviewOpaqueRecord, error) {
	var volume, bodyDigest string
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT volume, body_digest, body FROM codex_review_workspaces
		WHERE source_run_id = ?`, sourceRunID).Scan(&volume, &bodyDigest, &body); err != nil {
		return CodexReviewOpaqueRecord{}, fmt.Errorf("get Codex review workspace %q: %w",
			sourceRunID, notFoundOr(err))
	}
	return CodexReviewOpaqueRecord{Key: volume, BodyDigest: bodyDigest, Body: body}, nil
}

func (tx *ReadTx) ListCodexReviewWorkspaceIDs(ctx context.Context) ([]string, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT source_run_id FROM codex_review_workspaces ORDER BY source_run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (tx *InternalTx) DeleteCodexReviewWorkspace(
	ctx context.Context, sourceRunID, expectedVolume string, expectedBody []byte,
) error {
	expectedDigest := codexReviewBodyDigest(expectedBody)
	result, err := tx.tx.ExecContext(ctx, `DELETE FROM codex_review_workspaces
		WHERE source_run_id = ? AND volume = ? AND body_digest = ? AND body = ?`,
		sourceRunID, expectedVolume, expectedDigest, string(expectedBody))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	var currentVolume, currentDigest, currentBody string
	err = tx.tx.QueryRowContext(ctx, `SELECT volume, body_digest, body FROM codex_review_workspaces
		WHERE source_run_id = ?`, sourceRunID).Scan(&currentVolume, &currentDigest, &currentBody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("delete Codex review workspace %q: %w", sourceRunID, ErrImmutableConflict)
}

func (tx *InternalTx) FinalizeCodexReviewWorkspace(
	ctx context.Context, sourceRunID string, expectedBody, body []byte,
) error {
	expectedDigest := codexReviewBodyDigest(expectedBody)
	bodyDigest := codexReviewBodyDigest(body)
	result, err := tx.tx.ExecContext(ctx, `UPDATE codex_review_workspaces
		SET body_digest = ?, body = ?
		WHERE source_run_id = ? AND body_digest = ? AND body = ?`,
		bodyDigest, string(body), sourceRunID, expectedDigest, string(expectedBody))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	var currentDigest, currentBody string
	if err := tx.tx.QueryRowContext(ctx, `SELECT body_digest, body FROM codex_review_workspaces
		WHERE source_run_id = ?`, sourceRunID).Scan(&currentDigest, &currentBody); err != nil {
		return notFoundOr(err)
	}
	if currentDigest == bodyDigest && currentBody == string(body) {
		return nil
	}
	return ErrImmutableConflict
}

func (tx *InternalTx) BeginCodexReviewIntent(
	ctx context.Context, runID, state string, body []byte,
) error {
	if runID == "" || state != "preparing" || len(body) == 0 {
		return fmt.Errorf("begin Codex review intent: %w", ErrImmutableConflict)
	}
	bodyDigest := codexReviewStateBodyDigest(state, body)
	var currentState, currentDigest, currentBody string
	err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_intents
		WHERE run_id = ?`, runID).Scan(&currentState, &currentDigest, &currentBody)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.tx.ExecContext(ctx, `INSERT INTO codex_review_intents
			(run_id, state, body_digest, body) VALUES (?, ?, ?, ?)`,
			runID, state, bodyDigest, string(body))
		return err
	}
	if err != nil {
		return err
	}
	if currentDigest != codexReviewStateBodyDigest(currentState, []byte(currentBody)) {
		return fmt.Errorf("begin Codex review intent %q: %w", runID, ErrImmutableConflict)
	}
	if currentState == state && currentDigest == bodyDigest && currentBody == string(body) {
		return nil
	}
	if currentState != "closed" {
		return fmt.Errorf("begin Codex review intent %q from %q: %w",
			runID, currentState, ErrImmutableConflict)
	}
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM codex_review_bindings WHERE run_id = ?`, runID); err != nil {
		return err
	}
	result, err := tx.tx.ExecContext(ctx, `UPDATE codex_review_intents
		SET state = ?, body_digest = ?, body = ?
		WHERE run_id = ? AND state = 'closed' AND body_digest = ? AND body = ?`,
		state, bodyDigest, string(body), runID, currentDigest, currentBody)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	return fmt.Errorf("restart Codex review intent %q: %w", runID, ErrImmutableConflict)
}

func (tx *ReadTx) GetCodexReviewIntent(
	ctx context.Context, runID string,
) (CodexReviewOpaqueRecord, error) {
	var state, bodyDigest string
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_intents
		WHERE run_id = ?`, runID).Scan(&state, &bodyDigest, &body); err != nil {
		return CodexReviewOpaqueRecord{}, fmt.Errorf("get Codex review intent %q: %w",
			runID, notFoundOr(err))
	}
	return CodexReviewOpaqueRecord{Key: runID, State: state, BodyDigest: bodyDigest, Body: body}, nil
}

func (tx *ReadTx) ListCodexReviewIntentIDs(ctx context.Context) ([]string, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT run_id FROM codex_review_intents ORDER BY run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (tx *InternalTx) UpdateCodexReviewIntent(
	ctx context.Context, runID, expectedState, nextState string, expectedBody, nextBody []byte,
) error {
	expectedDigest := codexReviewStateBodyDigest(expectedState, expectedBody)
	nextDigest := codexReviewStateBodyDigest(nextState, nextBody)
	result, err := tx.tx.ExecContext(ctx, `UPDATE codex_review_intents
		SET state = ?, body_digest = ?, body = ?
		WHERE run_id = ? AND state = ? AND body_digest = ? AND body = ?`,
		nextState, nextDigest, string(nextBody), runID, expectedState, expectedDigest, string(expectedBody))
	if err != nil {
		return fmt.Errorf("update Codex review intent %q: %w", runID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var state, bodyDigest, body string
	err = tx.tx.QueryRowContext(ctx,
		`SELECT state, body_digest, body FROM codex_review_intents WHERE run_id = ?`, runID).
		Scan(&state, &bodyDigest, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("update Codex review intent %q: %w", runID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if state == nextState && bodyDigest == nextDigest && body == string(nextBody) {
		return nil
	}
	return fmt.Errorf("update Codex review intent %q from %q: %w",
		runID, expectedState, ErrImmutableConflict)
}

func (tx *InternalTx) RecordCodexReviewBinding(ctx context.Context, runID string, body []byte) error {
	if runID == "" || len(body) == 0 {
		return fmt.Errorf("put Codex review binding: %w", ErrImmutableConflict)
	}
	bodyDigest := codexReviewBodyDigest(body)
	return tx.putImmutable(ctx, `INSERT INTO codex_review_bindings (run_id, body_digest, body)
		VALUES (?, ?, ?) ON CONFLICT (run_id) DO NOTHING`,
		[]any{runID, bodyDigest, string(body)},
		`SELECT body_digest || body FROM codex_review_bindings WHERE run_id = ?`,
		[]any{runID}, codexReviewBodyAuthority(body))
}

func (tx *ReadTx) GetCodexReviewBinding(
	ctx context.Context, runID string,
) (CodexReviewOpaqueRecord, error) {
	var bodyDigest string
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT body_digest, body FROM codex_review_bindings
		WHERE run_id = ?`, runID).Scan(&bodyDigest, &body); err != nil {
		return CodexReviewOpaqueRecord{}, fmt.Errorf("get Codex review binding %q: %w",
			runID, notFoundOr(err))
	}
	return CodexReviewOpaqueRecord{Key: runID, BodyDigest: bodyDigest, Body: body}, nil
}

func (tx *InternalTx) RecordCodexReviewRequest(
	ctx context.Context, invocationID string, body []byte,
) error {
	bodyDigest := codexReviewBodyDigest(body)
	return tx.putImmutable(ctx, `INSERT INTO codex_review_requests (invocation_id, body_digest, body)
		VALUES (?, ?, ?) ON CONFLICT (invocation_id) DO NOTHING`,
		[]any{invocationID, bodyDigest, string(body)},
		`SELECT body_digest || body FROM codex_review_requests WHERE invocation_id = ?`,
		[]any{invocationID}, codexReviewBodyAuthority(body))
}

func (tx *ReadTx) GetCodexReviewRequest(
	ctx context.Context, invocationID string,
) (CodexReviewOpaqueRecord, error) {
	var bodyDigest string
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT body_digest, body FROM codex_review_requests
		WHERE invocation_id = ?`, invocationID).Scan(&bodyDigest, &body); err != nil {
		return CodexReviewOpaqueRecord{}, fmt.Errorf("get Codex review request %q: %w",
			invocationID, notFoundOr(err))
	}
	return CodexReviewOpaqueRecord{Key: invocationID, BodyDigest: bodyDigest, Body: body}, nil
}

func (tx *InternalTx) RecordCodexReviewOutcome(
	ctx context.Context, invocationID string, body []byte,
) error {
	bodyDigest := codexReviewStateBodyDigest("collected", body)
	result, err := tx.tx.ExecContext(ctx, `INSERT INTO codex_review_outcomes
        (invocation_id, state, body_digest, body)
        VALUES (?, 'collected', ?, ?) ON CONFLICT (invocation_id) DO NOTHING`,
		invocationID, bodyDigest, string(body))
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 1 {
		return err
	}
	var state, currentDigest, currentBody string
	if err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_outcomes
        WHERE invocation_id = ?`, invocationID).Scan(&state, &currentDigest, &currentBody); err != nil {
		return err
	}
	if currentBody == string(body) && currentDigest == codexReviewStateBodyDigest(state, []byte(currentBody)) {
		return nil
	}
	return ErrImmutableConflict
}

func (tx *ReadTx) GetCodexReviewOutcome(
	ctx context.Context, invocationID string,
) (CodexReviewOpaqueRecord, error) {
	var state, bodyDigest string
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_outcomes
        WHERE invocation_id = ?`, invocationID).Scan(&state, &bodyDigest, &body); err != nil {
		return CodexReviewOpaqueRecord{}, fmt.Errorf("get Codex review outcome %q: %w",
			invocationID, notFoundOr(err))
	}
	return CodexReviewOpaqueRecord{
		Key: invocationID, State: state, BodyDigest: bodyDigest, Body: body,
	}, nil
}

func (tx *ReadTx) ListCodexReviewOutcomeIDs(ctx context.Context) ([]string, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT invocation_id FROM codex_review_outcomes ORDER BY invocation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (tx *InternalTx) MarkCodexReviewOutcomeReady(
	ctx context.Context, invocationID string,
) error {
	var state, bodyDigest, body string
	if err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_outcomes
        WHERE invocation_id = ?`, invocationID).Scan(&state, &bodyDigest, &body); err != nil {
		return notFoundOr(err)
	}
	if bodyDigest != codexReviewStateBodyDigest(state, []byte(body)) {
		return ErrImmutableConflict
	}
	if state == "ready" {
		return nil
	}
	if state != "collected" {
		return ErrImmutableConflict
	}
	readyDigest := codexReviewStateBodyDigest("ready", []byte(body))
	result, err := tx.tx.ExecContext(ctx, `UPDATE codex_review_outcomes
        SET state = 'ready', body_digest = ?
        WHERE invocation_id = ? AND state = 'collected' AND body_digest = ? AND body = ?`,
		readyDigest, invocationID, bodyDigest, body)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return err
	}
	var currentState, currentDigest, currentBody string
	if err := tx.tx.QueryRowContext(ctx, `SELECT state, body_digest, body FROM codex_review_outcomes
        WHERE invocation_id = ?`, invocationID).Scan(&currentState, &currentDigest, &currentBody); err != nil {
		return notFoundOr(err)
	}
	if currentState == "ready" && currentDigest == readyDigest && currentBody == body {
		return nil
	}
	return ErrImmutableConflict
}
