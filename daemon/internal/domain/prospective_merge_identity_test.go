package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// reentryMerge is a valid prospective merge identity: three distinct commits.
func reentryMerge() domain.ProspectiveMergeIdentity {
	return domain.ProspectiveMergeIdentity{BaseSHA: "base-2", HeadSHA: "head-1", MergeSHA: "merge-1"}
}

func TestProspectiveMergeIdentityGolden(t *testing.T) {
	merge := reentryMerge()
	if err := merge.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(merge, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "prospective_merge_identity", append(body, '\n'))
}

func TestProspectiveMergeIdentityValidation(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		mutate  func(*domain.ProspectiveMergeIdentity)
		wantErr error
	}{
		"no base":         {func(m *domain.ProspectiveMergeIdentity) { m.BaseSHA = "" }, domain.ErrEmptyField},
		"no head":         {func(m *domain.ProspectiveMergeIdentity) { m.HeadSHA = "" }, domain.ErrEmptyField},
		"no merge":        {func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = "" }, domain.ErrEmptyField},
		"head is base":    {func(m *domain.ProspectiveMergeIdentity) { m.HeadSHA = m.BaseSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"merge is base":   {func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = m.BaseSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"merge is head":   {func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = m.HeadSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"all one commit":  {func(m *domain.ProspectiveMergeIdentity) { m.HeadSHA, m.MergeSHA = m.BaseSHA, m.BaseSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"empty identity":  {func(m *domain.ProspectiveMergeIdentity) { *m = domain.ProspectiveMergeIdentity{} }, domain.ErrEmptyField},
		"only the merge":  {func(m *domain.ProspectiveMergeIdentity) { m.BaseSHA, m.HeadSHA = "", "" }, domain.ErrEmptyField},
		"only the parent": {func(m *domain.ProspectiveMergeIdentity) { m.HeadSHA, m.MergeSHA = "", "" }, domain.ErrEmptyField},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			merge := reentryMerge()
			test.mutate(&merge)
			if err := merge.Validate(); !errors.Is(err, test.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
