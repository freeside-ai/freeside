package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrHandoffJournalClosed is returned when an amendment or terminal close
// targets a record that already has an outcome. A closed record is immutable:
// one run has one durable lifecycle, ever.
var ErrHandoffJournalClosed = errors.New("handoff journal record is closed")

// ErrHandoffJournalProofConflict is returned when a proof amendment tries to
// replace a different value already committed for the run. Exact retries
// converge, but an earned proof is never rewritable.
var ErrHandoffJournalProofConflict = errors.New("handoff journal proof is already recorded differently")

// HandoffJournalOutcome is the store-owned persistence vocabulary. The ward
// adapter maps it one-for-one to ward.HandoffJournalOutcome without making the
// store import the runner boundary.
type HandoffJournalOutcome string

const (
	HandoffCompleted HandoffJournalOutcome = "completed"
	HandoffCanceled  HandoffJournalOutcome = "canceled"
	HandoffFailed    HandoffJournalOutcome = "failed"
	HandoffLoss      HandoffJournalOutcome = "loss"
)

// AllHandoffJournalOutcomes lists every valid HandoffJournalOutcome.
var AllHandoffJournalOutcomes = []HandoffJournalOutcome{
	HandoffCompleted, HandoffCanceled, HandoffFailed, HandoffLoss,
}

func (o HandoffJournalOutcome) valid() bool {
	switch o {
	case HandoffCompleted, HandoffCanceled, HandoffFailed, HandoffLoss:
		return true
	default:
		return false
	}
}

// HandoffJournalLease is the persisted reference to the exact mutation
// window the journal and lease transaction opened.
type HandoffJournalLease struct {
	AuthIdentityID domain.AuthIdentityID `json:"auth_identity_id"`
	Holder         domain.InvocationID   `json:"holder"`
	Fence          int64                 `json:"fence"`
	AcquiredAt     time.Time             `json:"acquired_at"`
	ExpiresAt      time.Time             `json:"expires_at"`
}

func (l HandoffJournalLease) validate() error {
	if l.AuthIdentityID == "" || l.Holder == "" {
		return errors.New("handoff journal lease identity and holder are required")
	}
	if l.Fence < 1 {
		return fmt.Errorf("handoff journal lease fence %d is not positive", l.Fence)
	}
	if l.AcquiredAt.IsZero() || l.ExpiresAt.IsZero() || !l.ExpiresAt.After(l.AcquiredAt) {
		return errors.New("handoff journal lease window is invalid")
	}
	return nil
}

// HandoffJournalState is the persistence projection of ward's prepared
// lifecycle-scoped provider state.
type HandoffJournalState struct {
	ConfigRootFingerprint     string `json:"config_root_fingerprint"`
	ContinuityFingerprint     string `json:"continuity_fingerprint"`
	SessionScratchFingerprint string `json:"session_scratch_fingerprint"`
	ConfigRootTarget          string `json:"config_root_target"`
	ContinuityTarget          string `json:"continuity_target"`
	SessionScratchTarget      string `json:"session_scratch_target"`
	ConfigRootReadOnly        bool   `json:"config_root_read_only"`
	ContinuityReadOnly        bool   `json:"continuity_read_only"`
	SessionScratchReadOnly    bool   `json:"session_scratch_read_only"`
	ConfigRootDigest          string `json:"config_root_digest"`
	ContinuityDigest          string `json:"continuity_digest"`
	SessionScratchDigest      string `json:"session_scratch_digest"`
}

func (s HandoffJournalState) validate() error {
	if s.ConfigRootFingerprint == "" || s.ContinuityFingerprint == "" ||
		s.SessionScratchFingerprint == "" || s.ConfigRootDigest == "" ||
		s.ContinuityDigest == "" || s.SessionScratchDigest == "" ||
		s.ConfigRootTarget == "" || s.ContinuityTarget == "" ||
		s.SessionScratchTarget == "" {
		return errors.New("handoff journal state binding is incomplete")
	}
	return nil
}

// Validate makes the extracted state binding eligible for the store's
// canonical encoder.
func (s HandoffJournalState) Validate() error { return s.validate() }

