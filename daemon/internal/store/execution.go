package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrRepositoryUntrusted is returned when an admission that must run against a
// trusted repository names one with no approved trust profile to bind its
// canonical numeric identity. It is deliberately distinct from ErrNotFound:
// the admission row exists and was refused, which is not the same as having no
// record.
var ErrRepositoryUntrusted = errors.New("admission names a repository with no approved trust profile")

// The durable execution record (plan §5.3, §5.7): what admitted one stage
// attempt, and what that attempt exported. Both are write-once and
// daemon-internal, so the writes live on InternalTx with non-Put names (the
// #38 invariant) and rows carry no entity_version/as_of_revision. The engine
// records an admission inside the same Write that appends its attempt, which
// InternalTx being embedded in WriteTx makes possible: an attempt with no
// audited class, or an admission for an attempt that was never appended, is
// exactly what a split would leave behind after a crash.
//
// Two boundaries protect these rows, and they answer different questions. The
// record is self-certifying, so decode rejects a body whose fields no longer
// resolve to its content address; that catches partial corruption and any
// edit that did not recompute the digest, not an actor with full write access
// to the database, who could recompute it. The re-gate is the half that keeps
// meaning: a snapshot admitted under an older, weaker floor stops reading as
// admissible the moment policy raises it, while historical waiver-bearing
// records remain readable only after the encrypted backup gate passes.

const (
	recordExecutionAdmissionSQL = `
INSERT INTO execution_admissions
    (invocation_id, id, run_id, stage_id, attempt_id, operating_mode, auth_identity_id,
     agent_digest, enrollment_id, enrollment_generation, admitted_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`
	selectExecutionAdmissionBodySQL = `SELECT body FROM execution_admissions WHERE invocation_id = ?`
	getExecutionAdmissionSQL        = `
SELECT invocation_id, id, run_id, stage_id, attempt_id, operating_mode, auth_identity_id,
       agent_digest, enrollment_id, enrollment_generation, admitted_at, body
FROM execution_admissions WHERE invocation_id = ?`
	// Ordered by rowid (insertion order), never by the RFC3339Nano admitted_at
	// column: trailing zeros are trimmed, so sub-second instants misorder
	// lexicographically.
	listRunExecutionAdmissionsSQL = `
SELECT invocation_id, id, run_id, stage_id, attempt_id, operating_mode, auth_identity_id,
       agent_digest, enrollment_id, enrollment_generation, admitted_at, body
FROM execution_admissions WHERE run_id = ? ORDER BY rowid`
	listExecutionAdmissionsSQL = `
SELECT invocation_id, id, run_id, stage_id, attempt_id, operating_mode, auth_identity_id,
       agent_digest, enrollment_id, enrollment_generation, admitted_at, body
FROM execution_admissions ORDER BY rowid`
	listActiveIdentityExecutionCandidatesSQL = `
SELECT admission.invocation_id
FROM execution_admissions AS admission
JOIN outbox AS dispatch
    ON dispatch.idempotency_key = admission.invocation_id
WHERE admission.auth_identity_id = ?
  AND admission.invocation_id <> ?
  AND dispatch.status IN ('dispatching', 'dispatched')`

	recordExecutionExportSQL = `
INSERT INTO execution_exports
    (invocation_id, admission_id, observed_base_sha, head_sha, manifest_digest,
     evidence_manifest_digest, commit_plan_present, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`
	selectExecutionExportBodySQL = `SELECT body FROM execution_exports WHERE invocation_id = ?`
	getExecutionExportSQL        = `
SELECT invocation_id, admission_id, observed_base_sha, head_sha, manifest_digest,
       evidence_manifest_digest, commit_plan_present, recorded_at, body
FROM execution_exports WHERE invocation_id = ?`

	recordCurrentImportStartSQL = `
INSERT INTO current_import_starts (invocation_id, admission_id, body)
VALUES (?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`
	selectCurrentImportStartBodySQL = `SELECT body FROM current_import_starts WHERE invocation_id = ?`
	getCurrentImportStartSQL        = `
SELECT invocation_id, admission_id, body
FROM current_import_starts WHERE invocation_id = ?`

	recordExecutionOutcomeSQL = `
INSERT INTO execution_outcomes
    (invocation_id, admission_id, status, summary, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`
	selectExecutionOutcomeBodySQL = `SELECT body FROM execution_outcomes WHERE invocation_id = ?`
	getExecutionOutcomeSQL        = `
SELECT invocation_id, admission_id, status, summary, recorded_at, body
FROM execution_outcomes WHERE invocation_id = ?`

	recordExportRejectionSQL = `
INSERT INTO export_rejections
    (invocation_id, admission_id, recorded_at, body)
VALUES (?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`
	selectExportRejectionBodySQL = `SELECT body FROM export_rejections WHERE invocation_id = ?`
	getExportRejectionSQL        = `
SELECT invocation_id, admission_id, recorded_at, body
FROM export_rejections WHERE invocation_id = ?`
)

