package domain

import "fmt"

// Run is one workflow execution over an approved spec (plan §5.12). The spec is
// digest-addressed; stages and their attempts hold the execution structure.
type Run struct {
	ID           RunID     `json:"id"`
	ProjectID    ProjectID `json:"project_id"`
	SpecDigest   Digest    `json:"spec_digest"`
	PolicyDigest Digest    `json:"policy_digest"`
	Stages       []Stage   `json:"stages"`
}

// Validate reports whether the run is well-formed. A run is project-scoped and
// bound to a digested per-run policy (plan §3.2, §5.12), so both keys are
// required; each stage must both validate and name this run, since RunID is the
// join key persisted rows are reconstructed under.
func (r Run) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("run id: %w", ErrEmptyID)
	}
	if r.ProjectID == "" {
		return fmt.Errorf("run %s project_id: %w", r.ID, ErrEmptyID)
	}
	if r.SpecDigest == "" {
		return fmt.Errorf("run %s spec_digest: %w", r.ID, ErrEmptyField)
	}
	if r.PolicyDigest == "" {
		return fmt.Errorf("run %s policy_digest: %w", r.ID, ErrEmptyField)
	}
	seenStages := make(map[StageID]struct{}, len(r.Stages))
	// invocation_id is the run-wide reconciliation key (plan §5.3: one committed
	// invocation intent, at most one accepted result), so it must be unique
	// across the whole run, not merely within a stage.
	seenInvocation := make(map[InvocationID]struct{})
	for _, s := range r.Stages {
		if err := s.Validate(); err != nil {
			return err
		}
		if s.RunID != r.ID {
			return fmt.Errorf("stage %s run_id %q under run %s: %w", s.ID, s.RunID, r.ID, ErrParentKeyMismatch)
		}
		if _, dup := seenStages[s.ID]; dup {
			return fmt.Errorf("stage %s: %w", s.ID, ErrDuplicate)
		}
		seenStages[s.ID] = struct{}{}
		for _, a := range s.Attempts {
			if _, dup := seenInvocation[a.InvocationID]; dup {
				return fmt.Errorf("attempt %s invocation_id %s reused across run %s: %w", a.ID, a.InvocationID, r.ID, ErrDuplicate)
			}
			seenInvocation[a.InvocationID] = struct{}{}
		}
	}
	return nil
}

// Stage is one bounded phase of a run (elaboration, implementation, review,
// verification), holding its attempts (plan §5.3, §5.12).
type Stage struct {
	ID       StageID   `json:"id"`
	RunID    RunID     `json:"run_id"`
	Name     string    `json:"name"`
	Attempts []Attempt `json:"attempts"`
}

// Validate reports whether the stage is well-formed. RunID is the parent join
// key, so it is required; each attempt must both validate and name this stage.
func (s Stage) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("stage id: %w", ErrEmptyID)
	}
	if s.RunID == "" {
		return fmt.Errorf("stage %s run_id: %w", s.ID, ErrEmptyID)
	}
	if s.Name == "" {
		return fmt.Errorf("stage %s name: %w", s.ID, ErrEmptyField)
	}
	// Attempt identity and ordinals are the keys reconstruction and retry
	// ordering join on, so each must be unique within the stage.
	seenID := make(map[AttemptID]struct{}, len(s.Attempts))
	seenInvocation := make(map[InvocationID]struct{}, len(s.Attempts))
	for idx, a := range s.Attempts {
		if err := a.Validate(); err != nil {
			return err
		}
		if a.StageID != s.ID {
			return fmt.Errorf("attempt %s stage_id %q under stage %s: %w", a.ID, a.StageID, s.ID, ErrParentKeyMismatch)
		}
		if _, dup := seenID[a.ID]; dup {
			return fmt.Errorf("attempt id %s: %w", a.ID, ErrDuplicate)
		}
		// Number is the retry ordinal and Attempts is the serialized history, so
		// the numbers must run 1, 2, 3, ... in slice order (this subsumes
		// positivity and uniqueness).
		if want := idx + 1; a.Number != want {
			return fmt.Errorf("attempt %s number %d, want %d: %w", a.ID, a.Number, want, ErrNonContiguous)
		}
		if _, dup := seenInvocation[a.InvocationID]; dup {
			return fmt.Errorf("attempt %s invocation_id %s: %w", a.ID, a.InvocationID, ErrDuplicate)
		}
		seenID[a.ID] = struct{}{}
		seenInvocation[a.InvocationID] = struct{}{}
	}
	return nil
}

// Attempt is one execution of a stage, bound to the daemon-generated
// invocation id that makes the external start reconcilable (plan §5.3): one
// committed invocation intent, at most one accepted result.
type Attempt struct {
	ID           AttemptID    `json:"id"`
	StageID      StageID      `json:"stage_id"`
	Number       int          `json:"number"`
	InvocationID InvocationID `json:"invocation_id"`
}

// Validate reports whether the attempt is well-formed. StageID is the parent
// join key, so it is required.
func (a Attempt) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("attempt id: %w", ErrEmptyID)
	}
	if a.StageID == "" {
		return fmt.Errorf("attempt %s stage_id: %w", a.ID, ErrEmptyID)
	}
	if a.InvocationID == "" {
		return fmt.Errorf("attempt %s invocation_id: %w", a.ID, ErrEmptyID)
	}
	if a.Number < 1 {
		return fmt.Errorf("attempt %s number %d: %w", a.ID, a.Number, ErrNonPositive)
	}
	return nil
}
