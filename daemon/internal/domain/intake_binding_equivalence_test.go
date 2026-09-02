package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestIntakeSubjectBindingRenameEquivalence is the refute-first equivalence
// harness the high-assurance profile requires for a behaviour-preserving change
// on a returned-object trust boundary (AGENTS.md): a diff-read only asserts the
// ImplementationRunID -> SpecificationRunID rename preserves the decoded-binding
// re-gate, so this reconstructs the pre-rename Validate and measures that old and
// new reach the same accept/reject decision, sentinel-for-sentinel, over a
// combinatorial corpus. The rename is behaviour-preserving iff every corpus point
// agrees; a silently swapped operand or comparison would diverge here.
//
// The corpus feeds each version its own field layout carrying the same logical
// values (old.ImplementationRunID == new.SpecificationRunID), which is the whole
// point: the wire key changed, so comparing identical bytes would diverge by
// construction; comparing the same logical binding measures that the
// authentication logic is unchanged.
func TestIntakeSubjectBindingRenameEquivalence(t *testing.T) {
	t.Parallel()

	validDigest := domain.Digest(contentaddr.Sum([]byte("resolved-policy")))
	otherDigest := domain.Digest(contentaddr.Sum([]byte("other-policy")))

	projectIDs := []domain.ProjectID{"", "proj-1"}
	runIDs := []domain.RunID{"", "run-spec-1"}
	// workUnit selector: 0 => derived from runID (valid), 1 => a foreign id, 2 => empty.
	workUnitSel := []int{0, 1, 2}
	policyArtifactIDs := []domain.ArtifactID{"", "policy-art-1"}
	policyArtifactDigests := []domain.Digest{validDigest, otherDigest, "not-a-digest", ""}
	resolvedPolicyDigests := []domain.Digest{validDigest, "not-a-digest", ""}
	sources := []domain.SpecificationSource{
		{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 42, IssueNumber: 7},
		},
		{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "spec-1"},
		{Kind: "bogus"}, // invalid source
	}

	workUnitFor := func(sel int, runID domain.RunID) domain.WorkUnitID {
		switch sel {
		case 0:
			return domain.WorkUnitIDForRun(runID)
		case 1:
			return "workunit-foreign"
		default:
			return ""
		}
	}

	corpus := 0
	for _, projectID := range projectIDs {
		for _, runID := range runIDs {
			for _, wuSel := range workUnitSel {
				for _, policyArtifactID := range policyArtifactIDs {
					for _, paDigest := range policyArtifactDigests {
						for _, rpDigest := range resolvedPolicyDigests {
							for i := range sources {
								workUnitID := workUnitFor(wuSel, runID)
								src := sources[i]
								oldErr := oldIntakeBindingValidate(oldIntakeSubjectBinding{
									ProjectID: projectID, WorkUnitID: workUnitID,
									ImplementationRunID:  runID,
									PolicyArtifactID:     policyArtifactID,
									PolicyArtifactDigest: paDigest,
									ResolvedPolicyDigest: rpDigest,
									Source:               src,
								})
								newErr := domain.IntakeSubjectBinding{
									ProjectID: projectID, WorkUnitID: workUnitID,
									SpecificationRunID:   runID,
									PolicyArtifactID:     policyArtifactID,
									PolicyArtifactDigest: paDigest,
									ResolvedPolicyDigest: rpDigest,
									Source:               src,
								}.Validate()
								if !sameBindingVerdict(oldErr, newErr) {
									t.Fatalf("rename changed the re-gate decision at corpus point %d: old=%v new=%v (project=%q run=%q wuSel=%d paID=%q paDig=%q rpDig=%q srcKind=%q)",
										corpus, oldErr, newErr, projectID, runID, wuSel, policyArtifactID, paDigest, rpDigest, src.Kind)
								}
								corpus++
							}
						}
					}
				}
			}
		}
	}
	if corpus == 0 {
		t.Fatal("empty corpus")
	}
}

// sameBindingVerdict reports whether two re-gate results are the same decision:
// both accept, or both reject wrapping the same sentinel. Messages differ only
// by the renamed field's name, so the sentinel identity is the comparable.
func sameBindingVerdict(oldErr, newErr error) bool {
	if (oldErr == nil) != (newErr == nil) {
		return false
	}
	if oldErr == nil {
		return true
	}
	for _, sentinel := range []error{
		domain.ErrEmptyID,
		domain.ErrIntakeOccurrenceInconsistent,
		domain.ErrInvalidSpecificationSourceKind,
		domain.ErrSpecificationSourceInconsistent,
		domain.ErrEmptyField,
		domain.ErrNonPositive,
	} {
		if errors.Is(oldErr, sentinel) != errors.Is(newErr, sentinel) {
			return false
		}
	}
	return true
}

// oldIntakeSubjectBinding + oldIntakeBindingValidate reconstruct the pre-rename
// (git show 04271c82~1) binding and its Validate verbatim, the only difference
// being the ImplementationRunID field name, so the harness measures the rename
// rather than re-testing the new code against itself.
type oldIntakeSubjectBinding struct {
	ProjectID            domain.ProjectID
	WorkUnitID           domain.WorkUnitID
	ImplementationRunID  domain.RunID
	PolicyArtifactID     domain.ArtifactID
	PolicyArtifactDigest domain.Digest
	ResolvedPolicyDigest domain.Digest
	Source               domain.SpecificationSource
}

func oldIntakeBindingValidate(b oldIntakeSubjectBinding) error {
	if b.ProjectID == "" {
		return domain.ErrEmptyID
	}
	if b.WorkUnitID == "" {
		return domain.ErrEmptyID
	}
	if b.ImplementationRunID == "" {
		return domain.ErrEmptyID
	}
	if b.WorkUnitID != domain.WorkUnitIDForRun(b.ImplementationRunID) {
		return domain.ErrIntakeOccurrenceInconsistent
	}
	if b.PolicyArtifactID == "" {
		return domain.ErrEmptyID
	}
	if !contentaddr.Valid(string(b.PolicyArtifactDigest)) {
		return domain.ErrIntakeOccurrenceInconsistent
	}
	if !contentaddr.Valid(string(b.ResolvedPolicyDigest)) {
		return domain.ErrIntakeOccurrenceInconsistent
	}
	if b.PolicyArtifactDigest != b.ResolvedPolicyDigest {
		return domain.ErrIntakeOccurrenceInconsistent
	}
	return b.Source.Validate()
}