// RecordExecutionAdmission persists the spawn-time record for one attempt. It
// re-gates the record against current policy before writing, so an admission
// the running daemon would not grant cannot be recorded as though it had, and
// it cross-checks the attempt against the run aggregate in the same
// transaction: an admission is a claim about an attempt, and a claim about an
// attempt the run does not carry is refused.
//
// The attempt cross-check is a write-time check only. A run's stages and
// attempts are append-only (domain.ValidateRunTransition), so an attempt that
// existed when the admission was recorded cannot later stop existing; a
// read-time repeat would cost a run read per row and rule out nothing.
//
// Write-once: a byte-identical replay converges on the stored row, and a
// second admission of the same invocation with different content fails with
// ErrImmutableConflict.
func (tx *WriteTx) RecordExecutionAdmission(ctx context.Context, admission domain.ExecutionAdmission) error {
	if admission.BackupEncryptionWaiver != nil {
		return fmt.Errorf("record execution admission %q: %w",
			admission.InvocationID, domain.ErrBackupEncryptionWaiverUnsupported)
	}
	body, err := encode(admission)
	if err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	if err := tx.gateAdmission(ctx, admission); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	// The operating-state half runs only here and at dispatch, not in
	// scanExecutionAdmission's re-gate: an operator stop or a blocking item
	// closes new unattended operation without making recorded history
	// unreadable (RequireUnattendedAdmissible). Running before putImmutable
	// applies only to a new record: an exact immutable replay returned above is
	// history, while dispatch re-checks operating state before starting work.
	if err := tx.RequireUnattendedAdmissible(ctx, admission); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	if err := tx.RequireBackendConformant(ctx, admission); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	if err := tx.requireRecordedAttempt(ctx, admission); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	existing, err := tx.existingBody(ctx, selectExecutionAdmissionBodySQL, admission.InvocationID)
	if err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	if existing != nil {
		if string(existing) != body {
			return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, ErrImmutableConflict)
		}
		return nil
	}
	if err := tx.RequireIdentityExecutionCapacity(ctx, admission); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	var identity any
	if admission.AuthIdentityID != nil {
		identity = *admission.AuthIdentityID
	}
	var agentDigest, enrollmentID, enrollmentGeneration any
	if binding := admission.AgentBinding; binding != nil {
		agentDigest = string(binding.AgentDigest)
		enrollmentID = binding.EnrollmentID
		enrollmentGeneration = binding.EnrollmentGeneration
	}
	inserted, err := tx.putImmutableInserted(ctx, recordExecutionAdmissionSQL,
		[]any{
			admission.InvocationID, admission.ID, admission.RunID, admission.StageID,
			admission.AttemptID, admission.OperatingMode, identity,
			agentDigest, enrollmentID, enrollmentGeneration,
			formatTime(admission.AdmittedAt), body,
		},
		selectExecutionAdmissionBodySQL, []any{admission.InvocationID}, body)
	if err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	// The observation milestone rides only the inserting transaction so every
	// lane records it atomically with the fact; it is projection, not
	// authority (issue #394), its instant is the admission's own, and a
	// byte-identical replay against a pre-0024 database must not backfill it
	// (the migration's no-backfill rule).
	if !inserted {
		return nil
	}
	if admission.AuthIdentityID != nil {
		if err := tx.MarkOutboxDispatching(ctx, string(admission.InvocationID)); err != nil {
			return fmt.Errorf("reserve execution dispatch %q: %w", admission.InvocationID, err)
		}
	}
	invocation := admission.InvocationID
	if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
		RunID: admission.RunID, Kind: domain.MilestoneInvocationAdmitted,
		InvocationID: &invocation, RecordedAt: admission.AdmittedAt,
	}); err != nil {
		return fmt.Errorf("record execution admission %q: %w", admission.InvocationID, err)
	}
	return nil
}

// ActiveIdentityExecutionCount derives current inference occupancy from the
// write-once execution record: an admission is active after its durable
// pre-start reservation or driver-accepted handoff, and until either mutually
// exclusive terminal row exists. There is no counter to leak across a crash.
func (tx *ReadTx) ActiveIdentityExecutionCount(
	ctx context.Context, identityID domain.AuthIdentityID,
) (int, error) {
	if _, err := tx.GetAuthIdentity(ctx, identityID); err != nil {
		return 0, fmt.Errorf("count active executions for auth identity %q: %w", identityID, err)
	}
	return tx.activeIdentityExecutionCount(ctx, identityID, "")
}

func (tx *ReadTx) activeIdentityExecutionCount(
	ctx context.Context, identityID domain.AuthIdentityID, exclude domain.InvocationID,
) (int, error) {
	rows, err := tx.tx.QueryContext(ctx, listActiveIdentityExecutionCandidatesSQL, identityID, exclude)
	if err != nil {
		return 0, fmt.Errorf("list active executions for auth identity %q: %w", identityID, err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var invocationID domain.InvocationID
		if err := rows.Scan(&invocationID); err != nil {
			return 0, fmt.Errorf("list active executions for auth identity %q: %w", identityID, err)
		}
		if _, err := tx.GetExecutionExportRecord(ctx, invocationID); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("validate execution export %q: %w", invocationID, err)
		}
		if _, err := tx.GetExecutionOutcomeRecord(ctx, invocationID); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("validate execution outcome %q: %w", invocationID, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list active executions for auth identity %q: %w", identityID, err)
	}
	return count, nil
}

// RequireIdentityExecutionCapacity is the transactional scheduling gate. The
// store begins writes with SQLite's immediate lock, so this derived count and
// the admission insert serialize as one decision across concurrent callers.
// A durable pre-start reservation or driver-accepted handoff counts, closing
// the cross-daemon interval between admission and Start. A synchronous driver
// refusal releases that reservation, so an input-materialization refusal does
// not consume an execution slot. A replay excludes its own invocation so the
// write-once convergence contract remains intact. Provider-free clean
// verification carries no identity and is outside this limit by definition.
func (tx *ReadTx) RequireIdentityExecutionCapacity(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	if admission.AuthIdentityID == nil {
		return nil
	}
	identity, err := tx.GetAuthIdentity(ctx, *admission.AuthIdentityID)
	if err != nil {
		return fmt.Errorf("load auth identity %q parallelism: %w", *admission.AuthIdentityID, err)
	}
	active, err := tx.activeIdentityExecutionCount(
		ctx, identity.ID, admission.InvocationID,
	)
	if err != nil {
		return err
	}
	if active >= identity.MaxParallelExecutions {
		return fmt.Errorf("auth identity %q has %d active executions at limit %d: %w",
			identity.ID, active, identity.MaxParallelExecutions,
			domain.ErrIdentityParallelismExhausted)
	}
	return nil
}

// LookupExecutionAdmission reconstructs one attempt's admission and reports
// separately whether a row exists at all.
//
// The separation is the point. A caller deciding "does this attempt have an
// audited class?" must not learn the answer by classifying an error, because
// the gate itself can fail with a not-found (an unattended admission whose
// trusted profile is gone), and reading that as "no record" would accept work whose
// reconstruction had explicitly failed closed. Here absence is a boolean and
// every error is a failure.
func (tx *ReadTx) LookupExecutionAdmission(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionAdmission, bool, error) {
	row := tx.tx.QueryRowContext(ctx, getExecutionAdmissionSQL, id)
	admission, err := tx.scanExecutionAdmission(ctx, row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.ExecutionAdmission{}, false, nil
	case err != nil:
		return domain.ExecutionAdmission{}, false, fmt.Errorf("get execution admission %q: %w", id, err)
	}
	if admission.InvocationID != id {
		return domain.ExecutionAdmission{}, false,
			fmt.Errorf("get execution admission %q: %w", id, errRowInconsistent)
	}
	return admission, true, nil
}

// GetExecutionAdmission reconstructs one attempt's admission, reporting a
// missing row as ErrNotFound. Callers that must distinguish an absent record
// from a refused one use LookupExecutionAdmission instead.
func (tx *ReadTx) GetExecutionAdmission(ctx context.Context, id domain.InvocationID) (domain.ExecutionAdmission, error) {
	admission, found, err := tx.LookupExecutionAdmission(ctx, id)
	if err != nil {
		return domain.ExecutionAdmission{}, err
	}
	if !found {
		return domain.ExecutionAdmission{}, fmt.Errorf("get execution admission %q: %w", id, ErrNotFound)
	}
	return admission, nil
}

// getExecutionAdmissionForWrite loads an admission for a boundary that accepts
// new caller-supplied state. Review-configuration recovery makes one parked
// admission reconstructible, but cannot authorize a new record derived from
// that superseded profile.
func (tx *ReadTx) getExecutionAdmissionForWrite(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionAdmission, error) {
	admission, err := scanExecutionAdmissionRecord(tx.tx.QueryRowContext(ctx, getExecutionAdmissionSQL, id))
	if err != nil {
		return domain.ExecutionAdmission{}, fmt.Errorf(
			"get execution admission %q: %w", id, notFoundOr(err))
	}
	if admission.InvocationID != id {
		return domain.ExecutionAdmission{}, fmt.Errorf(
			"get execution admission %q: %w", id, errRowInconsistent)
	}
	if err := tx.gateAdmission(ctx, admission); err != nil {
		return domain.ExecutionAdmission{}, fmt.Errorf("get execution admission %q: %w", id, err)
	}
	return admission, nil
}

// GetExecutionAdmissionRecord authenticates the immutable recorded admission
// without re-applying mutable current admission policy. It is for terminal
// history and backup closure only; any path that may still start, recover, or
// accept work must use GetExecutionAdmission.
func (tx *ReadTx) GetExecutionAdmissionRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionAdmission, error) {
	admission, err := scanExecutionAdmissionRecord(
		tx.tx.QueryRowContext(ctx, getExecutionAdmissionSQL, id),
	)
	if err != nil {
		return domain.ExecutionAdmission{}, fmt.Errorf(
			"get execution admission record %q: %w", id, notFoundOr(err))
	}
	if admission.InvocationID != id {
		return domain.ExecutionAdmission{}, fmt.Errorf(
			"get execution admission record %q: %w", id, errRowInconsistent)
	}
	return admission, nil
}

