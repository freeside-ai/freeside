package domain

import "fmt"

// SourceIssueClosureParameters is the fixed parameter type for the
// source_issue_closure effect kind (plan §5.13). It names the work unit whose
// pull request this is, the issue the daemon selected to close, whether this
// proposal would actually write the close, and the trust the target earns. No
// field is authority: the store re-derives the closable source from current
// rows and re-gates every stored value through GateSourceIssueClosure.
type SourceIssueClosureParameters struct {
	// SubjectHandle is the opaque work-unit handle, reused from the task
	// proposal so the existing subject_handle column and policy lookup apply.
	SubjectHandle OpaqueSubjectHandle `json:"subject_handle"`
	// Target is the issue the daemon selected. It is never a value an agent
	// supplied directly; for a recommended provenance the daemon confirmed it
	// names the same repository id. Its name is the one current when the
	// proposal was made and may predate a rename.
	Target IssueSubjectRef `json:"target"`
	// Resolves reports whether this proposal, if approved, would write the
	// close. A daemon_fallback proposal always carries false.
	Resolves bool `json:"resolves"`
	// Provenance is the trust the target earns (verified or recommended).
	Provenance ClosureProvenance `json:"provenance"`
	// Origin records which mechanism produced the proposal.
	Origin ClosureFlagOrigin `json:"origin"`
}

// Validate reports whether the parameters are structurally sound. The
// daemon_fallback invariant (resolves must be false) is enforced here so a
// decoded row cannot claim a fallback that would nonetheless close an issue.
func (p SourceIssueClosureParameters) Validate() error {
	if p.SubjectHandle == "" {
		return fmt.Errorf("source issue closure subject_handle: %w", ErrEmptyID)
	}
	if err := p.Target.Validate(); err != nil {
		return fmt.Errorf("source issue closure target: %w", err)
	}
	if !p.Provenance.valid() {
		return fmt.Errorf("source issue closure provenance %q: %w", p.Provenance, ErrEffectProposalInconsistent)
	}
	if !p.Origin.valid() {
		return fmt.Errorf("source issue closure origin %q: %w", p.Origin, ErrEffectProposalInconsistent)
	}
	if p.Origin == ClosureFlagOriginDaemonFallback && p.Resolves {
		return fmt.Errorf("daemon_fallback closure with resolves set: %w", ErrEffectProposalInconsistent)
	}
	return nil
}

// ClosableSource is the daemon's trusted determination, from current durable
// state, of how a work unit's pull request may close a source issue. The store
// computes it from live rows and passes it to GateSourceIssueClosure; it is
// never taken from a proposal body. A recommended determination leaves
// IssueNumber zero because the client chose the exact issue and the daemon
// cannot re-derive it (#1417); the approval binding pins the proposal digest so
// a later change to that number voids the approval.
type ClosableSource struct {
	// Present reports whether the work unit has any closable source. When
	// false no closure proposal is authorized (fail closed).
	Present bool
	// Provenance is the trust the determination earns.
	Provenance ClosureProvenance
	// Repo and RepositoryID are the repository the work targets; RepositoryID
	// is the forge's authoritative numeric identity used for equality.
	Repo         string
	RepositoryID int64
	// IssueNumber is the daemon-bound issue for a verified determination and
	// zero for a recommended one.
	IssueNumber int
}

// Validate reports whether the determination is internally consistent.
func (c ClosableSource) Validate() error {
	if !c.Present {
		if c.Provenance != "" || c.Repo != "" || c.RepositoryID != 0 || c.IssueNumber != 0 {
			return fmt.Errorf("absent closable source carries fields: %w", ErrClosableSourceInconsistent)
		}
		return nil
	}
	if !c.Provenance.valid() {
		return fmt.Errorf("closable source provenance %q: %w", c.Provenance, ErrClosableSourceInconsistent)
	}
	if c.Repo == "" {
		return fmt.Errorf("closable source repo: %w", ErrClosableSourceInconsistent)
	}
	if c.RepositoryID <= 0 {
		return fmt.Errorf("closable source repository_id %d: %w", c.RepositoryID, ErrClosableSourceInconsistent)
	}
	switch c.Provenance {
	case ClosureProvenanceVerified:
		if c.IssueNumber <= 0 {
			return fmt.Errorf("verified closable source issue_number %d: %w", c.IssueNumber, ErrClosableSourceInconsistent)
		}
	case ClosureProvenanceRecommended:
		if c.IssueNumber != 0 {
			return fmt.Errorf("recommended closable source carries issue_number %d: %w", c.IssueNumber, ErrClosableSourceInconsistent)
		}
	}
	return nil
}