// HandoffJournalInstructions is the persistence projection of ward's
// explicit trusted instruction-bundle proof.
type HandoffJournalInstructions struct {
	CompositionVersion       string `json:"composition_version"`
	HostDigest               string `json:"host_digest"`
	RepositoryManifestDigest string `json:"repository_manifest_digest"`
	BundleDigest             string `json:"bundle_digest"`
}

func (i HandoffJournalInstructions) validate() error {
	if i.CompositionVersion == "" || i.HostDigest == "" ||
		i.RepositoryManifestDigest == "" || i.BundleDigest == "" {
		return errors.New("handoff journal instruction binding is incomplete")
	}
	return nil
}

// Validate makes the extracted instruction binding eligible for the store's
// canonical encoder.
func (i HandoffJournalInstructions) Validate() error { return i.validate() }

// HandoffJournalRecord is the store persistence shape mapped by the production
// ward adapter. It is daemon-internal bookkeeping, not a synchronized domain
// entity. Every extracted column is cross-checked against the validated body
// when reconstructed.
type HandoffJournalRecord struct {
	RunID                 string                      `json:"run_id"`
	OwnershipToken        string                      `json:"ownership_token"`
	SpecDigest            string                      `json:"spec_digest"`
	ObservedBaseSHA       string                      `json:"observed_base_sha"`
	CredentialPreDigest   string                      `json:"credential_pre_digest"`
	WriterComplete        bool                        `json:"writer_complete"`
	CancellationRequested bool                        `json:"cancellation_requested"`
	WriterFailureStatus   *int                        `json:"writer_failure_status"`
	State                 *HandoffJournalState        `json:"state"`
	Instructions          *HandoffJournalInstructions `json:"instructions"`
	Lease                 *HandoffJournalLease        `json:"lease"`
	ExportDir             string                      `json:"export_dir"`
	Outcome               *HandoffJournalOutcome      `json:"outcome"`
	OpenedAt              time.Time                   `json:"opened_at"`
}

// Validate re-runs the store-owned state and shape gates. Ward applies its
// stricter run/token/digest grammar again after the adapter reconstructs this
// value.
func (r HandoffJournalRecord) Validate() error {
	if r.RunID == "" || r.OwnershipToken == "" || r.SpecDigest == "" {
		return errors.New("handoff journal identity fields are required")
	}
	if r.OpenedAt.IsZero() {
		return errors.New("handoff journal opened_at is required")
	}
	if r.Lease != nil {
		if err := r.Lease.validate(); err != nil {
			return err
		}
	} else if r.CredentialPreDigest != "" {
		return errors.New("unleased handoff journal record carries a credential pre-digest")
	}
	if r.WriterComplete && r.Lease != nil && r.CredentialPreDigest == "" {
		return errors.New("completed leased writer has no credential pre-digest")
	}
	if r.WriterFailureStatus != nil {
		if *r.WriterFailureStatus < 1 || *r.WriterFailureStatus > 255 {
			return errors.New("writer failure status is outside 1..255")
		}
		if r.WriterComplete {
			return errors.New("writer cannot be both complete and failed")
		}
	}
	if r.State != nil {
		if err := r.State.validate(); err != nil {
			return err
		}
	}
	if r.Instructions != nil {
		if err := r.Instructions.validate(); err != nil {
			return err
		}
	}
	if r.ExportDir != "" && !filepath.IsAbs(r.ExportDir) {
		return errors.New("handoff journal export_dir is not absolute")
	}
	if r.Outcome != nil {
		if !r.Outcome.valid() {
			return fmt.Errorf("handoff journal outcome %q is invalid", *r.Outcome)
		}
		if *r.Outcome == HandoffCompleted && (!r.WriterComplete || r.ExportDir == "") {
			return errors.New("completed handoff journal record lacks writer or export proof")
		}
		if *r.Outcome == HandoffCanceled && !r.CancellationRequested {
			return errors.New("canceled handoff journal record lacks cancellation intent")
		}
		if *r.Outcome == HandoffFailed && r.WriterFailureStatus == nil {
			return errors.New("failed handoff journal record lacks writer failure status")
		}
		if r.CancellationRequested && *r.Outcome != HandoffCanceled {
			return errors.New("cancellation intent must close as canceled")
		}
	}
	return nil
}