// ListRunExecutionAdmissions reconstructs every admission recorded for a run,
// in insertion order. It reuses the same reconstruction function as the
// single-record Get, so the re-gate cannot be missed on one path.
func (tx *ReadTx) ListRunExecutionAdmissions(ctx context.Context, runID domain.RunID) ([]domain.ExecutionAdmission, error) {
	return tx.listRunExecutionAdmissions(ctx, runID, true)
}

// ListRunExecutionAdmissionRecords returns immutable admission history without
// re-applying current policy. Presentation and audit projections use it so a
// later floor or trust-profile change cannot erase the fact that an invocation
// was admitted and may have incurred cost.
func (tx *ReadTx) ListRunExecutionAdmissionRecords(
	ctx context.Context, runID domain.RunID,
) ([]domain.ExecutionAdmission, error) {
	rows, err := tx.tx.QueryContext(ctx, listExecutionAdmissionsSQL)
	if err != nil {
		return nil, fmt.Errorf("list execution admission records for run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports any deferred-close failure
	var admissions []domain.ExecutionAdmission
	for rows.Next() {
		admission, err := scanExecutionAdmissionRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list execution admission records for run %q: %w", runID, err)
		}
		if admission.RunID == runID {
			admissions = append(admissions, admission)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list execution admission records for run %q: %w", runID, err)
	}
	return admissions, nil
}

func (tx *ReadTx) listRunExecutionAdmissions(
	ctx context.Context, runID domain.RunID, currentPolicy bool,
) ([]domain.ExecutionAdmission, error) {
	rows, err := tx.tx.QueryContext(ctx, listRunExecutionAdmissionsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list execution admissions for run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports any deferred-close failure
	var admissions []domain.ExecutionAdmission
	for rows.Next() {
		var admission domain.ExecutionAdmission
		if currentPolicy {
			admission, err = tx.scanExecutionAdmission(ctx, rows)
		} else {
			admission, err = scanExecutionAdmissionRecord(rows)
		}
		if err != nil {
			return nil, fmt.Errorf("list execution admissions for run %q: %w", runID, err)
		}
		admissions = append(admissions, admission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list execution admissions for run %q: %w", runID, err)
	}
	return admissions, nil
}

// scanExecutionAdmission is the single reconstruction path (see scanner):
// scan, decode, cross-check the extracted columns against the body, and
// re-run the admission gate. Both the Get and the List go through it.
func (tx *ReadTx) scanExecutionAdmission(ctx context.Context, row scanner) (domain.ExecutionAdmission, error) {
	admission, err := scanExecutionAdmissionRecord(row)
	if err != nil {
		return domain.ExecutionAdmission{}, err
	}
	if err := tx.gateReconstructedAdmission(ctx, admission); err != nil {
		return domain.ExecutionAdmission{}, err
	}
	return admission, nil
}

// scanExecutionAdmissionRecord authenticates the durable record without
// applying current admission policy. Backup closure uses this narrower
// reconstruction because retired or currently refused admissions still name
// blobs a restore must preserve.
func scanExecutionAdmissionRecord(row scanner) (domain.ExecutionAdmission, error) {
	var (
		invocationID         string
		id                   string
		runID                string
		stageID              string
		attemptID            string
		operatingMode        string
		authIdentityID       sql.NullString
		agentDigest          sql.NullString
		enrollmentID         sql.NullString
		enrollmentGeneration sql.NullInt64
		admittedAt           string
		body                 []byte
	)
	if err := row.Scan(&invocationID, &id, &runID, &stageID, &attemptID,
		&operatingMode, &authIdentityID, &agentDigest, &enrollmentID,
		&enrollmentGeneration, &admittedAt, &body); err != nil {
		return domain.ExecutionAdmission{}, err
	}
	admission, err := decode[domain.ExecutionAdmission](body)
	if err != nil {
		return domain.ExecutionAdmission{}, err
	}
	if string(admission.InvocationID) != invocationID || string(admission.ID) != id ||
		string(admission.RunID) != runID || string(admission.StageID) != stageID ||
		string(admission.AttemptID) != attemptID ||
		string(admission.OperatingMode) != operatingMode ||
		!authIdentityColumnEqual(authIdentityID, admission.AuthIdentityID) ||
		!agentBindingColumnsEqual(agentDigest, enrollmentID, enrollmentGeneration, admission.AgentBinding) ||
		!timeColumnEqual(admittedAt, admission.AdmittedAt) {
		return domain.ExecutionAdmission{}, errRowInconsistent
	}
	return admission, nil
}

// agentBindingColumnsEqual reports whether the extracted nullable agent
// columns agree with the decoded body's binding: all absent on a legacy
// admission, or all present and matching on a v4 one. Without this the
// enrollment foreign key would be decorative, since reconstruction would take
// the body's word for a binding no trusted row backs.
func agentBindingColumnsEqual(
	agentDigest, enrollmentID sql.NullString, generation sql.NullInt64,
	want *domain.AdmissionAgentBinding,
) bool {
	if want == nil {
		return !agentDigest.Valid && !enrollmentID.Valid && !generation.Valid
	}
	return agentDigest.Valid && agentDigest.String == string(want.AgentDigest) &&
		enrollmentID.Valid && enrollmentID.String == string(want.EnrollmentID) &&
		generation.Valid && generation.Int64 == int64(want.EnrollmentGeneration)
}

// authIdentityColumnEqual reports whether the extracted nullable column names
// the same identity as the decoded body: both absent, or both the same id.
// Without this the foreign key is decorative, since reconstruction would take
// the body's word for an identity binding no trusted row backs.
func authIdentityColumnEqual(column sql.NullString, want *domain.AuthIdentityID) bool {
	if !column.Valid || want == nil {
		return !column.Valid && want == nil
	}
	return column.String == string(*want)
}

// evidenceDigestColumnEqual reports whether the extracted nullable column
// names the same evidence manifest as the decoded body: both absent, or both
// the same digest.
func evidenceDigestColumnEqual(column sql.NullString, want *domain.Digest) bool {
	if !column.Valid || want == nil {
		return !column.Valid && want == nil
	}
	return column.String == string(*want)
}

// gateAdmission re-runs the trusted admission gate against the policy this
// transaction carries: the checks that define what the record means under
// current policy, so they hold on every reconstruction path. §5.7's
// operating-state conditions — no operator stop in force, no blocking
// system_health item — are deliberately not here but in
// RequireUnattendedAdmissible, which runs when an admission is recorded and
// when a stored one dispatches: they are preconditions on new unattended
// operation, and re-running them at reconstruction would make recorded
// history unreadable the moment an operator stops. This gate is the store's
// enforcement of the half the record cannot check for itself, and it fails
// closed: an unconfigured floor admits nothing, exactly as a nil
// approved-recipe set approves nothing.
//
// A historical record carrying the retired §5.7 waiver still gets one
// identity check: the repository identity the record names must match the one
// the repository's approved trust profile carries. That profile is
// human-approved state the record cannot write, so the name-to-id pair stops
// being self-asserted and the legacy field cannot validate its own target.
func (tx *ReadTx) gateAdmission(ctx context.Context, admission domain.ExecutionAdmission) error {
	return tx.gateAdmissionWithReviewConfigurationRecovery(ctx, admission, false)
}

// gateReconstructedAdmission re-gates a durable admission read. A command-backed
// review-configuration recovery is authority to reconstruct the parked run
// under its superseded profile, but it does not relax the gate for a new
// caller-supplied admission.
func (tx *ReadTx) gateReconstructedAdmission(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	return tx.gateAdmissionWithReviewConfigurationRecovery(ctx, admission, true)
}

func (tx *ReadTx) gateAdmissionWithReviewConfigurationRecovery(
	ctx context.Context, admission domain.ExecutionAdmission, allowReviewConfigurationRecovery bool,
) error {
	policy := tx.admissionPolicy
	if admission.OperatingMode == domain.ModeUnattended {
		health, err := tx.transactionBackupHealth(ctx)
		if err != nil {
			return fmt.Errorf("backup health: %w", err)
		}
		policy.BackupHealth = health
	}
	if err := domain.AdmittedUnder(admission, policy); err != nil {
		return err
	}
	// Unattended admissions must be anchored to a human-approved profile
	// because §5.7 lists one among their required conformance. The record's own
	// name-and-number pair is caller-supplied, so the profile is what makes it
	// evidence rather than an assertion. Historical waiver-bearing records
	// remain a subset of this class.
	// An admission naming a provider identity is only as good as that
	// identity's declaration: the foreign key proves a row exists, not that it
	// reconstructs. Reading it here means a record whose identity has a
	// malformed body fails closed, rather than a replay dispatching under
	// credential state whose concurrency, refresh, and snapshot declaration
	// could not be read back.
	if admission.AuthIdentityID != nil {
		if _, err := tx.GetAuthIdentity(ctx, *admission.AuthIdentityID); err != nil {
			return fmt.Errorf("admission %q names auth identity %q: %w",
				admission.InvocationID, *admission.AuthIdentityID, err)
		}
	}
	// A v4 admission's enrollment leg is re-gated here: the enrollment and the
	// exact generation it mounted must still reconstruct (through their own
	// account-binding and expiry-per-method gates), the enrollment must belong
	// to the admitted identity, and the recorded credential mode and store
	// manifest must agree with those records. A legacy admission (nil binding)
	// deliberately gets none of this: the §5.4 permanent legacy rule keeps its
	// admitted identity and credential mode and never resolves it against
	// current configuration.
	if binding := admission.AgentBinding; binding != nil {
		enrollment, err := tx.GetClientEnrollment(ctx, binding.EnrollmentID)
		if err != nil {
			return fmt.Errorf("admission %q names enrollment %q: %w",
				admission.InvocationID, binding.EnrollmentID, err)
		}
		if admission.AuthIdentityID == nil || enrollment.AuthIdentityID != *admission.AuthIdentityID {
			return fmt.Errorf("admission %q enrollment %q belongs to identity %q: %w",
				admission.InvocationID, binding.EnrollmentID, enrollment.AuthIdentityID,
				domain.ErrAdmissionDerivationMismatch)
		}
		if admission.CredentialMode != enrollment.CredentialMode {
			return fmt.Errorf("admission %q credential mode %q, enrollment carries %q: %w",
				admission.InvocationID, admission.CredentialMode, enrollment.CredentialMode,
				domain.ErrAdmissionDerivationMismatch)
		}
		generation, err := tx.GetEnrollmentGeneration(ctx, binding.EnrollmentID, binding.EnrollmentGeneration)
		if err != nil {
			return fmt.Errorf("admission %q names enrollment generation %q/%d: %w",
				admission.InvocationID, binding.EnrollmentID, binding.EnrollmentGeneration, err)
		}
		if generation.StoreManifestDigest != binding.StoreManifestDigest {
			return fmt.Errorf("admission %q store manifest %q, generation records %q: %w",
				admission.InvocationID, binding.StoreManifestDigest, generation.StoreManifestDigest,
				domain.ErrAdmissionDerivationMismatch)
		}
	}
	attempt, attemptErr := tx.GetProductionAttemptByRun(ctx, admission.RunID)
	if attemptErr != nil && !errors.Is(attemptErr, ErrNotFound) {
		return fmt.Errorf("admission %q capability retry attempt: %w", admission.InvocationID, attemptErr)
	}
	attemptManifest := attempt.CapabilityManifestDigest
	if (attemptManifest == nil) != (admission.CapabilityManifestDigest == nil) {
		return fmt.Errorf("admission %q capability manifest presence disagrees with attempt: %w",
			admission.InvocationID, domain.ErrAdmissionDerivationMismatch)
	}
	if admission.CapabilityManifestDigest != nil {
		if attemptErr != nil {
			return fmt.Errorf("admission %q capability retry attempt: %w", admission.InvocationID, attemptErr)
		}
		if *attemptManifest != *admission.CapabilityManifestDigest {
			return fmt.Errorf("admission %q capability manifest attempt binding: %w",
				admission.InvocationID, domain.ErrAdmissionDerivationMismatch)
		}
		resolved, err := tx.GetResolvedPolicy(ctx, admission.RunID)
		if err != nil {
			return fmt.Errorf("admission %q capability retry policy: %w", admission.InvocationID, err)
		}
		manifests, err := domain.CapabilityManifestsFromPolicy(resolved)
		if err != nil {
			return fmt.Errorf("admission %q capability retry policy: %w", admission.InvocationID, err)
		}
		matched := false
		for _, manifest := range manifests {
			if manifest.Digest == *admission.CapabilityManifestDigest &&
				manifest.EgressProfile == admission.EgressProfile {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("admission %q capability manifest is not policy-derived: %w",
				admission.InvocationID, domain.ErrAdmissionDerivationMismatch)
		}
	}
	if !admission.RequiresTrustProfile() {
		return nil
	}
	profile, err := tx.LatestTrustProfile(ctx, admission.Base.Repo)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Deliberately not reported as ErrNotFound: the admission row is
			// present and was refused. A caller asking "is there a record?"
			// must not read this refusal as "there is none".
			return fmt.Errorf("admission %q: no approved trust profile for %q: %w",
				admission.InvocationID, admission.Base.Repo, ErrRepositoryUntrusted)
		}
		return fmt.Errorf("admission %q: trusted profile for %q: %w",
			admission.InvocationID, admission.Base.Repo, err)
	}
	if profile.RepositoryID != admission.Base.RepositoryID {
		return fmt.Errorf("admission %q names repository %d, trusted profile for %q names %d: %w",
			admission.InvocationID, admission.Base.RepositoryID,
			admission.Base.Repo, profile.RepositoryID, domain.ErrRepositoryIdentityMismatch)
	}
	// The repository id survives a revision, so it cannot answer "was this
	// admitted under the profile that is approved now". The digest can: an
	// operator who activates a revised profile expects it to bind in-flight
	// work, not just the next run.
	if admission.TrustProfileDigest == nil || *admission.TrustProfileDigest != profile.ProfileDigest {
		// A review-configuration adoption authorizes the run, not merely the
		// parked review invocation that carries its decision. The transition's
		// full authority is re-derived on every read, including that its
		// superseding revision is still latest and changes only reviewer
		// configuration. Accept exactly that one recorded hop for admissions
		// the run minted under its superseded revision. Launch currency remains
		// an engine concern because only the engine knows the daemon's effective
		// reviewer-configuration digest.
		if allowReviewConfigurationRecovery && admission.TrustProfileDigest != nil {
			transition, found, recoveryErr := tx.LatestReviewConfigurationRecoveryTransition(
				ctx, admission.RunID,
			)
			if recoveryErr != nil &&
				!errors.Is(recoveryErr, domain.ErrReviewConfigRecoveryIneffective) {
				return recoveryErr
			}
			if recoveryErr == nil && found &&
				transition.SupersededProfileDigest == *admission.TrustProfileDigest {
				return nil
			}
		}
		return fmt.Errorf("admission %q was admitted under trust profile %v, %q now activates %s: %w",
			admission.InvocationID, admission.TrustProfileDigest,
			admission.Base.Repo, profile.ProfileDigest, domain.ErrTrustProfileSuperseded)
	}
	return nil
}

func (tx *ReadTx) transactionBackupHealth(ctx context.Context) (*domain.BackupHealth, error) {
	if tx.backupHealthEvaluated {
		return tx.backupHealth, tx.backupHealthErr
	}
	tx.backupHealthEvaluated = true
	if tx.backupHealthSource == nil {
		return nil, nil
	}
	state, err := tx.backupHealthContext(ctx)
	if err != nil {
		tx.backupHealthErr = err
		return nil, err
	}
	health, err := tx.backupHealthSource.BackupHealth(ctx, state)
	if err != nil {
		tx.backupHealthErr = err
		return nil, err
	}
	tx.backupHealth = &health
	return tx.backupHealth, nil
}

// requireRecordedAttempt checks that the run carries the exact attempt the
// admission claims. It reads the run in the writer's own transaction, so a
// concurrent writer cannot append the attempt after this passed.
func (tx *InternalTx) requireRecordedAttempt(ctx context.Context, admission domain.ExecutionAdmission) error {
	run, err := tx.GetRun(ctx, admission.RunID)
	if err != nil {
		return err
	}
	// The run's spec and policy digests are fixed at creation, and the record
	// is what the driver is later started from, so an admission that names
	// different ones would point execution at a spec or policy the run is not
	// bound to. They are available right here; take them from the run rather
	// than from the caller's word for them.
	if err := tx.requireBoundInputs(ctx, admission); err != nil {
		return err
	}
	if admission.SpecDigest != run.SpecDigest || admission.PolicyDigest != run.PolicyDigest {
		return fmt.Errorf(
			"admission %q names spec %s and policy %s, run %q is bound to %s and %s: %w",
			admission.InvocationID, admission.SpecDigest, admission.PolicyDigest,
			run.ID, run.SpecDigest, run.PolicyDigest, domain.ErrParentKeyMismatch)
	}
	for _, stage := range run.Stages {
		if stage.ID != admission.StageID {
			continue
		}
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID != admission.InvocationID {
				continue
			}
			if attempt.ID != admission.AttemptID {
				return fmt.Errorf("run %q binds invocation %q to attempt %q, admission names %q: %w",
					run.ID, admission.InvocationID, attempt.ID, admission.AttemptID, domain.ErrParentKeyMismatch)
			}
			return nil
		}
	}
	return fmt.Errorf("run %q carries no attempt %q for invocation %q in stage %q: %w",
		run.ID, admission.AttemptID, admission.InvocationID, admission.StageID, domain.ErrParentKeyMismatch)
}

// requireBoundInputs checks the record's input digest against the invocation's
// own durable binding. The agent invocation record *is* the statement of what
// inputs a turn was given (§5.14), so where one exists it is the authority and
// the admission may not claim different inputs; the digest is recomputed here
// rather than trusted from the caller.
//
// An invocation with no such record is left alone: not every stage kind binds
// its inputs through a conversation prefix or artifact list, and there is
// nothing to compare against for one that does not. This closes substitution
// wherever a durable input binding exists, which today is every dispatched
// invocation.
func (tx *InternalTx) requireBoundInputs(ctx context.Context, admission domain.ExecutionAdmission) error {
	invocation, err := tx.GetAgentInvocation(ctx, admission.InvocationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	bound, err := invocation.ComputeInputDigest()
	if err != nil {
		return err
	}
	if admission.InputDigest != bound {
		return fmt.Errorf("admission %q names input digest %s, invocation binds %s: %w",
			admission.InvocationID, admission.InputDigest, bound, domain.ErrParentKeyMismatch)
	}
	return nil
}

// RecordExecutionExport persists what one admitted attempt handed back. The
// admission is loaded in the same transaction (which re-gates it) and the
// binding is checked against it: an export whose observed base differs from
// the admitted base, or which names another admission, is refused before it
// can let the publication chain bind a head to an admission that never
// produced it.
//
// Write-once: a byte-identical replay converges, and a second, different
// export for one invocation fails with ErrImmutableConflict.
func (tx *WriteTx) RecordExecutionExport(ctx context.Context, export domain.ExecutionExport) error {
	return tx.recordExecutionExport(ctx, export, true)
}

// RecordExecutionExportRecord persists terminal export history against the
// authenticated immutable admission without re-applying mutable admission
// policy. It is the narrow store boundary used by the production workflow
// after it has independently authenticated production ownership and replay;
// callers accepting ordinary attended work use RecordExecutionExport.
func (tx *WriteTx) RecordExecutionExportRecord(
	ctx context.Context, export domain.ExecutionExport,
) error {
	return tx.recordExecutionExport(ctx, export, false)
}

func (tx *WriteTx) recordExecutionExport(
	ctx context.Context, export domain.ExecutionExport, requireCurrent bool,
) error {
	body, err := encode(export)
	if err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	var admission domain.ExecutionAdmission
	if requireCurrent {
		admission, err = tx.getExecutionAdmissionForWrite(ctx, export.InvocationID)
	} else {
		admission, err = tx.GetExecutionAdmissionRecord(ctx, export.InvocationID)
	}
	if err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	if err := domain.ValidateExportBinding(admission, export); err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	if err := tx.requireExecutionAuthorityAbsent(
		ctx, selectExecutionOutcomeBodySQL, export.InvocationID, "outcome",
	); err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	var evidence any
	if export.EvidenceManifestDigest != nil {
		evidence = string(*export.EvidenceManifestDigest)
	}
	inserted, err := tx.putImmutableInserted(ctx, recordExecutionExportSQL,
		[]any{
			export.InvocationID, export.AdmissionID, export.ObservedBaseSHA, export.HeadSHA,
			export.ManifestDigest, evidence, export.CommitPlanPresent,
			formatTime(export.RecordedAt), body,
		},
		selectExecutionExportBodySQL, []any{export.InvocationID}, body)
	if err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	// Observation milestone beside the fact, only on the inserting
	// transaction (the no-backfill rule); the instant is the export's own
	// (issue #394).
	if !inserted {
		return nil
	}
	invocation := export.InvocationID
	if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
		RunID: admission.RunID, Kind: domain.MilestoneExecutionExportRecorded,
		InvocationID: &invocation, RecordedAt: export.RecordedAt,
	}); err != nil {
		return fmt.Errorf("record execution export %q: %w", export.InvocationID, err)
	}
	return nil
}

// GetExecutionExport reconstructs one attempt's export record, re-checking
// its binding to the admission it names. The admission read re-gates that
// record too, so a read of an export cannot succeed while the run it belongs
// to is no longer admissible.
func (tx *ReadTx) GetExecutionExport(ctx context.Context, id domain.InvocationID) (domain.ExecutionExport, error) {
	return tx.getExecutionExport(ctx, id, true)
}

// GetExecutionExportRecord authenticates immutable terminal history without
// re-applying mutable current admission policy.
func (tx *ReadTx) GetExecutionExportRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, error) {
	return tx.getExecutionExport(ctx, id, false)
}

