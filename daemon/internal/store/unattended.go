package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordUnattendedOperationTransitionSQL = `
INSERT INTO unattended_operation_transitions (state, command_id, reason, occurred_at)
VALUES (?, ?, ?, ?)`
	latestUnattendedOperationTransitionSQL = `
SELECT state, command_id, reason, occurred_at
FROM unattended_operation_transitions ORDER BY id DESC LIMIT 1`
	listOpenAttentionItemsByTypeSQL = `
SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, readiness_summary, readiness_detail, yield_history, entity_version, as_of_revision, body
FROM attention_items WHERE item_type = ? AND status = 'open' ORDER BY id`
	listOpenAttentionItemsForRunSQL = `
SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, readiness_summary, readiness_detail, yield_history, entity_version, as_of_revision, body
FROM attention_items WHERE subject_run_id = ? AND status = 'open' ORDER BY id`
	attentionRunBodyLookupSQL = `
SELECT id, item_type, status, subject_run_id, body
FROM attention_items ORDER BY id`
	// The lookup columns can only fail open by omission: a row whose column
	// diverges from its canonical body is invisible to the WHERE clause
	// above, so the per-row cross-check in scanAttentionItemSnapshot never
	// sees it. This one SQL pass proves the column view agrees with the body
	// view for every row (COALESCE mirrors the 0017 backfill's treatment of
	// a body the extraction cannot read), so an omitted mismatch fails the
	// query instead of silently shrinking the blocking set.
	attentionColumnDivergenceSQL = `
SELECT COUNT(*) FROM attention_items
WHERE item_type <> COALESCE(json_extract(body, '$.type'), '')
   OR status <> COALESCE(json_extract(body, '$.status'), '')
   OR COALESCE(health_posture, '') <> COALESCE(json_extract(body, '$.posture'), '')
   OR COALESCE(subject_run_id, '') <> COALESCE(json_extract(body, '$.subject.run_id'), '')`
	// Restrict the omission guard to rows that either independent view binds
	// to the selected run. json_each preserves object member order, so taking
	// the last case-insensitive match mirrors encoding/json's scalar-field
	// lookup. Repeated struct fields merge in Go, so candidate selection also
	// considers a matching run_id in every subject occurrence before rejecting
	// the duplicate as ambiguous. SQLite lower handles ASCII and replace handles
	// long s, the only non-ASCII simple-fold mate of a letter in these lookup
	// keys. Every body walk is json_valid-guarded, so an unreadable unrelated row
	// cannot abort the query; an unreadable selected row still disagrees with
	// its non-empty persisted binding and fails closed.
	attentionRunColumnDivergenceSQL = `
WITH top_level AS (
    SELECT subject_run_id, item_type, status,
           CASE WHEN json_valid(body) THEN
                COALESCE((
                    SELECT CASE WHEN type = 'object' THEN value ELSE '{}' END
                    FROM json_each(body)
                    WHERE lower(replace(key, 'ſ', 's')) = 'subject'
                    ORDER BY id DESC LIMIT 1
                ), '{}') ELSE '{}' END AS body_subject,
           CASE WHEN json_valid(body) THEN COALESCE((
                SELECT value FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'type'
                ORDER BY id DESC LIMIT 1
           ), '') ELSE '' END AS body_item_type,
           CASE WHEN json_valid(body) THEN COALESCE((
                SELECT value FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'status'
                ORDER BY id DESC LIMIT 1
           ), '') ELSE '' END AS body_status,
           CASE WHEN json_valid(body) THEN EXISTS (
                SELECT 1
                FROM json_each(body) AS subject_member
                JOIN json_each(CASE WHEN subject_member.type = 'object'
                                    THEN subject_member.value ELSE '{}' END) AS run_member
                WHERE lower(replace(subject_member.key, 'ſ', 's')) = 'subject'
                  AND lower(replace(run_member.key, 'ſ', 's')) = 'run_id'
                  AND NULLIF(run_member.value, '') = ?1
           ) ELSE 0 END AS body_mentions_selected_run,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'subject'
           ) ELSE 0 END AS body_subject_keys,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'subject' AND key <> 'subject'
           ) ELSE 0 END AS body_subject_aliases,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'type'
           ) ELSE 0 END AS body_type_keys,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'type' AND key <> 'type'
           ) ELSE 0 END AS body_type_aliases,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'status'
           ) ELSE 0 END AS body_status_keys,
           CASE WHEN json_valid(body) THEN (
                SELECT COUNT(*) FROM json_each(body)
                WHERE lower(replace(key, 'ſ', 's')) = 'status' AND key <> 'status'
           ) ELSE 0 END AS body_status_aliases
    FROM attention_items
), bindings AS (
    SELECT subject_run_id, item_type, status, body_item_type, body_status,
           NULLIF((
                SELECT value FROM json_each(body_subject)
                WHERE lower(replace(key, 'ſ', 's')) = 'run_id'
                ORDER BY id DESC LIMIT 1
           ), '') AS body_run_id,
           body_mentions_selected_run,
           body_subject_keys, body_subject_aliases,
           body_type_keys, body_type_aliases,
           body_status_keys, body_status_aliases,
           (SELECT COUNT(*) FROM json_each(body_subject)
            WHERE lower(replace(key, 'ſ', 's')) = 'run_id') AS body_run_id_keys,
           (SELECT COUNT(*) FROM json_each(body_subject)
            WHERE lower(replace(key, 'ſ', 's')) = 'run_id' AND key <> 'run_id') AS body_run_id_aliases
    FROM top_level
)
SELECT COUNT(*) FROM bindings
WHERE (subject_run_id = ?1 OR body_run_id = ?1 OR body_mentions_selected_run)
  AND (COALESCE(subject_run_id, '') <> COALESCE(body_run_id, '')
       OR item_type <> body_item_type
       OR status <> body_status
       OR body_subject_keys > 1
       OR body_subject_aliases > 0
       OR body_type_keys > 1
       OR body_type_aliases > 0
       OR body_status_keys > 1
       OR body_status_aliases > 0
       OR body_run_id_keys > 1
       OR body_run_id_aliases > 0)`
)

