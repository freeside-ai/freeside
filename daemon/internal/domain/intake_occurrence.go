package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Label-intake occurrence contract (plan §5.12, issue #720). A labeled-issue
// occurrence is the durable binding the label initiator reconciles against:
// which (repository, issue, label) it observed, how many times the label has
// been continuously present-then-absent (the ordinal), the current observed
// state, and — once admission has run — the proposal admitted under the
// occurrence's derived key plus the daemon-selected subject a start assembles.
// The record is the authority later reconciliation stands on, so it validates
// like a contract type and the store re-gates it at reconstruction. Nothing
// here observes issue content: the issue's text is the elaborator's research,
// never authority.
//
// The decline latch is derived, not stored: an occurrence that stays present
// after its proposal reached a terminal decision admits nothing, and only a
// recorded absent (or a closed occurrence followed by a reopen observation)
// lets the initiator allocate ordinal n+1. Repeated same-state observations
// are idempotent, so polling converges.

// IntakeSubjectBinding is the daemon-selected subject and the exact
// elaboration/start inputs a start may assemble for an occurrence (plan §5.12,
// GQ1). It is minted at admission and names, rather than fetches, its inputs:
// the project, the work-unit subject the proposal's opaque handle resolves
// through, the policy artifact and its digest and the resolved-policy digest
// as read at admission, and the typed elaboration source. A start whose named
// input is missing or stale is refused with a durable reason; nothing falls
// back to a placeholder. Publication metadata is operator-authored and absent
// at label admission, so it is composed by the reconciliation loop, not bound
// here; the target repository is named by an issue_subject source.
type IntakeSubjectBinding struct {
	ProjectID            ProjectID         `json:"project_id"`
	WorkUnitID           WorkUnitID        `json:"work_unit_id"`
	ImplementationRunID  RunID             `json:"implementation_run_id"`
	PolicyArtifactID     ArtifactID        `json:"policy_artifact_id"`
	PolicyArtifactDigest Digest            `json:"policy_artifact_digest"`
	ResolvedPolicyDigest Digest            `json:"resolved_policy_digest"`
	Source               ElaborationSource `json:"source"`
}

// Validate reports whether the binding is well-formed. It does not resolve its
// named inputs against the store; that re-gate (input present, digest current)
// is the store's, and a missing or stale input is a durable refusal, not a
// malformed record.
func (b IntakeSubjectBinding) Validate() error {
	if b.ProjectID == "" {
		return fmt.Errorf("intake subject project_id: %w", ErrEmptyID)
	}
	if b.WorkUnitID == "" {
		return fmt.Errorf("intake subject work_unit_id: %w", ErrEmptyID)
	}
	if b.ImplementationRunID == "" {
		return fmt.Errorf("intake subject implementation_run_id: %w", ErrEmptyID)
	}
	if b.WorkUnitID != WorkUnitIDForRun(b.ImplementationRunID) {
		return fmt.Errorf("intake subject work_unit_id %q is not derived from run %q: %w",
			b.WorkUnitID, b.ImplementationRunID, ErrIntakeOccurrenceInconsistent)
	}
	if b.PolicyArtifactID == "" {
		return fmt.Errorf("intake subject policy_artifact_id: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(b.PolicyArtifactDigest)) {
		return fmt.Errorf("intake subject policy_artifact_digest %q: %w",
			b.PolicyArtifactDigest, ErrIntakeOccurrenceInconsistent)
	}
	if !contentaddr.Valid(string(b.ResolvedPolicyDigest)) {
		return fmt.Errorf("intake subject resolved_policy_digest %q: %w",
			b.ResolvedPolicyDigest, ErrIntakeOccurrenceInconsistent)
	}
	// The policy artifact is the resolved policy's content, so its digest equals
	// the resolved-policy digest (the invariant the elaboration path enforces:
	// policyArtifact.Digest == resolvedPolicy.Digest). A binding whose two policy
	// digests disagree could never have been the snapshot a start assembles.
	if b.PolicyArtifactDigest != b.ResolvedPolicyDigest {
		return fmt.Errorf("intake subject policy_artifact_digest %q differs from resolved_policy_digest %q: %w",
			b.PolicyArtifactDigest, b.ResolvedPolicyDigest, ErrIntakeOccurrenceInconsistent)
	}
	return b.Source.Validate()
}

// IntakeAdmission is the atomic admission binding: the proposal admitted under
// the occurrence's derived upstream-event key, and the daemon-selected subject
// the proposal's opaque handle resolves through. Both are written in one
// transaction or neither. The admission key is redundant with the occurrence's
// own derivation and is re-gated equal at reconstruction, so a tampered row
// cannot point the occurrence at a foreign proposal.
type IntakeAdmission struct {
	AdmissionKey       ProposalAdmissionKey `json:"admission_key"`
	ProposalInstanceID ProposalInstanceID   `json:"proposal_instance_id"`
	ProposalDigest     Digest               `json:"proposal_digest"`
	Subject            IntakeSubjectBinding `json:"subject"`
}

// Validate reports whether the admission is structurally well-formed. The
// cross-check that the admission key equals the occurrence's derived key is
// IntakeOccurrence.Validate's, which holds both the occurrence coordinates and
// this binding.
func (a IntakeAdmission) Validate() error {
	if err := a.AdmissionKey.Validate(); err != nil {
		return fmt.Errorf("intake admission key: %w", err)
	}
	if a.AdmissionKey.Source != ProposalSourceUpstreamEvent {
		return fmt.Errorf("intake admission key source %q: %w",
			a.AdmissionKey.Source, ErrIntakeOccurrenceInconsistent)
	}
	if a.ProposalInstanceID == "" {
		return fmt.Errorf("intake admission proposal_instance_id: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(a.ProposalDigest)) {
		return fmt.Errorf("intake admission proposal_digest %q: %w",
			a.ProposalDigest, ErrIntakeOccurrenceInconsistent)
	}
	return a.Subject.Validate()
}

// IntakeStartRefusal is a durable, re-readable record that a start was refused
// for the occurrence and the admitted item was left as an ordinary proposal.
type IntakeStartRefusal struct {
	Reason     IntakeStartRefusalReason `json:"reason"`
	RecordedAt time.Time                `json:"recorded_at"`
}

// Validate reports whether the refusal is well-formed.
func (r IntakeStartRefusal) Validate() error {
	if !r.Reason.valid() {
		return fmt.Errorf("intake start refusal reason %q: %w", r.Reason, ErrInvalidIntakeStartRefusalReason)
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("intake start refusal recorded_at: %w", ErrMissingTimestamp)
	}
	if r.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("intake start refusal recorded_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// IntakeSupersession records that the occurrence's still-open proposal was
// superseded, with its reason. It exists only where an admitted proposal did:
// a supersession without an admission has nothing to supersede.
type IntakeSupersession struct {
	Reason     IntakeSupersessionReason `json:"reason"`
	RecordedAt time.Time                `json:"recorded_at"`
}

// Validate reports whether the supersession is well-formed.
func (s IntakeSupersession) Validate() error {
	if !s.Reason.valid() {
		return fmt.Errorf("intake supersession reason %q: %w", s.Reason, ErrInvalidIntakeSupersessionReason)
	}
	if s.RecordedAt.IsZero() {
		return fmt.Errorf("intake supersession recorded_at: %w", ErrMissingTimestamp)
	}
	if s.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("intake supersession recorded_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// IntakeOccurrence is the durable label-intake occurrence aggregate. It is a
// current-state record: allocation writes a present, unbound occurrence;
// observation advances its state idempotently; admission binds a proposal and
// subject; refusal and supersession append their durable facts.
type IntakeOccurrence struct {
	Repo         string                `json:"repo"`
	RepositoryID int64                 `json:"repository_id"`
	IssueNumber  int                   `json:"issue_number"`
	Label        string                `json:"label"`
	Ordinal      int                   `json:"ordinal"`
	State        IntakeOccurrenceState `json:"state"`
	// Admission is present once admission has minted the proposal and subject
	// for this occurrence, nil before. Rendered as explicit null when absent
	// (the golden pointer-for-optional convention).
	Admission *IntakeAdmission `json:"admission"`
	// Refusal records a durable start refusal (WIP cap, mode, or missing/stale
	// subject input); present when a start was refused for this occurrence.
	// Explicit null when absent.
	Refusal *IntakeStartRefusal `json:"refusal"`
	// Supersession records that the admitted proposal was superseded (label
	// removed or issue closed); present only alongside an admission on a
	// no-longer-present occurrence. Explicit null when absent.
	Supersession *IntakeSupersession `json:"supersession"`
	// RecordedAt is the instant this occurrence reached its current State: set
	// at allocation and restamped by the store on each real state transition, so
	// it reports when the occurrence became present/absent/closed, not when it
	// was first allocated. Sub-facts (Refusal, Supersession) carry their own.
	RecordedAt time.Time `json:"recorded_at"`
}

// intakeOccurrenceSuccessors lists the states one occurrence may next hold,
// including itself for idempotent re-observation. Once absent or closed the
// occurrence never returns to present: a reappearing label is a new occurrence
// under the next ordinal, not this one revived. The switch dispatches on the
// state and so omits default.
func intakeOccurrenceSuccessors(s IntakeOccurrenceState) []IntakeOccurrenceState {
	switch s {
	case IntakeOccurrencePresent:
		return []IntakeOccurrenceState{IntakeOccurrencePresent, IntakeOccurrenceAbsent, IntakeOccurrenceClosed}
	case IntakeOccurrenceAbsent:
		return []IntakeOccurrenceState{IntakeOccurrenceAbsent, IntakeOccurrenceClosed}
	case IntakeOccurrenceClosed:
		return []IntakeOccurrenceState{IntakeOccurrenceClosed}
	}
	return nil
}

// CanTransitionTo reports whether an occurrence in state s may next hold next,
// the single definition the store's idempotent observation gate shares.
func (s IntakeOccurrenceState) CanTransitionTo(next IntakeOccurrenceState) bool {
	return slices.Contains(intakeOccurrenceSuccessors(s), next)
}

// UpstreamEventID derives the canonical proposal upstream-event occurrence
// identifier from the authenticated occurrence coordinates, so the admission
// key derives from the occurrence record and a replay converges through
// AllocateProposalInstance. Position is fixed and the leading and trailing
// segments are integers, so distinct occurrences never collide even when a
// label contains a slash; the value is well under MaxProposalOccurrenceIDBytes.
func (o IntakeOccurrence) UpstreamEventID() string {
	return fmt.Sprintf("label-intake/%d/%d/%s/%d", o.RepositoryID, o.IssueNumber, o.Label, o.Ordinal)
}

// ProposalAdmissionKey derives the occurrence's upstream-event admission key,
// the single source of the key admission allocates and reconciliation
// re-derives.
func (o IntakeOccurrence) ProposalAdmissionKey() ProposalAdmissionKey {
	return ProposalAdmissionKey{Source: ProposalSourceUpstreamEvent, UpstreamEventID: o.UpstreamEventID()}
}

// Validate reports whether the occurrence is well-formed and its bound facts
// are internally consistent: the derived admission key matches the recorded
// one, an issue_subject source names this occurrence's own issue, supersession
// exists only alongside an admission on a no-longer-present occurrence, and the
// derived upstream-event id is a valid occurrence identifier.
func (o IntakeOccurrence) Validate() error {
	if o.Repo == "" {
		return fmt.Errorf("intake occurrence repo: %w", ErrEmptyField)
	}
	if o.RepositoryID <= 0 {
		return fmt.Errorf("intake occurrence repository_id %d: %w", o.RepositoryID, ErrNonPositive)
	}
	if o.IssueNumber <= 0 {
		return fmt.Errorf("intake occurrence issue_number %d: %w", o.IssueNumber, ErrNonPositive)
	}
	if o.Label == "" {
		return fmt.Errorf("intake occurrence label: %w", ErrEmptyField)
	}
	if o.Ordinal < 1 {
		return fmt.Errorf("intake occurrence ordinal %d: %w", o.Ordinal, ErrNonPositive)
	}
	if !o.State.valid() {
		return fmt.Errorf("intake occurrence state %q: %w", o.State, ErrInvalidIntakeOccurrenceState)
	}
	if !validProposalOccurrenceID(o.UpstreamEventID()) {
		return fmt.Errorf("intake occurrence derives an invalid upstream-event id: %w", ErrIntakeOccurrenceInconsistent)
	}
	if o.RecordedAt.IsZero() {
		return fmt.Errorf("intake occurrence recorded_at: %w", ErrMissingTimestamp)
	}
	if o.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("intake occurrence recorded_at: %w", ErrTimestampNotUTC)
	}
	if o.Admission != nil {
		if err := o.Admission.Validate(); err != nil {
			return err
		}
		want, err := o.ProposalAdmissionKey().String()
		if err != nil {
			return err
		}
		got, err := o.Admission.AdmissionKey.String()
		if err != nil {
			return err
		}
		if want != got {
			return fmt.Errorf("intake admission key does not derive from the occurrence: %w", ErrIntakeOccurrenceInconsistent)
		}
		if err := o.validateAdmissionSubjectIssue(); err != nil {
			return err
		}
	}
	if o.Refusal != nil {
		// A start refusal leaves an admitted proposal as an ordinary card, so it
		// presupposes an admission: a refusal on an unadmitted occurrence names
		// no proposal that was "left", and reconciliation could consume it for a
		// nonexistent admission. Reject the shape at the boundary.
		if o.Admission == nil {
			return fmt.Errorf("intake refusal without an admitted proposal: %w", ErrIntakeOccurrenceInconsistent)
		}
		if err := o.Refusal.Validate(); err != nil {
			return err
		}
	}
	if o.Supersession != nil {
		if o.Admission == nil {
			return fmt.Errorf("intake supersession without an admitted proposal: %w", ErrIntakeOccurrenceInconsistent)
		}
		if o.State == IntakeOccurrencePresent {
			return fmt.Errorf("intake supersession while the label is present: %w", ErrIntakeOccurrenceInconsistent)
		}
		if err := o.Supersession.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateAdmissionSubjectIssue requires a label occurrence's admission subject
// to be the issue_subject arm naming this occurrence's own repository and issue.
// A label occurrence is intrinsically about its observed issue; the
// spec_artifact arm exists only for the operator's freesided submit path, which
// mints no occurrence. Accepting it here would let a tampered or mistaken
// binding name an arbitrary specification artifact as the occurrence's subject —
// the placeholder-artifact-as-authority GQ1 rejected — so an auto-start consumer
// could elaborate unrelated work instead of the observed issue. Fail closed.
func (o IntakeOccurrence) validateAdmissionSubjectIssue() error {
	source := o.Admission.Subject.Source
	if source.Kind != ElaborationSourceIssueSubject {
		return fmt.Errorf("intake admission subject must name the occurrence's issue, not a %q source: %w",
			source.Kind, ErrIntakeOccurrenceInconsistent)
	}
	ref := source.IssueSubject
	if ref.Repo != o.Repo || ref.RepositoryID != o.RepositoryID || ref.IssueNumber != o.IssueNumber {
		return fmt.Errorf("intake subject issue reference does not name the occurrence's issue: %w",
			ErrIntakeOccurrenceInconsistent)
	}
	return nil
}

// NewIntakeOccurrence builds a freshly allocated, present, unbound occurrence.
// The store assigns the ordinal under its write lock (the latch); this
// constructor stamps the instant in UTC and validates the shape.
func NewIntakeOccurrence(
	repo string, repositoryID int64, issueNumber int, label string, ordinal int, recordedAt time.Time,
) (IntakeOccurrence, error) {
	o := IntakeOccurrence{
		Repo: repo, RepositoryID: repositoryID, IssueNumber: issueNumber,
		Label: label, Ordinal: ordinal, State: IntakeOccurrencePresent,
		RecordedAt: recordedAt.UTC(),
	}
	if err := o.Validate(); err != nil {
		return IntakeOccurrence{}, err
	}
	return o, nil
}