func (tx *ReadTx) getExecutionExport(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionExport, error) {
	var (
		invocationID    string
		admissionID     string
		observedBaseSHA string
		headSHA         string
		manifestDigest  string
		evidenceDigest  sql.NullString
		commitPlan      bool
		recordedAt      string
		body            []byte
	)
	err := tx.tx.QueryRowContext(ctx, getExecutionExportSQL, id).
		Scan(&invocationID, &admissionID, &observedBaseSHA, &headSHA, &manifestDigest,
			&evidenceDigest, &commitPlan, &recordedAt, &body)
	if err != nil {
		return domain.ExecutionExport{}, fmt.Errorf("get execution export %q: %w", id, notFoundOr(err))
	}
	export, err := decode[domain.ExecutionExport](body)
	if err != nil {
		return domain.ExecutionExport{}, fmt.Errorf("get execution export %q: %w", id, err)
	}
	if export.InvocationID != id || string(export.InvocationID) != invocationID ||
		string(export.AdmissionID) != admissionID ||
		export.ObservedBaseSHA != observedBaseSHA || export.HeadSHA != headSHA ||
		string(export.ManifestDigest) != manifestDigest ||
		!evidenceDigestColumnEqual(evidenceDigest, export.EvidenceManifestDigest) ||
		export.CommitPlanPresent != commitPlan ||
		!timeColumnEqual(recordedAt, export.RecordedAt) {
		return domain.ExecutionExport{}, fmt.Errorf("get execution export %q: %w", id, errRowInconsistent)
	}
	var admission domain.ExecutionAdmission
	if requireCurrent {
		admission, err = tx.GetExecutionAdmission(ctx, id)
	} else {
		admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
	}
	if err != nil {
		return domain.ExecutionExport{}, fmt.Errorf("get execution export %q: %w", id, err)
	}
	if err := domain.ValidateExportBinding(admission, export); err != nil {
		return domain.ExecutionExport{}, fmt.Errorf("get execution export %q: %w", id, err)
	}
	return export, nil
}