// RecordUnattendedOperationTransition appends one operator stop/resume
// decision (plan §4 stop_unattended; issue #319). Append-only by design: the
// log is the audit trail of operator intent, the latest row is the operating
// state, and a repeated state (a second stop while stopped) is a real
// recorded decision, not a conflict. Named Record, not Put: rows are never
// updated. Puts validate before writing: the transition must be authorized
// by the accepted command it names (requireTransitionCommand), the same
// binding reconstruction re-derives, so a row this boundary would refuse to
// write cannot be told apart from tampering when read back.
func (tx *InternalTx) RecordUnattendedOperationTransition(
	ctx context.Context, transition domain.UnattendedOperationTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	if err := tx.requireTransitionCommand(ctx, transition); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordUnattendedOperationTransitionSQL,
		transition.State, *transition.CommandID, transition.Reason,
		formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	return nil
}

// requireTransitionCommand re-derives a transition's authority from the
// immutable command it names: the command must reconstruct (running its own
// gates) and its accepted action must authorize the transition's state. The
// stored state is a decoded trust bit — "resumed" lifts a safety gate — so
// it is never trusted on its own: a single-column tamper flipping stopped to
// resumed fails closed here because the referenced command still says
// stop_unattended.
func (tx *ReadTx) requireTransitionCommand(
	ctx context.Context, transition domain.UnattendedOperationTransition,
) error {
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return fmt.Errorf("transition command %q: %w", *transition.CommandID, err)
	}
	authorizing, ok := transition.State.AuthorizingAction()
	if !ok {
		return fmt.Errorf("unattended operation state %q: %w",
			transition.State, domain.ErrInvalidUnattendedOperationState)
	}
	if command.Action != authorizing {
		return fmt.Errorf("transition %q backed by command %q with action %q: %w",
			transition.State, command.CommandID, command.Action,
			domain.ErrTransitionCommandMismatch)
	}
	return nil
}

// LatestUnattendedOperationTransition reconstructs the current operating
// state: the newest appended transition, with presence reported separately
// because an empty log is the legitimate "never stopped" state, not an error
// (the LookupExecutionAdmission shape).
func (tx *ReadTx) LatestUnattendedOperationTransition(
	ctx context.Context,
) (domain.UnattendedOperationTransition, bool, error) {
	var (
		state      string
		commandID  sql.NullString
		reason     string
		occurredAt string
	)
	err := tx.tx.QueryRowContext(ctx, latestUnattendedOperationTransitionSQL).
		Scan(&state, &commandID, &reason, &occurredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.UnattendedOperationTransition{}, false, nil
	case err != nil:
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition occurred_at %q: %w", occurredAt, err)
	}
	transition := domain.UnattendedOperationTransition{
		State:      domain.UnattendedOperationState(state),
		Reason:     reason,
		OccurredAt: at,
	}
	if commandID.Valid {
		transition.CommandID = &commandID.String
	}
	// Gets validate after reading: a row the current vocabulary cannot
	// express (an unknown state written by tampering or a future schema, or
	// one naming no command) fails closed instead of reconstructing as "not
	// stopped", and the state is re-derived from the immutable command the
	// row names (requireTransitionCommand) rather than trusted from the
	// column — a tampered resumed row does not lift the stop while its
	// command still says stop_unattended.
	if err := transition.Validate(); err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	if err := tx.requireTransitionCommand(ctx, transition); err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	return transition, true, nil
}

