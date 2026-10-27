package projectimage

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestClassifyUpkeepFindingsPrefersAClassThatOpensNoTask(t *testing.T) {
	for _, test := range []struct {
		clauses []RebuildRefusal
		want    domain.ImageUpkeepRefusalClass
	}{
		{[]RebuildRefusal{RebuildRefusalRecipeChanged}, domain.ImageUpkeepRefusalPolicy},
		{[]RebuildRefusal{RebuildRefusalUndeclaredAuthority, RebuildRefusalNoRegistrySet}, domain.ImageUpkeepRefusalPolicy},
		{[]RebuildRefusal{RebuildRefusalRecipeChanged, RebuildRefusalUnpinnedSource}, domain.ImageUpkeepRefusalRepair},
		{[]RebuildRefusal{RebuildRefusalNoRegistrySet, RebuildRefusalLockfileInconsistent}, domain.ImageUpkeepRefusalRepair},
		{[]RebuildRefusal{RebuildRefusalProofFailed}, domain.ImageUpkeepRefusalRepair},
		{[]RebuildRefusal{RebuildRefusalUnpinnedSource, RebuildRefusalUnsupportedInput}, domain.ImageUpkeepRefusalUnsupportedNPMInput},
		{[]RebuildRefusal{RebuildRefusalUnsupportedInput, RebuildRefusalGoDependencyChanged}, domain.ImageUpkeepRefusalGoModuleDelta},
		{[]RebuildRefusal{RebuildRefusalRecipeChanged, RebuildRefusalGoDependencyChanged}, domain.ImageUpkeepRefusalGoModuleDelta},
		{[]RebuildRefusal{RebuildRefusalNotConfigured}, domain.ImageUpkeepRefusalHostFault},
	} {
		findings := make([]domain.ImageUpkeepClauseFinding, 0, len(test.clauses))
		for _, clause := range test.clauses {
			findings = append(findings, domain.ImageUpkeepClauseFinding{Clause: clause, Detail: "detail"})
		}
		if got, err := ClassifyUpkeepFindings(findings); err != nil || got != test.want {
			t.Errorf("%v: class = %q, %v; want %q", test.clauses, got, err, test.want)
		}
	}
}

func TestClassifyUpkeepFindingsClassifiesEveryClause(t *testing.T) {
	for _, clause := range AllRebuildRefusals {
		if _, err := ClassifyUpkeepFindings([]domain.ImageUpkeepClauseFinding{{Clause: clause}}); err != nil {
			t.Errorf("%s: %v", clause, err)
		}
	}
	for _, findings := range [][]domain.ImageUpkeepClauseFinding{nil, {{Clause: "bogus"}}} {
		if _, err := ClassifyUpkeepFindings(findings); !errors.Is(err, domain.ErrImageUpkeepRefusalInvalid) {
			t.Errorf("%+v: err = %v, want ErrImageUpkeepRefusalInvalid", findings, err)
		}
	}
}

func TestClassifyUpkeepBuildFailureNamesARepairOnlyOnPositiveEvidence(t *testing.T) {
	for _, test := range []struct {
		err  error
		want domain.ImageUpkeepRefusalClass
	}{
		{fmt.Errorf("proof: %w", ErrProofFailed), domain.ImageUpkeepRefusalRepair},
		{fmt.Errorf("install: %w", ErrInstallFailed), domain.ImageUpkeepRefusalRepair},
		{fmt.Errorf("request: %w", ErrInvalidRequest), domain.ImageUpkeepRefusalHostFault},
		{errors.New("container runtime unavailable"), domain.ImageUpkeepRefusalHostFault},
	} {
		if got := ClassifyUpkeepBuildFailure(test.err); got != test.want {
			t.Errorf("%v: class = %q, want %q", test.err, got, test.want)
		}
	}
}