const (
	insertHandoffJournalSQL = `
INSERT INTO handoff_journal_records
    (run_id, ownership_token, spec_digest, observed_base_sha,
     credential_pre_digest, writer_complete, cancellation_requested,
	     writer_failure_status, state_preparation, instruction_preparation,
     lease_auth_identity_id,
     lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
     export_dir, outcome, opened_at, body)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	getHandoffJournalSQL = `
SELECT ownership_token, spec_digest, observed_base_sha,
       credential_pre_digest, writer_complete, cancellation_requested,
	       writer_failure_status, state_preparation, instruction_preparation,
       lease_auth_identity_id,
       lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
       export_dir, outcome, opened_at, body
FROM handoff_journal_records WHERE run_id = ?`
	updateHandoffJournalSQL = `
UPDATE handoff_journal_records
SET ownership_token = ?, spec_digest = ?, observed_base_sha = ?,
    credential_pre_digest = ?, writer_complete = ?, cancellation_requested = ?,
	    writer_failure_status = ?, state_preparation = ?, instruction_preparation = ?,
    lease_auth_identity_id = ?, lease_holder = ?, lease_fence = ?,
    lease_acquired_at = ?, lease_expires_at = ?, export_dir = ?,
    outcome = ?, opened_at = ?, body = ?
WHERE run_id = ? AND outcome IS NULL`
)

// BeginHandoffJournal durably opens one record. A run id is single-use:
// reopening either an open or closed record fails.
func (tx *InternalTx) BeginHandoffJournal(ctx context.Context, rec HandoffJournalRecord) error {
	if rec.Lease != nil {
		return errors.New("begin handoff journal: leased records require BeginLeasedHandoffJournal")
	}
	if err := validateNewHandoffJournal(rec); err != nil {
		return err
	}
	return tx.insertHandoffJournal(ctx, rec)
}

func validateNewHandoffJournal(rec HandoffJournalRecord) error {
	if rec.ObservedBaseSHA != "" || rec.CredentialPreDigest != "" ||
		rec.WriterComplete || rec.CancellationRequested || rec.WriterFailureStatus != nil ||
		rec.State != nil || rec.Instructions != nil ||
		rec.ExportDir != "" || rec.Outcome != nil {
		return errors.New("begin handoff journal: new record carries unearned progress")
	}
	return nil
}

func (tx *InternalTx) insertHandoffJournal(ctx context.Context, rec HandoffJournalRecord) error {
	if rec.Outcome != nil {
		return errors.New("begin handoff journal: new record already has an outcome")
	}
	var exists bool
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM handoff_journal_records WHERE run_id = ?)`, rec.RunID).
		Scan(&exists); err != nil {
		return fmt.Errorf("begin handoff journal %q: %w", rec.RunID, err)
	}
	if exists {
		return fmt.Errorf("begin handoff journal %q: %w", rec.RunID, ErrImmutableConflict)
	}
	body, args, err := encodeHandoffJournal(rec)
	if err != nil {
		return fmt.Errorf("begin handoff journal %q: %w", rec.RunID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertHandoffJournalSQL,
		rec.RunID, rec.OwnershipToken, rec.SpecDigest, rec.ObservedBaseSHA,
		rec.CredentialPreDigest, rec.WriterComplete, rec.CancellationRequested,
		args.writerFailureStatus, args.state, args.instructions,
		args.leaseID, args.leaseHolder, args.leaseFence,
		args.leaseAcquiredAt, args.leaseExpiresAt,
		rec.ExportDir, args.outcome, formatTime(rec.OpenedAt), body); err != nil {
		return fmt.Errorf("begin handoff journal %q: %w", rec.RunID, err)
	}
	return nil
}