// RequireUnattendedAdmissible is the operating-state half of §5.7's
// unattended conditions, checked in the admitting transaction (issue #321):
// no operator stop in force, and no blocking system_health item. It is a
// recording-time precondition on new unattended operation — consulted when an
// admission is recorded and when a stored admission is about to dispatch —
// not part of a record's meaning, so reconstruction (scanExecutionAdmission)
// deliberately does not re-run it: an operator stop must not make recorded
// history unreadable (lists, exports, backup closure), it must stop what
// happens next.
//
// The blocking rule is typed, never an item-ID convention: an open
// system_health item blocks unless it carries a BlockingSupersession whose
// condition holds against this transaction's live policy (plan §4: "a
// validated configuration supersedes it"). The stored condition is a claim,
// not a verdict. Supersedes re-derives whether healthy encrypted backup
// evidence has retired a legacy waived-posture notice. Matched rows are fully
// reconstructed and re-gated (scanAttentionItemSnapshot) before they can
// block or be skipped; the extracted columns only select candidates.
func (tx *ReadTx) RequireUnattendedAdmissible(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	if admission.OperatingMode != domain.ModeUnattended {
		return nil
	}
	if err := tx.RequireUnattendedOperationOpen(ctx); err != nil {
		return fmt.Errorf("admission %q: %w", admission.InvocationID, err)
	}
	return nil
}

// RequireUnattendedOperationOpen is the whole operating-state predicate with
// no admission in hand: no operator stop in force, and no blocking open
// system_health item. It exists as one function so every consumer gates on
// both conditions or neither — the engine's per-pass check calls it for a
// dispatch whose operating mode is unknowable (no admission configuration),
// where "unknown fails closed" must mean the full gate, not whichever half
// was remembered.
func (tx *ReadTx) RequireUnattendedOperationOpen(ctx context.Context) error {
	latest, found, err := tx.LatestUnattendedOperationTransition(ctx)
	if err != nil {
		return err
	}
	if found && latest.State == domain.UnattendedStopped {
		return domain.ErrUnattendedOperationStopped
	}
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return err
	}
	policy := tx.admissionPolicy
	backupHealthLoaded := false
	for _, item := range items {
		if *item.Posture == domain.HealthPostureAdvisory {
			continue
		}
		if item.BlockingSupersession == nil {
			return fmt.Errorf("item %q: %w", item.ID, domain.ErrBlockingSystemHealth)
		}
		if !backupHealthLoaded {
			health, healthErr := tx.transactionBackupHealth(ctx)
			if healthErr != nil {
				return fmt.Errorf("item %q: backup health: %w", item.ID, healthErr)
			}
			policy.BackupHealth = health
			backupHealthLoaded = true
		}
		if err := item.BlockingSupersession.Supersedes(policy); err != nil {
			return fmt.Errorf("item %q: %w: %w", item.ID, domain.ErrBlockingSystemHealth, err)
		}
	}
	return nil
}

