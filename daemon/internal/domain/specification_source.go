package domain

import "fmt"

// IssueSubjectRef names an occurrence-bound issue subject: the repository
// identity pair (the BaseRevision convention) and issue number a label-intake
// specification is about. It carries no issue content by design; the issue's
// title and body enter specification only as specifier-fetched research, never
// as authority (plan §5.12). RepositoryID is the forge's canonical numeric
// identity, so a transferred or reused name can never redirect the subject.
type IssueSubjectRef struct {
	Repo         string `json:"repo"`
	RepositoryID int64  `json:"repository_id"`
	IssueNumber  int    `json:"issue_number"`
}

// Validate reports whether the reference is well-formed.
func (r IssueSubjectRef) Validate() error {
	if r.Repo == "" {
		return fmt.Errorf("issue subject repo: %w", ErrEmptyField)
	}
	if r.RepositoryID <= 0 {
		return fmt.Errorf("issue subject repository_id %d: %w", r.RepositoryID, ErrNonPositive)
	}
	if r.IssueNumber <= 0 {
		return fmt.Errorf("issue subject issue_number %d: %w", r.IssueNumber, ErrNonPositive)
	}
	return nil
}

// SpecificationSource is the typed union naming what a start specification
// assembles its subject from. Exactly one arm is present, consistent with
// Kind: a pre-registered work-item artifact (the freesided submit path), or
// an occurrence-bound issue subject (the label-intake path). Widening the
// specification intake to this union is what lets label intake name its subject
// without minting a placeholder specification artifact and without admitting
// observed issue content as authority. The daemon-owned reconciliation loop
// (label-initiator intake) resolves the named arm into a concrete specification
// run; this contract type only makes the two intakes nameable side by side.
type SpecificationSource struct {
	Kind SpecificationSourceKind `json:"kind"`
	// WorkItemArtifactID is the pre-registered specification artifact for the
	// work_item_artifact arm; empty otherwise.
	WorkItemArtifactID ArtifactID `json:"work_item_artifact_id,omitempty"`
	// IssueSubject names the occurrence-bound issue for the issue_subject arm;
	// nil otherwise. Rendered as explicit null when absent (the golden
	// pointer-for-optional convention), so the wire shape pins the absent arm.
	IssueSubject *IssueSubjectRef `json:"issue_subject"`
}

// Validate reports whether exactly the fields of the declared arm are set. The
// arm switch dispatches per-kind structure and so omits default; a valid kind
// is guaranteed by the membership check above, and the trailing return guards
// the invalid zero value.
func (s SpecificationSource) Validate() error {
	if !s.Kind.valid() {
		return fmt.Errorf("specification source kind %q: %w", s.Kind, ErrInvalidSpecificationSourceKind)
	}
	switch s.Kind {
	case SpecificationSourceWorkItemArtifact:
		if s.WorkItemArtifactID == "" {
			return fmt.Errorf("specification source work_item_artifact_id: %w", ErrEmptyID)
		}
		if s.IssueSubject != nil {
			return fmt.Errorf("work_item_artifact source carries an issue subject: %w", ErrSpecificationSourceInconsistent)
		}
		return nil
	case SpecificationSourceIssueSubject:
		if s.WorkItemArtifactID != "" {
			return fmt.Errorf("issue_subject source carries a spec artifact: %w", ErrSpecificationSourceInconsistent)
		}
		if s.IssueSubject == nil {
			return fmt.Errorf("issue_subject source has no issue subject: %w", ErrSpecificationSourceInconsistent)
		}
		return s.IssueSubject.Validate()
	}
	return ErrSpecificationSourceInconsistent
}
