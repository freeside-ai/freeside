package domain

import (
	"fmt"
	"time"
)

// ExportRejection is the durable, write-once diagnostic record of why the
// gauntlet definitively rejected one admitted attempt's released export: the
// per-finding detail (kind, path, and the finding's own locating fields) the
// containment reported. It exists so an operator can diagnose a rejection
// after the released export directory is cleaned, when the finding detail
// would otherwise survive only as a count in the failed outcome's summary.
//
// It is diagnostic, not terminal authority: it sits beside the
// ExecutionOutcome(failed) the same rejection records, never in place of it,
// and it is deliberately not mutually exclusive with either terminal-authority
// row. The finding shape mirrors importer.Finding but is spelled here in plain
// fields because domain must not import importer (that package imports this
// one); Kind is a plain string for the same reason.
//
// This record is daemon-internal: it is not carried on the §5.14 sync surface
// and holds no entity_version/as_of_revision. Surfacing rejection detail to
// the operator UI is future contract work; the moment a field of this type
// (or its findings) is read by an API/app client, this stops being a
// daemon-only record and the change becomes kind:contract (issue #768).
type ExportRejection struct {
	InvocationID InvocationID `json:"invocation_id"`
	AdmissionID  Digest       `json:"admission_id"`
	// Findings is a bounded diagnostic subset — the writer caps how many it
	// persists so an adversarial export with a candidate-controlled flood of
	// findings cannot bloat this permanent row and every backup checkpoint.
	Findings []ExportRejectionFinding `json:"findings"`
	// TotalFindings is the true count the gauntlet reported, which Findings may
	// truncate. It is the count the failed-outcome summary states, so a recovery
	// path that reconstructs the summary from this record alone reproduces the
	// exact byte form the live path wrote and converges on the write-once
	// outcome.
	TotalFindings int       `json:"total_findings"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// ExportRejectionFinding mirrors one importer.Finding for the durable record.
// Path and PathHex are mutually exclusive as in the manifest (a canonical path
// or the raw name bytes of an invalid one); Rule and Line locate a secret
// finding. No field ever carries candidate content bytes.
type ExportRejectionFinding struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	PathHex string `json:"path_hex,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Line    int    `json:"line,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// ExportRejectionInput carries the caller-supplied fields of an
// ExportRejection; it exists for symmetry with the other execution
// constructors and to detach the findings from the caller's slice.
type ExportRejectionInput struct {
	InvocationID  InvocationID
	AdmissionID   Digest
	Findings      []ExportRejectionFinding
	TotalFindings int
	RecordedAt    time.Time
}

// NewExportRejection builds a validated rejection record in canonical
// byte-form, detaching the findings slice and normalizing the timestamp to
// UTC, so a replayed record of the same rejection converges on the stored body
// instead of colliding under a false immutable conflict.
func NewExportRejection(in ExportRejectionInput) (ExportRejection, error) {
	r := ExportRejection{
		InvocationID:  in.InvocationID,
		AdmissionID:   in.AdmissionID,
		Findings:      append([]ExportRejectionFinding(nil), in.Findings...),
		TotalFindings: in.TotalFindings,
		RecordedAt:    in.RecordedAt.UTC(),
	}
	if err := r.Validate(); err != nil {
		return ExportRejection{}, err
	}
	return r, nil
}

// Validate reports whether the rejection record is well-formed. It checks the
// record alone; that it belongs to its admission is
// ValidateExportRejectionBinding's question, since this value does not carry
// the admission.
func (r ExportRejection) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("export rejection invocation_id: %w", ErrEmptyID)
	}
	if r.AdmissionID == "" {
		return fmt.Errorf("export rejection %s admission_id: %w", r.InvocationID, ErrEmptyID)
	}
	if len(r.Findings) == 0 {
		return fmt.Errorf("export rejection %s: %w", r.InvocationID, ErrExportRejectionEmpty)
	}
	// TotalFindings is the true count Findings may truncate, so it can never be
	// fewer than the retained subset. The cap itself is the writer's policy, not
	// a domain invariant.
	if r.TotalFindings < len(r.Findings) {
		return fmt.Errorf("export rejection %s total_findings %d below retained %d: %w",
			r.InvocationID, r.TotalFindings, len(r.Findings), ErrOutcomeInconsistent)
	}
	for i, f := range r.Findings {
		if f.Kind == "" {
			return fmt.Errorf("export rejection %s finding %d kind: %w", r.InvocationID, i, ErrEmptyField)
		}
		if f.Path != "" && f.PathHex != "" {
			return fmt.Errorf("export rejection %s finding %d: %w", r.InvocationID, i, ErrFindingPathConflict)
		}
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("export rejection %s recorded_at: %w", r.InvocationID, ErrMissingTimestamp)
	}
	// One instant has one byte form, so a replayed record converges instead of
	// colliding under a false immutable conflict.
	if r.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("export rejection %s recorded_at: %w", r.InvocationID, ErrTimestampNotUTC)
	}
	return nil
}

// ValidateExportRejectionBinding requires the same invocation and admission,
// and forward-moving time, as the trusted admission that authorized the
// attempt: a rejection is what an admitted attempt's export was found to be,
// so it cannot name another admission or predate the admission itself.
func ValidateExportRejectionBinding(a ExecutionAdmission, r ExportRejection) error {
	if r.InvocationID != a.InvocationID || r.AdmissionID != a.ID {
		return fmt.Errorf("export rejection %s disagrees with admission %s: %w",
			r.InvocationID, a.InvocationID, ErrParentKeyMismatch)
	}
	if r.RecordedAt.Before(a.AdmittedAt) {
		return fmt.Errorf("export rejection %s recorded %s, admitted %s: %w",
			r.InvocationID, r.RecordedAt, a.AdmittedAt, ErrTimestampOutOfOrder)
	}
	return nil
}
