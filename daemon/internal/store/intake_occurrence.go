package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Label-intake occurrence persistence (issue #720). The occurrence is a
// daemon-internal current-state record, not a sync-carried aggregate: it
// upserts by its (repository, issue, label, ordinal) key under the store's
// immediate write lock, so the ordinal latch and its mutations serialize as
// one decision across callers. The JSON body is the authoritative
// domain.IntakeOccurrence; the extracted columns exist for the latch query and
// the reconstruction re-gate.
//
// The admission binding is the trust boundary, and it is minted, not accepted.
// Admission derives the whole subject binding from the occurrence and the
// admitted proposal: it mints the work-unit declaration for the occurrence's own
// issue (plan §5.12 item 3), so (repository, issue, project, run) are
// authoritative by construction and there is no caller-supplied subject field to
// cross-check dimension by dimension. deriveIntakeAdmission is the single
// derivation both the write mint and the read re-gate share, so a stored binding
// is authentic iff it byte-equals what re-derivation produces; anything else
// fails closed. The repository/issue authority rides on the proposal admission
// key (UpstreamEventID carries repository_id, issue, label, ordinal): a proposal
// admitted under a different occurrence's key never matches, so a foreign-repo or
// foreign-issue proposal is refused without needing a project→repository lookup
// the declaration cannot answer.

// ErrIntakeAdmissionInconsistent marks a stored occurrence whose admission
// binding no longer authentically names its proposal or subject: the read
// re-gate fails closed with it so a consumer can tell a tampered or
// out-of-date binding from a transient error and hold, never act on it.
var ErrIntakeAdmissionInconsistent = errors.New("intake occurrence admission binding is inconsistent with its parents")

// ErrIntakeProjectRepositoryMismatch marks a run whose project does not belong
// to the occurrence's own repository — the tie #720 could only document as a
// caller assumption, now store-enforced through the durable Project authority
// (issue #740). MintIntakeDeclaration returns it directly at mint time; the read
// re-gate additionally wraps ErrIntakeAdmissionInconsistent so a consumer still
// holds a tampered or cross-repo binding rather than acting on it.
var ErrIntakeProjectRepositoryMismatch = errors.New(
	"intake run project belongs to a different repository than the occurrence")

const intakeOccurrenceColumns = `repository_id, issue_number, label, ordinal, repo, state,
	admission_key, proposal_instance_id, work_unit_id, policy_artifact_id,
	refusal_reason, supersession_reason, body`

const selectIntakeOccurrenceSQL = `SELECT ` + intakeOccurrenceColumns + `
	FROM intake_occurrences
	WHERE repository_id = ? AND issue_number = ? AND label = ? AND ordinal = ?`

const selectLatestIntakeOccurrenceSQL = `SELECT ` + intakeOccurrenceColumns + `
	FROM intake_occurrences
	WHERE repository_id = ? AND issue_number = ? AND label = ?
	ORDER BY ordinal DESC LIMIT 1`

const upsertIntakeOccurrenceSQL = `
INSERT INTO intake_occurrences
	(repository_id, issue_number, label, ordinal, repo, state,
	 admission_key, proposal_instance_id, work_unit_id, policy_artifact_id,
	 refusal_reason, supersession_reason, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repository_id, issue_number, label, ordinal) DO UPDATE SET
	repo                 = excluded.repo,
	state                = excluded.state,
	admission_key        = excluded.admission_key,
	proposal_instance_id = excluded.proposal_instance_id,
	work_unit_id         = excluded.work_unit_id,
	policy_artifact_id   = excluded.policy_artifact_id,
	refusal_reason       = excluded.refusal_reason,
	supersession_reason  = excluded.supersession_reason,
	body                 = excluded.body`

// putIntakeOccurrence writes the full validated occurrence and its extracted
// columns. encode re-validates the body, so an internally inconsistent record
// never reaches the row.
func (tx *WriteTx) putIntakeOccurrence(ctx context.Context, o domain.IntakeOccurrence) error {
	body, err := encode(o)
	if err != nil {
		return fmt.Errorf("put intake occurrence: %w", err)
	}
	var admissionKey, proposalInstanceID, workUnitID, policyArtifactID, refusalReason, supersessionReason any
	if o.Admission != nil {
		key, err := o.Admission.AdmissionKey.String()
		if err != nil {
			return fmt.Errorf("put intake occurrence admission key: %w", err)
		}
		admissionKey = key
		proposalInstanceID = string(o.Admission.ProposalInstanceID)
		workUnitID = string(o.Admission.Subject.WorkUnitID)
		policyArtifactID = string(o.Admission.Subject.PolicyArtifactID)
	}
	if o.Refusal != nil {
		refusalReason = string(o.Refusal.Reason)
	}
	if o.Supersession != nil {
		supersessionReason = string(o.Supersession.Reason)
	}
	if _, err := tx.tx.ExecContext(ctx, upsertIntakeOccurrenceSQL,
		o.RepositoryID, o.IssueNumber, o.Label, o.Ordinal, o.Repo, o.State,
		admissionKey, proposalInstanceID, workUnitID, policyArtifactID, refusalReason, supersessionReason, body,
	); err != nil {
		return fmt.Errorf("put intake occurrence: %w", err)
	}
	return nil
}

// scanIntakeOccurrence reconstructs one occurrence, cross-checks the extracted
// columns against the decoded body, and re-derives the admission binding from
// its parents, rejecting a stored binding that does not match (verifyIntake
// Admission).
func (tx *ReadTx) scanIntakeOccurrence(ctx context.Context, sc scanner) (domain.IntakeOccurrence, error) {
	var (
		repositoryID       int64
		issueNumber        int
		label              string
		ordinal            int
		repo               string
		state              string
		admissionKey       sql.NullString
		proposalInstanceID sql.NullString
		workUnitID         sql.NullString
		policyArtifactID   sql.NullString
		refusalReason      sql.NullString
		supersessionReason sql.NullString
		body               []byte
	)
	if err := sc.Scan(&repositoryID, &issueNumber, &label, &ordinal, &repo, &state,
		&admissionKey, &proposalInstanceID, &workUnitID, &policyArtifactID,
		&refusalReason, &supersessionReason, &body); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	o, err := decode[domain.IntakeOccurrence](body)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if o.RepositoryID != repositoryID || o.IssueNumber != issueNumber || o.Label != label ||
		o.Ordinal != ordinal || o.Repo != repo || string(o.State) != state {
		return domain.IntakeOccurrence{}, errRowInconsistent
	}
	if err := tx.regateIntakeAdmissionColumns(o, admissionKey, proposalInstanceID, workUnitID, policyArtifactID); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	// The refusal and supersession reasons are daemon-authored facts with no
	// cross-parent authority to re-derive, so each is authenticated against its
	// own extracted column, catching a body-only tamper that would fabricate or
	// swap the cause.
	var refusalCause, supersessionCause string
	if o.Refusal != nil {
		refusalCause = string(o.Refusal.Reason)
	}
	if o.Supersession != nil {
		supersessionCause = string(o.Supersession.Reason)
	}
	if err := regateIntakeReasonColumn(o.Refusal != nil, refusalCause, refusalReason); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if err := regateIntakeReasonColumn(o.Supersession != nil, supersessionCause, supersessionReason); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if o.Admission != nil {
		if err := tx.verifyIntakeAdmission(ctx, o); err != nil {
			return domain.IntakeOccurrence{}, err
		}
	}
	return o, nil
}

// regateIntakeReasonColumn authenticates a decoded reason fact (the refusal or
// the supersession cause) against its extracted column: a present fact has a
// matching column and an absent one has none. These reasons are daemon-authored
// values with no cross-parent authority to re-derive, so the independent column
// is what catches a body-only tamper that would fabricate or swap a cause —
// reconciliation or audit would otherwise consume the forged reason.
func regateIntakeReasonColumn(present bool, cause string, column sql.NullString) error {
	if !present {
		if column.Valid {
			return errRowInconsistent
		}
		return nil
	}
	if !column.Valid || column.String != cause {
		return errRowInconsistent
	}
	return nil
}

// verifyIntakeAdmission re-gates a decoded occurrence's admission binding as a
// returned-object trust boundary: it re-derives the authoritative binding from
// the occurrence and its admitted proposal (deriveIntakeAdmission) and requires
// the stored binding to byte-equal it. Because the whole binding is re-derived
// and compared as one value, no subject field can be tampered independently —
// the enumerable dimension-by-dimension re-gate this replaced is why five review
// rounds each surfaced one more missed dimension. Fails closed with
// ErrIntakeAdmissionInconsistent, so a tampered or out-of-date row is held, never
// acted on. The policy artifact id is authenticated by its extracted column
// (regateIntakeAdmissionColumns), not re-resolved here, so an occurrence whose
// artifact later becomes unavailable stays readable while a body-only tamper of
// the id is still caught.
func (tx *ReadTx) verifyIntakeAdmission(ctx context.Context, o domain.IntakeOccurrence) error {
	expected, err := tx.deriveIntakeAdmission(ctx, o, o.Admission.ProposalInstanceID, o.Admission.Subject.PolicyArtifactID)
	if err != nil {
		return err
	}
	if !intakeAdmissionEqual(*o.Admission, expected) {
		return fmt.Errorf("intake admission binding is not authentic for its occurrence: %w", ErrIntakeAdmissionInconsistent)
	}
	// A recorded supersession is a decoded trust bit: the write path stamps it
	// only when a genuinely open card was withdrawn, but a tampered absent/closed
	// row could assert a withdrawal its proposal never took. Re-gate against two
	// authorities. First the item status must be superseded (an open, started, or
	// declined card was not intake-withdrawn). Status alone is insufficient
	// because start_with_changes also supersedes the original item while recording
	// a real start, so second: the decision ledger must hold no decision for the
	// instance — an intake withdrawal records none, every operator decision (start,
	// start_with_changes, decline) records one, and the two are mutually exclusive
	// (a decided card is never open for intake to withdraw). Fail closed on either.
	if o.Supersession != nil {
		item, err := tx.authenticatedProposalItem(ctx, o.Admission.ProposalInstanceID, o.Admission.ProposalDigest)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusSuperseded {
			return fmt.Errorf(
				"intake supersession fact contradicts the proposal item status %s: %w", item.Status, ErrIntakeAdmissionInconsistent)
		}
		decided, err := tx.proposalInstanceDecided(ctx, o.Admission.ProposalInstanceID)
		if err != nil {
			return err
		}
		if decided {
			return fmt.Errorf(
				"intake supersession fact contradicts a recorded proposal decision: %w", ErrIntakeAdmissionInconsistent)
		}
		// The reason is authenticated only in the direction that is decidable on
		// read. issue_closed is stamped only when the occurrence becomes closed,
		// and closed is terminal, so an issue_closed supersession on a
		// non-closed occurrence is impossible and rejected. The reverse is not a
		// read invariant: a label_removed supersession legitimately survives a
		// later absent->closed observation, so a closed occurrence may still carry
		// label_removed.
		if o.Supersession.Reason == domain.IntakeSupersededIssueClosed && o.State != domain.IntakeOccurrenceClosed {
			return fmt.Errorf(
				"intake issue_closed supersession on a %s occurrence: %w", o.State, ErrIntakeAdmissionInconsistent)
		}
	}
	return nil
}

// proposalInstanceDecided reports whether the decision ledger holds a terminal
// decision (start, start_with_changes, or decline) for the instance. An intake
// withdrawal records none, so a recorded decision means a superseded item was
// decided, not intake-withdrawn — the authority that distinguishes a genuine
// label/issue supersession from a start_with_changes that also superseded the
// original card.
func (tx *ReadTx) proposalInstanceDecided(ctx context.Context, instanceID domain.ProposalInstanceID) (bool, error) {
	var count int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_proposal_decisions WHERE instance_id = ?`, instanceID).Scan(&count); err != nil {
		return false, fmt.Errorf("intake supersession decision ledger: %w", err)
	}
	return count > 0, nil
}

// regateIntakeAdmissionColumns cross-checks the extracted admission columns
// against the decoded body: the four are set together, and each equals the body
// it mirrors. The policy_artifact_id column is the load-bearing check for that
// one field: unlike the work unit or admission key, the policy artifact id has
// no durable parent the read can re-derive it from (it may legitimately be gone),
// so this independent column is what catches a body-only tamper that would
// otherwise substitute a foreign or missing artifact id and be misread as a
// start-time unavailability rather than a corrupt row.
func (tx *ReadTx) regateIntakeAdmissionColumns(
	o domain.IntakeOccurrence, admissionKey, proposalInstanceID, workUnitID, policyArtifactID sql.NullString,
) error {
	if o.Admission == nil {
		if admissionKey.Valid || proposalInstanceID.Valid || workUnitID.Valid || policyArtifactID.Valid {
			return errRowInconsistent
		}
		return nil
	}
	if !admissionKey.Valid || !proposalInstanceID.Valid || !workUnitID.Valid || !policyArtifactID.Valid {
		return errRowInconsistent
	}
	key, err := o.Admission.AdmissionKey.String()
	if err != nil {
		return err
	}
	if admissionKey.String != key ||
		proposalInstanceID.String != string(o.Admission.ProposalInstanceID) ||
		workUnitID.String != string(o.Admission.Subject.WorkUnitID) ||
		policyArtifactID.String != string(o.Admission.Subject.PolicyArtifactID) {
		return errRowInconsistent
	}
	return nil
}

// authenticatedIntakeProposal authenticates the proposal admitted for an
// occurrence and returns it. The proposal must be admitted under the occurrence's
// own derived key (which carries repository, issue, label, and ordinal, so a
// foreign-repo or foreign-issue proposal never matches), must be a run proposal,
// and its two run references — the resolved-policy run and the subject handle —
// must agree, so the subject a start assembles is the proposal's own, not a
// caller's claim. Fails closed with ErrIntakeAdmissionInconsistent.
func (tx *ReadTx) authenticatedIntakeProposal(
	ctx context.Context, o domain.IntakeOccurrence, instanceID domain.ProposalInstanceID,
) (domain.ProposalInstance, error) {
	instance, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return domain.ProposalInstance{}, fmt.Errorf("intake admission proposal: %w", err)
	}
	instanceKey, err := instance.Admission.String()
	if err != nil {
		return domain.ProposalInstance{}, err
	}
	occurrenceKey, err := o.ProposalAdmissionKey().String()
	if err != nil {
		return domain.ProposalInstance{}, err
	}
	if instanceKey != occurrenceKey {
		return domain.ProposalInstance{}, fmt.Errorf(
			"intake admission binds a foreign proposal: %w", ErrIntakeAdmissionInconsistent)
	}
	if instance.Proposal.RunProposal == nil {
		return domain.ProposalInstance{}, fmt.Errorf(
			"intake admission proposal is not a run proposal: %w", ErrIntakeAdmissionInconsistent)
	}
	if domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(instance.Proposal.ResolvedPolicyRunID)) != instance.Proposal.RunProposal.SubjectHandle {
		return domain.ProposalInstance{}, fmt.Errorf(
			"intake admission proposal subject handle and resolved-policy run disagree: %w", ErrIntakeAdmissionInconsistent)
	}
	return instance, nil
}

// deriveIntakeAdmission is the single authority that builds an occurrence's
// admission binding from the occurrence and its admitted proposal — never from a
// caller- or row-supplied subject. It is what the write mint records and what the
// read re-gate compares against, so the binding has exactly one definition. It
// authenticates against **durable** parents only (the proposal, its minted
// work-unit declaration, and the run's resolved policy — all write-once /
// immutable): the subject's identity fields come from the declaration resolved
// through the proposal handle, and the check that the declaration's BoundIssue
// equals the occurrence's issue is what makes the declaration authoritative for
// this occurrence. The Source is minted from the occurrence's own (repo, issue).
// The policy artifact is named, not re-resolved here: its digest is pinned to the
// run's resolved policy (durable), so a tampered PolicyArtifactDigest is caught,
// but the artifact's *current availability* is a start-time concern #659 refuses
// with subject_input_missing/stale — the read boundary must not make an
// occurrence with an unavailable-but-authentic input unreadable (owner-ratified
// after round 11 #1). The admission-time presence of the artifact is checked
// separately on the write path (authenticateIntakePolicyArtifact). The
// declaration's project, by contrast, IS re-resolved here and required to belong
// to the occurrence's repository (issue #740): the projects row is write-once and
// undeletable, so an authentic binding always resolves it — the
// availability-vs-integrity distinction cuts the other way than for the policy
// artifact. This closes the tampered cross-repo case #720's re-gate could not
// check: the stored binding faithfully mirrors a declaration whose project maps
// to a foreign repository, so the byte-equality re-derivation still matches and
// only this check catches it. Fails closed with ErrIntakeAdmissionInconsistent.
func (tx *ReadTx) deriveIntakeAdmission(
	ctx context.Context, o domain.IntakeOccurrence,
	instanceID domain.ProposalInstanceID, policyArtifactID domain.ArtifactID,
) (domain.IntakeAdmission, error) {
	instance, err := tx.authenticatedIntakeProposal(ctx, o, instanceID)
	if err != nil {
		return domain.IntakeAdmission{}, err
	}
	handle := instance.Proposal.RunProposal.SubjectHandle
	declaration, policy, err := tx.ResolveProposalSubject(ctx, handle)
	if err != nil {
		return domain.IntakeAdmission{}, fmt.Errorf("intake admission subject: %w", err)
	}
	if declaration.BoundIssue == nil || *declaration.BoundIssue != o.IssueNumber {
		return domain.IntakeAdmission{}, fmt.Errorf(
			"intake admission work unit is not bound to the occurrence's issue %d: %w", o.IssueNumber, ErrIntakeAdmissionInconsistent)
	}
	// The declaration's project must belong to the occurrence's own repository
	// (#740). The projects row is write-once and undeletable, so for an authentic
	// binding it always reconstructs. A *durable* reconstruction failure —
	// unregistered (ErrNotFound), or a corrupt/tampered row
	// (errRowInconsistent, which scanProject also carries for a body that fails
	// to decode or re-validate) — is authority corruption, so classify it as
	// ErrIntakeAdmissionInconsistent (the hold signal a consumer must not retry
	// past) while preserving the cause. A *transient* fault (context
	// cancellation, a DB operational error) is propagated as-is, without the hold
	// sentinel, so a consumer retries a healthy admission instead of parking it.
	project, err := tx.GetProject(ctx, declaration.ProjectID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, errRowInconsistent) {
			return domain.IntakeAdmission{}, fmt.Errorf(
				"intake admission project %q authority is unresolvable: %w: %w",
				declaration.ProjectID, ErrIntakeAdmissionInconsistent, err)
		}
		return domain.IntakeAdmission{}, fmt.Errorf("intake admission project: %w", err)
	}
	if project.RepositoryID != o.RepositoryID {
		return domain.IntakeAdmission{}, fmt.Errorf(
			"intake admission run project %q belongs to repository %d, not the occurrence's %d: %w: %w",
			declaration.ProjectID, project.RepositoryID, o.RepositoryID,
			ErrIntakeAdmissionInconsistent, ErrIntakeProjectRepositoryMismatch)
	}
	return domain.IntakeAdmission{
		AdmissionKey:       o.ProposalAdmissionKey(),
		ProposalInstanceID: instanceID,
		ProposalDigest:     instance.Proposal.Digest,
		Subject: domain.IntakeSubjectBinding{
			ProjectID:            declaration.ProjectID,
			WorkUnitID:           declaration.ID,
			ImplementationRunID:  declaration.RunID,
			PolicyArtifactID:     policyArtifactID,
			PolicyArtifactDigest: policy.Digest,
			ResolvedPolicyDigest: policy.Digest,
			Source:               intakeIssueSubjectSource(o),
		},
	}, nil
}

// intakeIssueSubjectSource mints the issue_subject elaboration source from the
// occurrence's own coordinates. The subject a label start assembles is always
// this occurrence's issue, so it is derived here, never accepted from a caller.
func intakeIssueSubjectSource(o domain.IntakeOccurrence) domain.ElaborationSource {
	return domain.ElaborationSource{
		Kind: domain.ElaborationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{
			Repo: o.Repo, RepositoryID: o.RepositoryID, IssueNumber: o.IssueNumber,
		},
	}
}

// GetIntakeOccurrence reconstructs the occurrence for an exact coordinate, or
// ErrNotFound.
func (tx *ReadTx) GetIntakeOccurrence(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
) (domain.IntakeOccurrence, error) {
	o, err := tx.scanIntakeOccurrence(ctx, tx.tx.QueryRowContext(ctx,
		selectIntakeOccurrenceSQL, repositoryID, issueNumber, label, ordinal))
	if err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"get intake occurrence %d/%d/%s/%d: %w", repositoryID, issueNumber, label, ordinal, notFoundOr(err))
	}
	return o, nil
}

// LatestIntakeOccurrence returns the highest-ordinal occurrence for a
// (repository, issue, label), and false when none exists.
func (tx *ReadTx) LatestIntakeOccurrence(
	ctx context.Context, repositoryID int64, issueNumber int, label string,
) (domain.IntakeOccurrence, bool, error) {
	o, err := tx.scanIntakeOccurrence(ctx, tx.tx.QueryRowContext(ctx,
		selectLatestIntakeOccurrenceSQL, repositoryID, issueNumber, label))
	if errors.Is(notFoundOr(err), ErrNotFound) {
		return domain.IntakeOccurrence{}, false, nil
	}
	if err != nil {
		return domain.IntakeOccurrence{}, false, fmt.Errorf(
			"latest intake occurrence %d/%d/%s: %w", repositoryID, issueNumber, label, err)
	}
	return o, true, nil
}

// AllocateNextIntakeOccurrence returns the active occurrence for a present
// label, allocating the next ordinal only when the latch permits it: a present
// latest occurrence is already active (returned, allocated=false), and only an
// absent or closed latest — the label observed gone, then back — allocates
// ordinal n+1. The write lock serializes the read and the insert, so a replay
// or a concurrent poll converges on one row rather than a duplicate ordinal.
func (tx *WriteTx) AllocateNextIntakeOccurrence(
	ctx context.Context, repo string, repositoryID int64, issueNumber int, label string, now time.Time,
) (domain.IntakeOccurrence, bool, error) {
	latest, found, err := tx.LatestIntakeOccurrence(ctx, repositoryID, issueNumber, label)
	if err != nil {
		return domain.IntakeOccurrence{}, false, err
	}
	if found && latest.State == domain.IntakeOccurrencePresent {
		return latest, false, nil
	}
	ordinal := 1
	if found {
		// Re-gate the latch against the admitted item, not only the decoded
		// state. A non-present occurrence releases the next ordinal only if its
		// admitted proposal actually ended (withdrawn or decided). The write-path
		// observation guard prevents a present->absent move with an open item, but
		// a directly-tampered row could still decode as absent/closed while its
		// item stays open; allocating then would admit a second live proposal for
		// the same labeled issue. Fail closed on that inconsistency.
		if latest.Admission != nil {
			open, err := tx.proposalItemOpen(ctx, latest.Admission.ProposalInstanceID, latest.Admission.ProposalDigest)
			if err != nil {
				return domain.IntakeOccurrence{}, false, err
			}
			if open {
				return domain.IntakeOccurrence{}, false, fmt.Errorf(
					"intake latch: %s occurrence %d still has an open admitted proposal: %w",
					latest.State, latest.Ordinal, ErrIntakeAdmissionInconsistent)
			}
		}
		ordinal = latest.Ordinal + 1
	}
	occurrence, err := domain.NewIntakeOccurrence(repo, repositoryID, issueNumber, label, ordinal, now)
	if err != nil {
		return domain.IntakeOccurrence{}, false, fmt.Errorf("allocate intake occurrence: %w", err)
	}
	if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
		return domain.IntakeOccurrence{}, false, err
	}
	return occurrence, true, nil
}

// RecordIntakeObservation advances the occurrence's observed state. A same-state
// observation is idempotent (no write), and any other move must be a legal
// successor, so polling converges and a reappearing label never revives a spent
// occurrence in place.
func (tx *WriteTx) RecordIntakeObservation(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
	newState domain.IntakeOccurrenceState, now time.Time,
) (domain.IntakeOccurrence, error) {
	occurrence, err := tx.GetIntakeOccurrence(ctx, repositoryID, issueNumber, label, ordinal)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if occurrence.State == newState {
		return occurrence, nil
	}
	if !occurrence.State.CanTransitionTo(newState) {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake observation %s -> %s: %w", occurrence.State, newState, ErrImmutableConflict)
	}
	// A departure from present with a still-open admitted proposal must withdraw
	// that proposal through SupersedeIntakeProposal. A bare observation would
	// leave the open card actionable while a later reappearance allocates the
	// next ordinal, producing two live proposals for one issue. A decided card
	// (item no longer open) is safe to leave: it is terminal, so a next ordinal
	// is the correct next occurrence.
	if newState != domain.IntakeOccurrencePresent && occurrence.Admission != nil {
		open, err := tx.proposalItemOpen(ctx, occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
		if err != nil {
			return domain.IntakeOccurrence{}, err
		}
		if open {
			return domain.IntakeOccurrence{}, fmt.Errorf(
				"intake observation leaving present with an open admitted proposal must supersede it: %w",
				ErrImmutableConflict)
		}
	}
	// A real transition restamps RecordedAt with the observation instant: the
	// occurrence is a current-state record, so its timestamp must report when it
	// reached this state, not when it was allocated (the idempotent same-state
	// case returned above without a write).
	occurrence.State = newState
	occurrence.RecordedAt = now.UTC()
	if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	return occurrence, nil
}

// MintIntakeDeclaration mints and persists the implementation-run identity's
// work-unit declaration for a present occurrence (plan §5.12 item 3): bound to
// the occurrence's own issue, on the caller-minted run, at the project the run
// names and scoped to the run's resolved policy. The admission transaction calls
// this before AllocateProposalInstance, so the proposal's SubjectHandle resolves
// through the existing ResolveProposalSubject path and admission never has to
// trust a caller-supplied subject: binding the declaration to the occurrence's
// issue here is what makes (issue, project, run) authoritative by construction.
// Write-once, so a replay of the same admission converges and a pre-existing
// declaration for the run bound to a different issue is an immutable conflict —
// an occurrence can never be admitted onto another issue's work unit.
//
// The run's project must belong to the occurrence's own repository: this
// resolves run.ProjectID through the durable Project authority (issue #740) and
// refuses a run whose project maps to another repository, so an occurrence can
// never be admitted onto a run for a different repository's project (the case a
// shared issue number would otherwise let through). An unregistered project
// fails closed — the GetProject ErrNotFound propagates, nothing defaults open.
// This replaces the caller trust assumption #720 documented here, which held
// only because the store then had no project→repository map (a Run carries only
// a ProjectID, a ProjectImage a RepositoryID but no ProjectID, and no Project
// entity existed); #659 registers the project its configuration names before
// minting the run.
// The declaration instant is the occurrence's own RecordedAt, not a caller
// clock: a crash-recovery replay passes a later wall-clock, and a caller-clock
// DeclaredAt would make the reconstructed declaration differ byte-for-byte, so
// the write-once store would reject the replay and break the admission
// sequence's convergence.
func (tx *WriteTx) MintIntakeDeclaration(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
	runID domain.RunID,
) (domain.WorkUnitDeclaration, error) {
	occurrence, err := tx.GetIntakeOccurrence(ctx, repositoryID, issueNumber, label, ordinal)
	if err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	if occurrence.State != domain.IntakeOccurrencePresent {
		return domain.WorkUnitDeclaration{}, fmt.Errorf(
			"intake declaration for a %s occurrence: %w", occurrence.State, ErrImmutableConflict)
	}
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("intake declaration run: %w", err)
	}
	// The run's project must belong to the occurrence's own repository (#740).
	// An unregistered project fails closed: the GetProject ErrNotFound propagates.
	project, err := tx.GetProject(ctx, run.ProjectID)
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("intake declaration project: %w", err)
	}
	if project.RepositoryID != occurrence.RepositoryID {
		return domain.WorkUnitDeclaration{}, fmt.Errorf(
			"intake declaration run project %q belongs to repository %d, not the occurrence's %d: %w",
			run.ProjectID, project.RepositoryID, occurrence.RepositoryID, ErrIntakeProjectRepositoryMismatch)
	}
	policy, err := tx.GetResolvedPolicy(ctx, runID)
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("intake declaration resolved policy: %w", err)
	}
	issue := occurrence.IssueNumber
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
		BoundIssue:          &issue,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, runID, run.ProjectID, occurrence.RecordedAt)
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("intake declaration: %w", err)
	}
	if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("intake declaration: %w", err)
	}
	return declaration, nil
}

// BindIntakeAdmission records the occurrence's admission binding. It accepts no
// subject: the whole binding is derived (deriveIntakeAdmission) from the
// occurrence and its admitted proposal, whose minted work-unit declaration
// (recorded by MintIntakeDeclaration, resolved through the proposal handle) is
// authoritative for (project, run, issue). The caller names only the admitted
// proposal instance and the one elaboration input with no accessor to derive it,
// the policy artifact. A replay converges (the re-derived binding equals the
// stored one); a different proposal on an already-bound occurrence is an
// immutable conflict.
func (tx *WriteTx) BindIntakeAdmission(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
	proposalInstanceID domain.ProposalInstanceID, policyArtifactID domain.ArtifactID,
) (domain.IntakeOccurrence, error) {
	occurrence, err := tx.GetIntakeOccurrence(ctx, repositoryID, issueNumber, label, ordinal)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	admission, err := tx.deriveIntakeAdmission(ctx, occurrence, proposalInstanceID, policyArtifactID)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if occurrence.Admission != nil {
		// Converge iff the re-derived binding equals the stored one; any other
		// proposal or policy artifact is a conflicting re-admission.
		if intakeAdmissionEqual(*occurrence.Admission, admission) {
			return occurrence, nil
		}
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake occurrence already admitted a different proposal: %w", ErrImmutableConflict)
	}
	if occurrence.State != domain.IntakeOccurrencePresent {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake admission on a %s occurrence: %w", occurrence.State, ErrImmutableConflict)
	}
	// The named policy artifact must be present and match the run's resolved
	// policy at admission time — a binding cannot be recorded against an input
	// that is not there when it is minted. Later (start-time) unavailability is
	// #659's subject_input_missing/stale refusal, not a bind-time error, and the
	// read re-gate deliberately does not re-require it (see deriveIntakeAdmission).
	if err := tx.authenticateIntakePolicyArtifact(ctx, policyArtifactID, admission.Subject.ResolvedPolicyDigest); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	occurrence.Admission = &admission
	if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	return occurrence, nil
}

// authenticateIntakePolicyArtifact confirms the named policy artifact exists, is
// a policy artifact, and carries the run's resolved-policy digest — the
// admission-time presence check for the one elaboration input the binding names.
// It is the write path's, not the read re-gate's: a stored binding stays readable
// when this input later disappears (that is a start-time refusal), but a fresh
// admission cannot name an absent or mismatched artifact.
func (tx *ReadTx) authenticateIntakePolicyArtifact(
	ctx context.Context, policyArtifactID domain.ArtifactID, resolvedPolicyDigest domain.Digest,
) error {
	policyArtifact, err := tx.GetArtifact(ctx, policyArtifactID)
	if err != nil {
		return fmt.Errorf("intake admission policy artifact: %w", err)
	}
	if policyArtifact.Type != domain.ArtifactKindPolicy || policyArtifact.Digest != resolvedPolicyDigest {
		return fmt.Errorf("intake admission binds a foreign policy artifact: %w", ErrIntakeAdmissionInconsistent)
	}
	return nil
}

// RecordIntakeRefusal records a durable start refusal on the occurrence. A
// replay with the same reason converges; the item is left an ordinary proposal
// by the caller.
func (tx *WriteTx) RecordIntakeRefusal(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
	reason domain.IntakeStartRefusalReason, now time.Time,
) (domain.IntakeOccurrence, error) {
	occurrence, err := tx.GetIntakeOccurrence(ctx, repositoryID, issueNumber, label, ordinal)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	// A refusal leaves an admitted proposal as an ordinary card, so it
	// presupposes an admission. Refusing an unadmitted occurrence would stamp a
	// durable refusal naming no proposal (which IntakeOccurrence.Validate now
	// also rejects); guard it here for a clear error at the write.
	if occurrence.Admission == nil {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake refusal on an unadmitted occurrence: %w", ErrImmutableConflict)
	}
	// A replay of the same refusal converges regardless of any later item drift,
	// so it is checked before the freshness guards below.
	if occurrence.Refusal != nil {
		if occurrence.Refusal.Reason == reason {
			return occurrence, nil
		}
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake occurrence already refused for %s: %w", occurrence.Refusal.Reason, ErrImmutableConflict)
	}
	// A fresh refusal means the start was declined and the card was left open for
	// the operator, so it holds only while the occurrence is present and its
	// proposal item is still open and undecided. A delayed gate call after a
	// decision, or after a supersession withdrew the card, must not stamp a false
	// "left as an ordinary proposal" record.
	if occurrence.State != domain.IntakeOccurrencePresent {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake refusal on a %s occurrence: %w", occurrence.State, ErrImmutableConflict)
	}
	open, err := tx.proposalItemOpen(ctx, occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if !open {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake refusal on a non-open proposal: %w", ErrImmutableConflict)
	}
	occurrence.Refusal = &domain.IntakeStartRefusal{Reason: reason, RecordedAt: now.UTC()}
	if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	return occurrence, nil
}

// SupersedeIntakeProposal moves the occurrence to a no-longer-present state and
// supersedes its still-open proposal, recording the reason on the occurrence
// row (never on the sync-carried item, which keeps the wire contract
// unchanged). A decided proposal is left untouched — the item's terminal status
// admits no supersession — so only a genuinely open card is withdrawn.
func (tx *WriteTx) SupersedeIntakeProposal(
	ctx context.Context, repositoryID int64, issueNumber int, label string, ordinal int,
	reason domain.IntakeSupersessionReason, newState domain.IntakeOccurrenceState, now time.Time,
) (domain.IntakeOccurrence, error) {
	if newState == domain.IntakeOccurrencePresent {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake supersession must leave present: %w", ErrImmutableConflict)
	}
	// The reason must name the event that produced this exact departure: a
	// removed label leaves the occurrence absent, a closed issue leaves it
	// closed. A mismatched pair would persist a reason and state that disagree,
	// so audit and reconciliation could not tell which event ended the
	// occurrence.
	if !intakeSupersessionReasonMatchesState(reason, newState) {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake supersession reason %s does not match state %s: %w", reason, newState, ErrImmutableConflict)
	}
	occurrence, err := tx.GetIntakeOccurrence(ctx, repositoryID, issueNumber, label, ordinal)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	if occurrence.Admission == nil {
		return domain.IntakeOccurrence{}, fmt.Errorf(
			"intake supersession without an admitted proposal: %w", ErrImmutableConflict)
	}
	if occurrence.State != newState {
		if !occurrence.State.CanTransitionTo(newState) {
			return domain.IntakeOccurrence{}, fmt.Errorf(
				"intake supersession %s -> %s: %w", occurrence.State, newState, ErrImmutableConflict)
		}
		// Restamp RecordedAt with the transition instant (see RecordIntake
		// Observation); the Supersession carries its own RecordedAt separately.
		occurrence.State = newState
		occurrence.RecordedAt = now.UTC()
	}
	// Record the supersession fact only when a genuinely open proposal was
	// withdrawn now. If the operator has already started or declined the
	// proposal, its item is terminal and untouched, so stamping Supersession
	// would falsely claim a withdrawal the decision ledger contradicts; the
	// occurrence still leaves present, just without a supersession record. A
	// replay (the record already exists) still enforces reason agreement.
	superseded, err := tx.supersedeOpenProposalItem(ctx, occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	switch {
	case occurrence.Supersession != nil:
		if occurrence.Supersession.Reason != reason {
			return domain.IntakeOccurrence{}, fmt.Errorf(
				"intake occurrence already superseded for %s: %w", occurrence.Supersession.Reason, ErrImmutableConflict)
		}
	case superseded:
		occurrence.Supersession = &domain.IntakeSupersession{Reason: reason, RecordedAt: now.UTC()}
	}
	if err := tx.putIntakeOccurrence(ctx, occurrence); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	return occurrence, nil
}

// supersedeOpenProposalItem transitions the proposal's attention item to
// superseded when it is still open, reporting whether it did. A decided or
// already-terminal item is left unchanged (the item's status is the
// DecidedAt-guarded authority) and reported as not superseded, so the caller
// records the supersession fact only when a withdrawal actually happened.
func (tx *WriteTx) supersedeOpenProposalItem(
	ctx context.Context, instanceID domain.ProposalInstanceID, admittedDigest domain.Digest,
) (bool, error) {
	item, err := tx.authenticatedProposalItem(ctx, instanceID, admittedDigest)
	if err != nil {
		return false, err
	}
	if item.Status != domain.StatusOpen || item.DecidedAt != nil {
		return false, nil
	}
	superseded := item
	superseded.Status = domain.StatusSuperseded
	superseded.ItemVersion = item.ItemVersion + 1
	if err := tx.PutAttentionItem(ctx, superseded); err != nil {
		return false, fmt.Errorf("intake supersession item: %w", err)
	}
	return true, nil
}

// authenticatedProposalItem resolves the proposal instance's attention item
// through the proposal-item binding and confirms the binding names this exact
// instance AND renders the exact proposal content the occurrence admitted, so
// neither a missing/mis-pointed binding nor a same-instance revision can let
// intake inspect or withdraw a card whose content was never admitted.
// ProposalForItem is the authoritative item↔instance boundary; the admitted
// digest pins the content that boundary returns. Read-only, so the read re-gate
// (verifyIntakeAdmission) shares it with the write-path freshness guards.
func (tx *ReadTx) authenticatedProposalItem(
	ctx context.Context, instanceID domain.ProposalInstanceID, admittedDigest domain.Digest,
) (domain.AttentionItem, error) {
	itemID := domain.ItemID(instanceID)
	instance, proposal, err := tx.ProposalForItem(ctx, itemID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("intake proposal item binding: %w", err)
	}
	if instance.ID != instanceID || proposal.Digest != admittedDigest {
		return domain.AttentionItem{}, fmt.Errorf(
			"intake proposal item %q does not render the admitted proposal: %w", itemID, ErrIntakeAdmissionInconsistent)
	}
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("intake proposal item: %w", err)
	}
	// ProposalForItem authenticates the instance↔item binding and content digest
	// but not the item's semantic type, so a tampered attention_items row that
	// keeps the id, project, and digest while changing Type or Subject.Type would
	// still resolve here. A label admission withdraws or freshness-checks only a
	// run proposal card over a proposal batch, so require both, else a
	// supersession could act on a card of another kind. Fail closed.
	if item.Type != domain.AttentionRunProposal || item.Subject.Type != domain.SubjectProposalBatch {
		return domain.AttentionItem{}, fmt.Errorf(
			"intake proposal item %q is not a run proposal over a proposal batch: %w", itemID, ErrIntakeAdmissionInconsistent)
	}
	return item, nil
}

// proposalItemOpen reports whether the proposal instance's authenticated
// attention item is still an open, undecided card. A missing item or binding is
// an error, not "not open": an admitted occurrence always has one, and reading
// absence as closed would let a freshness guard pass on a corrupt state instead
// of failing closed.
func (tx *WriteTx) proposalItemOpen(
	ctx context.Context, instanceID domain.ProposalInstanceID, admittedDigest domain.Digest,
) (bool, error) {
	item, err := tx.authenticatedProposalItem(ctx, instanceID, admittedDigest)
	if err != nil {
		return false, err
	}
	return item.Status == domain.StatusOpen && item.DecidedAt == nil, nil
}

// intakeSupersessionReasonMatchesState pairs each supersession reason with the
// state it produces: a removed label leaves the occurrence absent, a closed
// issue leaves it closed. The switch dispatches on the reason and so omits
// default; the trailing return rejects the invalid zero value and any other
// pairing.
func intakeSupersessionReasonMatchesState(
	reason domain.IntakeSupersessionReason, state domain.IntakeOccurrenceState,
) bool {
	switch reason {
	case domain.IntakeSupersededLabelRemoved:
		return state == domain.IntakeOccurrenceAbsent
	case domain.IntakeSupersededIssueClosed:
		return state == domain.IntakeOccurrenceClosed
	}
	return false
}

// intakeAdmissionEqual compares two admissions by value. The subject carries a
// pointer (the issue-subject arm), so a struct == would test pointer identity;
// canonical JSON compares the values a replay must converge on.
func intakeAdmissionEqual(a, b domain.IntakeAdmission) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