// BeginLeasedHandoffJournal acquires the identity's mutation lease and opens
// its journal record in this one SQLite transaction. A crash can therefore
// observe neither state or both, never a live lease with no recovery record.
func (tx *InternalTx) BeginLeasedHandoffJournal(
	ctx context.Context,
	rec HandoffJournalRecord,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if rec.Lease != nil {
		return domain.AuthStoreMutationLease{}, errors.New("begin leased handoff journal: record already carries a lease")
	}
	if err := validateNewHandoffJournal(rec); err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	lease, err := tx.AcquireAuthStoreMutationLease(ctx, id, holder, now, expiresAt)
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	// Same-holder acquisition converges on an existing live window. That is
	// useful to idempotent lease callers, but a new run must not attach the
	// old window to a fresh ownership token: recovery could then release the
	// prior run's lease after auditing only the new run's runtime objects.
	// A window opened by this call has exactly the requested bounds.
	if !lease.AcquiredAt.Equal(now) || !lease.ExpiresAt.Equal(expiresAt) {
		return domain.AuthStoreMutationLease{}, errors.New(
			"begin leased handoff journal: acquisition converged on an existing lease window",
		)
	}
	rec.Lease = &HandoffJournalLease{
		AuthIdentityID: lease.AuthIdentityID,
		Holder:         lease.Holder,
		Fence:          lease.Fence,
		AcquiredAt:     lease.AcquiredAt,
		ExpiresAt:      lease.ExpiresAt,
	}
	if err := tx.insertHandoffJournal(ctx, rec); err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// GetHandoffJournal reconstructs and cross-checks one journal record.
func (tx *ReadTx) GetHandoffJournal(ctx context.Context, runID string) (HandoffJournalRecord, error) {
	var (
		ownership, specDigest, observedBase, credentialPre string
		writerComplete, cancellationRequested              bool
		writerFailureStatus                                sql.NullInt64
		statePreparation                                   string
		instructionPreparation                             string
		leaseID, leaseHolder                               sql.NullString
		leaseFence                                         sql.NullInt64
		leaseAcquiredAt, leaseExpiresAt                    sql.NullString
		exportDir, openedAt                                string
		outcome                                            sql.NullString
		body                                               []byte
	)
	err := tx.tx.QueryRowContext(ctx, getHandoffJournalSQL, runID).Scan(
		&ownership, &specDigest, &observedBase, &credentialPre,
		&writerComplete, &cancellationRequested,
		&writerFailureStatus, &statePreparation, &instructionPreparation,
		&leaseID, &leaseHolder, &leaseFence,
		&leaseAcquiredAt, &leaseExpiresAt, &exportDir, &outcome,
		&openedAt, &body,
	)
	if err != nil {
		return HandoffJournalRecord{}, fmt.Errorf("get handoff journal %q: %w", runID, notFoundOr(err))
	}
	rec, err := decode[HandoffJournalRecord](body)
	if err != nil {
		return HandoffJournalRecord{}, fmt.Errorf("get handoff journal %q: %w", runID, err)
	}
	if rec.RunID != runID || rec.OwnershipToken != ownership ||
		rec.SpecDigest != specDigest || rec.ObservedBaseSHA != observedBase ||
		rec.CredentialPreDigest != credentialPre || rec.WriterComplete != writerComplete ||
		rec.CancellationRequested != cancellationRequested ||
		!journalWriterFailureColumnEqual(writerFailureStatus, rec.WriterFailureStatus) ||
		!journalStateColumnEqual(statePreparation, rec.State) ||
		!journalInstructionsColumnEqual(instructionPreparation, rec.Instructions) ||
		rec.ExportDir != exportDir || !timeColumnEqual(openedAt, rec.OpenedAt) ||
		!journalOutcomeColumnEqual(outcome, rec.Outcome) ||
		!journalLeaseColumnsEqual(rec.Lease, leaseID, leaseHolder, leaseFence, leaseAcquiredAt, leaseExpiresAt) {
		return HandoffJournalRecord{}, fmt.Errorf("get handoff journal %q: %w", runID, errRowInconsistent)
	}
	return rec, nil
}

// MarkHandoffSeedObserved commits the pre-writer base proof.
func (tx *InternalTx) MarkHandoffSeedObserved(ctx context.Context, runID, observedBaseSHA string) error {
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		return setJournalString(&rec.ObservedBaseSHA, observedBaseSHA)
	})
}

// MarkHandoffCredentialObserved commits the pre-writer credential digest.
func (tx *InternalTx) MarkHandoffCredentialObserved(ctx context.Context, runID, preDigest string) error {
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		if rec.Lease == nil {
			return errors.New("mark handoff credential observed: record has no lease")
		}
		return setJournalString(&rec.CredentialPreDigest, preDigest)
	})
}