// RecordCurrentImportStart persists the trusted marker that a released export
// entered the current-policy import lane. It binds to immutable admission
// history rather than current policy: recording the requirement must remain
// possible even when that very policy will refuse the subsequent import.
//
// Write-once: a byte-identical replay converges, and a marker naming different
// authority for the invocation fails with ErrImmutableConflict.
func (tx *InternalTx) RecordCurrentImportStart(
	ctx context.Context, start domain.CurrentImportStart,
) error {
	body, err := encode(start)
	if err != nil {
		return fmt.Errorf("record current import start %q: %w", start.InvocationID, err)
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, start.InvocationID)
	if err != nil {
		return fmt.Errorf("record current import start %q: %w", start.InvocationID, err)
	}
	if err := domain.ValidateCurrentImportStartBinding(admission, start); err != nil {
		return fmt.Errorf("record current import start %q: %w", start.InvocationID, err)
	}
	if err := tx.putImmutable(ctx, recordCurrentImportStartSQL,
		[]any{start.InvocationID, start.AdmissionID, body},
		selectCurrentImportStartBodySQL, []any{start.InvocationID}, body); err != nil {
		return fmt.Errorf("record current import start %q: %w", start.InvocationID, err)
	}
	return nil
}

// GetCurrentImportStart reconstructs the current-policy import marker and
// re-checks every extracted column plus its immutable admission binding.
func (tx *ReadTx) GetCurrentImportStart(
	ctx context.Context, id domain.InvocationID,
) (domain.CurrentImportStart, error) {
	var invocationID, admissionID string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, getCurrentImportStartSQL, id).
		Scan(&invocationID, &admissionID, &body)
	if err != nil {
		return domain.CurrentImportStart{}, fmt.Errorf(
			"get current import start %q: %w", id, notFoundOr(err))
	}
	start, err := decode[domain.CurrentImportStart](body)
	if err != nil {
		return domain.CurrentImportStart{}, fmt.Errorf(
			"get current import start %q: %w", id, err)
	}
	if start.InvocationID != id || invocationID != string(start.InvocationID) ||
		admissionID != string(start.AdmissionID) {
		return domain.CurrentImportStart{}, fmt.Errorf(
			"get current import start %q: %w", id, errRowInconsistent)
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
	if err != nil {
		return domain.CurrentImportStart{}, fmt.Errorf(
			"get current import start %q: %w", id, err)
	}
	if err := domain.ValidateCurrentImportStartBinding(admission, start); err != nil {
		return domain.CurrentImportStart{}, fmt.Errorf(
			"get current import start %q: %w", id, err)
	}
	return start, nil
}