// SourceIssueClosureInput is the trusted construction input. The constructor
// derives provenance from the daemon's ClosableSource determination rather than
// from any caller claim, so a caller cannot mint verified authority.
type SourceIssueClosureInput struct {
	// SubjectHandle is the opaque work-unit handle.
	SubjectHandle OpaqueSubjectHandle
	// Source is the daemon's determination of the closable source.
	Source ClosableSource
	// ProposedTarget is the client-chosen target for a recommended source; it
	// is ignored for a verified source, where the daemon's bound issue is used.
	ProposedTarget IssueSubjectRef
	// Origin records which mechanism is constructing the proposal.
	Origin ClosureFlagOrigin
	// Resolves is the requested resolve flag; it is forced false for a
	// daemon_fallback origin.
	Resolves bool
}

// parameters derives the stored parameters from the trusted input.
func (in SourceIssueClosureInput) parameters() (SourceIssueClosureParameters, error) {
	if in.SubjectHandle == "" {
		return SourceIssueClosureParameters{}, fmt.Errorf("source issue closure subject_handle: %w", ErrEmptyID)
	}
	if !in.Origin.valid() {
		return SourceIssueClosureParameters{}, fmt.Errorf("source issue closure origin %q: %w", in.Origin, ErrEffectProposalInconsistent)
	}
	if err := in.Source.Validate(); err != nil {
		return SourceIssueClosureParameters{}, err
	}
	if !in.Source.Present {
		return SourceIssueClosureParameters{}, ErrClosableSourceAbsent
	}
	target, err := in.closureTarget()
	if err != nil {
		return SourceIssueClosureParameters{}, err
	}
	resolves := in.Resolves
	if in.Origin == ClosureFlagOriginDaemonFallback {
		resolves = false
	}
	params := SourceIssueClosureParameters{
		SubjectHandle: in.SubjectHandle, Target: target, Resolves: resolves,
		Provenance: in.Source.Provenance, Origin: in.Origin,
	}
	if err := params.Validate(); err != nil {
		return SourceIssueClosureParameters{}, err
	}
	return params, nil
}

// closureTarget resolves the target from the source determination. The switch
// dispatches behaviour and so omits default; the trailing return guards an
// invalid provenance that Validate did not already reject.
func (in SourceIssueClosureInput) closureTarget() (IssueSubjectRef, error) {
	switch in.Source.Provenance {
	case ClosureProvenanceVerified:
		return IssueSubjectRef{
			Repo: in.Source.Repo, RepositoryID: in.Source.RepositoryID, IssueNumber: in.Source.IssueNumber,
		}, nil
	case ClosureProvenanceRecommended:
		target := in.ProposedTarget
		if err := target.Validate(); err != nil {
			return IssueSubjectRef{}, fmt.Errorf("recommended closure target: %w", err)
		}
		// The client chose the issue number; the daemon confirms only that the
		// target names the same repository id, the identity a rename keeps
		// (#1537). A cross-repository target yields no proposal (plan §5.13).
		if target.RepositoryID != in.Source.RepositoryID {
			return IssueSubjectRef{}, fmt.Errorf("recommended closure target %q/%d outside source repository: %w",
				target.Repo, target.RepositoryID, ErrClosureTargetMismatch)
		}
		return target, nil
	}
	return IssueSubjectRef{}, ErrClosableSourceInconsistent
}

// GateSourceIssueClosure re-gates a closure proposal against the daemon's
// current closable-source determination. It rejects any proposal whose target
// or provenance the current state does not grant, and any proposal at all when
// there is no closable source (fail closed). The verified arm requires the
// same repository id and issue number; the recommended arm matches the
// repository id only, since the daemon cannot re-derive the client's chosen
// issue number. Neither compares names: the repository id is the identity, and
// a proposal made before a rename keeps the old name (#1537).
func GateSourceIssueClosure(proposal EffectProposal, closable ClosableSource) error {
	if proposal.Kind != EffectSourceIssueClosure || proposal.ClosureProposal == nil {
		return ErrEffectProposalInconsistent
	}
	if err := proposal.ClosureProposal.Validate(); err != nil {
		return err
	}
	if err := closable.Validate(); err != nil {
		return err
	}
	if !closable.Present {
		return ErrClosableSourceAbsent
	}
	c := proposal.ClosureProposal
	if c.Provenance != closable.Provenance {
		return ErrClosureProvenanceMismatch
	}
	switch closable.Provenance {
	case ClosureProvenanceVerified:
		if c.Target.RepositoryID != closable.RepositoryID || c.Target.IssueNumber != closable.IssueNumber {
			return ErrClosureTargetMismatch
		}
		return nil
	case ClosureProvenanceRecommended:
		if c.Target.RepositoryID != closable.RepositoryID {
			return ErrClosureTargetMismatch
		}
		return nil
	}
	return ErrClosableSourceInconsistent
}