// MarkHandoffStatePrepared commits the clean state-volume identities and
// manifests before the writer can be created.
func (tx *InternalTx) MarkHandoffStatePrepared(
	ctx context.Context,
	runID string,
	state HandoffJournalState,
) error {
	if err := state.validate(); err != nil {
		return fmt.Errorf("mark handoff state prepared: %w", err)
	}
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		switch {
		case rec.State == nil:
			copied := state
			rec.State = &copied
			return nil
		case *rec.State == state:
			return nil
		default:
			return ErrHandoffJournalProofConflict
		}
	})
}

// MarkHandoffInstructionsPrepared commits the explicit instruction sources
// and result digest before the writer can be created.
func (tx *InternalTx) MarkHandoffInstructionsPrepared(
	ctx context.Context,
	runID string,
	instructions HandoffJournalInstructions,
) error {
	if err := instructions.validate(); err != nil {
		return fmt.Errorf("mark handoff instructions prepared: %w", err)
	}
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		switch {
		case rec.Instructions == nil:
			copied := instructions
			rec.Instructions = &copied
			return nil
		case *rec.Instructions == instructions:
			return nil
		default:
			return ErrHandoffJournalProofConflict
		}
	})
}

// MarkHandoffWriterComplete commits the process-local writer-complete proof.
func (tx *InternalTx) MarkHandoffWriterComplete(ctx context.Context, runID string) error {
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		if rec.CancellationRequested || rec.WriterFailureStatus != nil {
			return ErrHandoffJournalProofConflict
		}
		rec.WriterComplete = true
		return nil
	})
}

// MarkHandoffCancellationRequested commits daemon cancellation intent before
// ward issues any stop. Exact retries converge and the durable bit outranks a
// launcher marker produced by the resulting signal.
func (tx *InternalTx) MarkHandoffCancellationRequested(
	ctx context.Context, runID string,
) error {
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		rec.CancellationRequested = true
		return nil
	})
}

// MarkHandoffWriterFailed commits the authenticated nonzero launcher status.
func (tx *InternalTx) MarkHandoffWriterFailed(
	ctx context.Context, runID string, status int,
) error {
	if status < 1 || status > 255 {
		return fmt.Errorf("mark handoff writer failed: status %d is outside 1..255", status)
	}
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		if rec.WriterComplete {
			return ErrHandoffJournalProofConflict
		}
		switch {
		case rec.WriterFailureStatus == nil:
			rec.WriterFailureStatus = &status
			return nil
		case *rec.WriterFailureStatus == status:
			return nil
		default:
			return ErrHandoffJournalProofConflict
		}
	})
}

// MarkHandoffExportMaterialized commits the verified export's host location.
func (tx *InternalTx) MarkHandoffExportMaterialized(ctx context.Context, runID, exportDir string) error {
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		if exportDir == "" {
			return errors.New("handoff journal export location is empty")
		}
		// Unlike the pre-writer proofs, this path is replaceable diagnostic
		// state. Recovery deliberately materializes a fresh export and must
		// replace the location left by a crash before terminal close.
		rec.ExportDir = exportDir
		return nil
	})
}

// CloseHandoffJournal commits one terminal outcome. A second close fails,
// including an exact replay, because terminality is the one-close contract.
func (tx *InternalTx) CloseHandoffJournal(
	ctx context.Context, runID string, outcome HandoffJournalOutcome,
) error {
	if !outcome.valid() {
		return fmt.Errorf("close handoff journal %q: invalid outcome %q", runID, outcome)
	}
	return tx.amendHandoffJournal(ctx, runID, func(rec *HandoffJournalRecord) error {
		rec.Outcome = &outcome
		return nil
	})
}

func setJournalString(dst *string, value string) error {
	switch {
	case value == "":
		return errors.New("handoff journal amendment is empty")
	case *dst == "":
		*dst = value
		return nil
	case *dst == value:
		return nil
	default:
		return ErrHandoffJournalProofConflict
	}
}