// RecordExecutionOutcome persists a trusted non-export terminal outcome.
// The admission binding is checked in the same transaction, and the row is
// write-once so a replay converges while a changed status or summary refuses.
func (tx *WriteTx) RecordExecutionOutcome(
	ctx context.Context, outcome domain.ExecutionOutcome,
) error {
	body, err := encode(outcome)
	if err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	// A non-export outcome closes an invocation; it does not authorize more
	// work. Bind it to the immutable admission record so policy changing
	// while an attempt runs cannot make failed, canceled, or lost terminal
	// history impossible to persist.
	admission, err := tx.GetExecutionAdmissionRecord(ctx, outcome.InvocationID)
	if err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	if err := domain.ValidateOutcomeBinding(admission, outcome); err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	if err := tx.requireExecutionAuthorityAbsent(
		ctx, selectExecutionExportBodySQL, outcome.InvocationID, "export",
	); err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	inserted, err := tx.putImmutableInserted(ctx, recordExecutionOutcomeSQL,
		[]any{
			outcome.InvocationID, outcome.AdmissionID, outcome.Status,
			outcome.Summary, formatTime(outcome.RecordedAt), body,
		},
		selectExecutionOutcomeBodySQL, []any{outcome.InvocationID}, body)
	if err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	// Observation milestone beside the fact, only on the inserting
	// transaction (the no-backfill rule); the instant is the outcome's own,
	// and only the closed status class crosses into the projection — never
	// the summary text (issue #394).
	if !inserted {
		return nil
	}
	invocation := outcome.InvocationID
	status := outcome.Status
	if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
		RunID: admission.RunID, Kind: domain.MilestoneExecutionOutcomeRecorded,
		InvocationID: &invocation, Outcome: &status, RecordedAt: outcome.RecordedAt,
	}); err != nil {
		return fmt.Errorf("record execution outcome %q: %w", outcome.InvocationID, err)
	}
	return nil
}