// ListOpenAttentionItems returns every open item of one type, in id order,
// selected by the extracted lookup columns and fully reconstructed through
// the shared scan (decode, cross-check, evidence re-gate), so a caller acting
// on the result acts on validated rows, never on column claims. Selection is
// guarded against omission too: the divergence count proves no row's columns
// disagree with its body, because a tampered column would otherwise hide an
// open blocking item from the WHERE clause with no scan left to refuse it.
// Consumers: the unattended admission gate above, and signet's stop
// transaction, which must find an existing resume-offering notice before
// raising another.
func (tx *ReadTx) ListOpenAttentionItems(
	ctx context.Context, itemType domain.AttentionType,
) ([]domain.AttentionItem, error) {
	var divergent int
	if err := tx.tx.QueryRowContext(ctx, attentionColumnDivergenceSQL).Scan(&divergent); err != nil {
		return nil, fmt.Errorf("list open %q items: column integrity: %w", itemType, err)
	}
	if divergent > 0 {
		return nil, fmt.Errorf("list open %q items: %d row(s) whose lookup columns diverge from their bodies: %w",
			itemType, divergent, errRowInconsistent)
	}
	rows, err := tx.tx.QueryContext(ctx, listOpenAttentionItemsByTypeSQL, itemType)
	if err != nil {
		return nil, fmt.Errorf("list open %q items: %w", itemType, err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.AttentionItem
	for rows.Next() {
		item, _, err := tx.scanAttentionItemSnapshot(ctx, rows)
		if err != nil {
			return nil, fmt.Errorf("list open %q items: %w", itemType, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list open %q items: %w", itemType, err)
	}
	return items, nil
}

// ListOpenAttentionItemsForRun returns every open item bound to one run, in
// id order. The independent subject_run_id column scopes the candidate set
// before body reconstruction and mutable evidence-policy gating, so corrupt
// or stale-policy rows for other runs do not block observation of this run.
// The scoped divergence guard still fails closed if either view would omit a
// selected row. The production-lifecycle supervisor (#795) is the consumer.
func (tx *ReadTx) ListOpenAttentionItemsForRun(
	ctx context.Context, runID domain.RunID,
) ([]domain.AttentionItem, error) {
	return tx.listOpenAttentionItemsForRun(ctx, runID, func(sc scanner) (domain.AttentionItem, Snapshot, error) {
		return tx.scanAttentionItemSnapshot(ctx, sc)
	})
}

// ListOpenAttentionItemRecordsForRun returns structurally authenticated open
// item records for one run without applying mutable evidence policy. It is a
// diagnostic-history primitive: callers must discard non-actionable records
// and reconstruct every actionable item through GetAttentionItem before using
// it. The production observer uses it so historical ready-item evidence cannot
// hide a published outcome while current actionable evidence still fails
// closed. "Structurally authenticated" includes the decision-surface re-gate
// (scanAttentionItemHistory): the identity is the item's own, so a record
// whose surface row is missing or disagrees is corrupt history, not history
// this primitive may hand back.
func (tx *ReadTx) ListOpenAttentionItemRecordsForRun(
	ctx context.Context, runID domain.RunID,
) ([]domain.AttentionItem, error) {
	return tx.listOpenAttentionItemsForRun(ctx, runID, func(sc scanner) (domain.AttentionItem, Snapshot, error) {
		return tx.scanAttentionItemHistory(ctx, sc)
	})
}

func (tx *ReadTx) listOpenAttentionItemsForRun(
	ctx context.Context,
	runID domain.RunID,
	scan func(scanner) (domain.AttentionItem, Snapshot, error),
) ([]domain.AttentionItem, error) {
	var divergent int
	if err := tx.tx.QueryRowContext(ctx, attentionRunColumnDivergenceSQL, runID).Scan(&divergent); err != nil {
		return nil, fmt.Errorf("list open items for run %q: column integrity: %w", runID, err)
	}
	if divergent > 0 {
		return nil, fmt.Errorf("list open items for run %q: %d row(s) whose lookup columns diverge from their bodies: %w",
			runID, divergent, errRowInconsistent)
	}
	if err := tx.checkAttentionRunBodyLookup(ctx, runID); err != nil {
		return nil, fmt.Errorf("list open items for run %q: column integrity: %w", runID, err)
	}
	rows, err := tx.tx.QueryContext(ctx, listOpenAttentionItemsForRunSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list open items for run %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.AttentionItem
	for rows.Next() {
		item, _, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("list open items for run %q: %w", runID, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list open items for run %q: %w", runID, err)
	}
	return items, nil
}

// checkAttentionRunBodyLookup closes the remaining parser-differential hole
// in the SQLite preflight above. SQLite JSON1 intentionally rejects nesting
// beyond 1,000 levels, while decode uses encoding/json and accepts a deeper
// otherwise-valid body. A tampered subject_run_id could hide that body from
// SQLite's dual-view candidate set, so this lightweight pass uses the same Go
// decoder to find candidate bodies before the indexed query reconstructs and
// policy-gates its selected rows. It only examines lookup fields: unrelated
// malformed or stale-policy rows remain isolated from the requested run.
func (tx *ReadTx) checkAttentionRunBodyLookup(ctx context.Context, runID domain.RunID) error {
	rows, err := tx.tx.QueryContext(ctx, attentionRunBodyLookupSQL)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id           string
			itemType     string
			status       string
			subjectRunID sql.NullString
			body         []byte
		)
		if err := rows.Scan(&id, &itemType, &status, &subjectRunID, &body); err != nil {
			return err
		}
		var lookup struct {
			Subject domain.Subject       `json:"subject"`
			Type    domain.AttentionType `json:"type"`
			Status  domain.ItemStatus    `json:"status"`
		}
		if err := json.Unmarshal(body, &lookup); err != nil {
			if subjectRunID.Valid && subjectRunID.String == string(runID) {
				return fmt.Errorf("item %q: %w", id, errRowInconsistent)
			}
			continue
		}
		bodyRunID := ""
		if lookup.Subject.RunID != nil {
			bodyRunID = string(*lookup.Subject.RunID)
		}
		if (!subjectRunID.Valid || subjectRunID.String != string(runID)) && bodyRunID != string(runID) {
			continue
		}
		columnRunID := ""
		if subjectRunID.Valid {
			columnRunID = subjectRunID.String
		}
		if columnRunID != bodyRunID || itemType != string(lookup.Type) || status != string(lookup.Status) {
			return fmt.Errorf("item %q: %w", id, errRowInconsistent)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}