func (tx *InternalTx) amendHandoffJournal(
	ctx context.Context,
	runID string,
	amend func(*HandoffJournalRecord) error,
) error {
	rec, err := tx.GetHandoffJournal(ctx, runID)
	if err != nil {
		return err
	}
	if rec.Outcome != nil {
		return fmt.Errorf("amend handoff journal %q: %w", runID, ErrHandoffJournalClosed)
	}
	if err := amend(&rec); err != nil {
		return fmt.Errorf("amend handoff journal %q: %w", runID, err)
	}
	body, args, err := encodeHandoffJournal(rec)
	if err != nil {
		return fmt.Errorf("amend handoff journal %q: %w", runID, err)
	}
	result, err := tx.tx.ExecContext(ctx, updateHandoffJournalSQL,
		rec.OwnershipToken, rec.SpecDigest, rec.ObservedBaseSHA,
		rec.CredentialPreDigest, rec.WriterComplete, rec.CancellationRequested,
		args.writerFailureStatus, args.state, args.instructions,
		args.leaseID, args.leaseHolder, args.leaseFence,
		args.leaseAcquiredAt, args.leaseExpiresAt,
		rec.ExportDir, args.outcome, formatTime(rec.OpenedAt), body, runID)
	if err != nil {
		return fmt.Errorf("amend handoff journal %q: %w", runID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("amend handoff journal %q: %w", runID, err)
	}
	if affected != 1 {
		return fmt.Errorf("amend handoff journal %q: %w", runID, ErrHandoffJournalClosed)
	}
	return nil
}

type handoffJournalArgs struct {
	leaseID, leaseHolder            any
	leaseFence                      any
	leaseAcquiredAt, leaseExpiresAt any
	writerFailureStatus             any
	state                           string
	instructions                    string
	outcome                         any
}

func encodeHandoffJournal(rec HandoffJournalRecord) (string, handoffJournalArgs, error) {
	body, err := encode(rec)
	if err != nil {
		return "", handoffJournalArgs{}, err
	}
	var args handoffJournalArgs
	if rec.Lease != nil {
		args.leaseID = rec.Lease.AuthIdentityID
		args.leaseHolder = rec.Lease.Holder
		args.leaseFence = rec.Lease.Fence
		args.leaseAcquiredAt = formatTime(rec.Lease.AcquiredAt)
		args.leaseExpiresAt = formatTime(rec.Lease.ExpiresAt)
	}
	if rec.Outcome != nil {
		args.outcome = *rec.Outcome
	}
	if rec.WriterFailureStatus != nil {
		args.writerFailureStatus = *rec.WriterFailureStatus
	}
	if rec.State != nil {
		stateBody, stateErr := encode(*rec.State)
		if stateErr != nil {
			return "", handoffJournalArgs{}, stateErr
		}
		args.state = stateBody
	}
	if rec.Instructions != nil {
		instructionBody, instructionErr := encode(*rec.Instructions)
		if instructionErr != nil {
			return "", handoffJournalArgs{}, instructionErr
		}
		args.instructions = instructionBody
	}
	return body, args, nil
}

func journalWriterFailureColumnEqual(column sql.NullInt64, status *int) bool {
	if status == nil {
		return !column.Valid
	}
	return column.Valid && column.Int64 == int64(*status)
}

func journalStateColumnEqual(column string, state *HandoffJournalState) bool {
	if state == nil {
		return column == ""
	}
	body, err := encode(*state)
	return err == nil && column == body
}

func journalInstructionsColumnEqual(
	column string,
	instructions *HandoffJournalInstructions,
) bool {
	if instructions == nil {
		return column == ""
	}
	body, err := encode(*instructions)
	return err == nil && column == body
}

func journalOutcomeColumnEqual(column sql.NullString, outcome *HandoffJournalOutcome) bool {
	if outcome == nil {
		return !column.Valid
	}
	return column.Valid && column.String == string(*outcome)
}

func journalLeaseColumnsEqual(
	lease *HandoffJournalLease,
	id, holder sql.NullString,
	fence sql.NullInt64,
	acquiredAt, expiresAt sql.NullString,
) bool {
	if lease == nil {
		return !id.Valid && !holder.Valid && !fence.Valid && !acquiredAt.Valid && !expiresAt.Valid
	}
	return id.Valid && id.String == string(lease.AuthIdentityID) &&
		holder.Valid && holder.String == string(lease.Holder) &&
		fence.Valid && fence.Int64 == lease.Fence &&
		acquiredAt.Valid && timeColumnEqual(acquiredAt.String, lease.AcquiredAt) &&
		expiresAt.Valid && timeColumnEqual(expiresAt.String, lease.ExpiresAt)
}