// requireExecutionAuthorityAbsent enforces that completed-export and
// non-export terminal authority stay mutually exclusive. It runs in the same
// write transaction as the eventual insert; SQLite serializes writers, and
// migration triggers repeat the invariant for raw database writes.
func (tx *InternalTx) requireExecutionAuthorityAbsent(
	ctx context.Context, query string, id domain.InvocationID, authority string,
) error {
	var body []byte
	err := tx.tx.QueryRowContext(ctx, query, id).Scan(&body)
	switch {
	case err == nil:
		return fmt.Errorf("execution %s already exists: %w",
			authority, ErrImmutableConflict)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("check execution %s: %w", authority, err)
	}
}

// GetExecutionOutcome reconstructs a non-export terminal outcome and
// re-checks every extracted column plus its current admission binding.
func (tx *ReadTx) GetExecutionOutcome(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, error) {
	return tx.getExecutionOutcome(ctx, id, true)
}

// GetExecutionOutcomeRecord authenticates immutable terminal history without
// re-applying mutable current admission policy.
func (tx *ReadTx) GetExecutionOutcomeRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, error) {
	return tx.getExecutionOutcome(ctx, id, false)
}

func (tx *ReadTx) getExecutionOutcome(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionOutcome, error) {
	var (
		invocationID string
		admissionID  string
		status       string
		summary      string
		recordedAt   string
		body         []byte
	)
	err := tx.tx.QueryRowContext(ctx, getExecutionOutcomeSQL, id).
		Scan(&invocationID, &admissionID, &status, &summary, &recordedAt, &body)
	if err != nil {
		return domain.ExecutionOutcome{}, fmt.Errorf(
			"get execution outcome %q: %w", id, notFoundOr(err))
	}
	outcome, err := decode[domain.ExecutionOutcome](body)
	if err != nil {
		return domain.ExecutionOutcome{}, fmt.Errorf("get execution outcome %q: %w", id, err)
	}
	if outcome.InvocationID != id || invocationID != string(outcome.InvocationID) ||
		admissionID != string(outcome.AdmissionID) ||
		status != string(outcome.Status) || summary != outcome.Summary ||
		!timeColumnEqual(recordedAt, outcome.RecordedAt) {
		return domain.ExecutionOutcome{}, fmt.Errorf(
			"get execution outcome %q: %w", id, errRowInconsistent)
	}
	var admission domain.ExecutionAdmission
	if requireCurrent {
		admission, err = tx.GetExecutionAdmission(ctx, id)
	} else {
		admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
	}
	if err != nil {
		return domain.ExecutionOutcome{}, fmt.Errorf("get execution outcome %q: %w", id, err)
	}
	if err := domain.ValidateOutcomeBinding(admission, outcome); err != nil {
		return domain.ExecutionOutcome{}, fmt.Errorf("get execution outcome %q: %w", id, err)
	}
	return outcome, nil
}

// RecordExportRejection persists the diagnostic per-finding detail of a
// definitively rejected export. It binds to the immutable admission record —
// not current policy — because the record is diagnostic history: re-gating
// current policy could make it unrecordable exactly when an operator needs to
// diagnose the rejection. It is not held mutually exclusive with either
// terminal-authority row: the same rejection also records an
// ExecutionOutcome(failed), and this row is that outcome's finding detail, not
// a competing authority.
//
// Write-once: a byte-identical replay of the same rejection converges, and a
// different body for one invocation fails with ErrImmutableConflict.
func (tx *WriteTx) RecordExportRejection(
	ctx context.Context, rejection domain.ExportRejection,
) error {
	body, err := encode(rejection)
	if err != nil {
		return fmt.Errorf("record export rejection %q: %w", rejection.InvocationID, err)
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, rejection.InvocationID)
	if err != nil {
		return fmt.Errorf("record export rejection %q: %w", rejection.InvocationID, err)
	}
	if err := domain.ValidateExportRejectionBinding(admission, rejection); err != nil {
		return fmt.Errorf("record export rejection %q: %w", rejection.InvocationID, err)
	}
	if err := tx.putImmutable(ctx, recordExportRejectionSQL,
		[]any{
			rejection.InvocationID, rejection.AdmissionID,
			formatTime(rejection.RecordedAt), body,
		},
		selectExportRejectionBodySQL, []any{rejection.InvocationID}, body); err != nil {
		return fmt.Errorf("record export rejection %q: %w", rejection.InvocationID, err)
	}
	return nil
}

// GetExportRejection reconstructs a rejection's diagnostic detail, re-checking
// every extracted column against the decoded body and binding it to the
// immutable admission record. It deliberately does not re-apply current
// admission policy: a rejection is terminal diagnostic history and must stay
// readable after the run is no longer admissible.
func (tx *ReadTx) GetExportRejection(
	ctx context.Context, id domain.InvocationID,
) (domain.ExportRejection, error) {
	var (
		invocationID string
		admissionID  string
		recordedAt   string
		body         []byte
	)
	err := tx.tx.QueryRowContext(ctx, getExportRejectionSQL, id).
		Scan(&invocationID, &admissionID, &recordedAt, &body)
	if err != nil {
		return domain.ExportRejection{}, fmt.Errorf(
			"get export rejection %q: %w", id, notFoundOr(err))
	}
	rejection, err := decode[domain.ExportRejection](body)
	if err != nil {
		return domain.ExportRejection{}, fmt.Errorf("get export rejection %q: %w", id, err)
	}
	if rejection.InvocationID != id || invocationID != string(rejection.InvocationID) ||
		admissionID != string(rejection.AdmissionID) ||
		!timeColumnEqual(recordedAt, rejection.RecordedAt) {
		return domain.ExportRejection{}, fmt.Errorf(
			"get export rejection %q: %w", id, errRowInconsistent)
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
	if err != nil {
		return domain.ExportRejection{}, fmt.Errorf("get export rejection %q: %w", id, err)
	}
	if err := domain.ValidateExportRejectionBinding(admission, rejection); err != nil {
		return domain.ExportRejection{}, fmt.Errorf("get export rejection %q: %w", id, err)
	}
	return rejection, nil
}
