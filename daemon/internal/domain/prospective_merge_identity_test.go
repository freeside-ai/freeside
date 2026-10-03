package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// reentryMerge is the prospective merge of reentrySuccessor's head into its
// base.
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

// TestPublicationSuccessorAllowsProspectiveMerge pins the gate to one
// authority shape: a base_advanced re-entry, for the merge of exactly its
// head into exactly its base.
func TestPublicationSuccessorAllowsProspectiveMerge(t *testing.T) {
	t.Parallel()
	advanced := reentrySuccessor()
	merge := reentryMerge()
	if !advanced.AllowsProspectiveMerge(merge) {
		t.Fatal("base_advanced re-entry refused the merge of its own head into its own base")
	}

	for name, mutate := range map[string]func(*domain.ProspectiveMergeIdentity){
		"another base":    func(m *domain.ProspectiveMergeIdentity) { m.BaseSHA = "base-1" },
		"another head":    func(m *domain.ProspectiveMergeIdentity) { m.HeadSHA = "head-2" },
		"swapped parents": func(m *domain.ProspectiveMergeIdentity) { m.BaseSHA, m.HeadSHA = m.HeadSHA, m.BaseSHA },
		"no merge":        func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = "" },
		"merge is head":   func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = m.HeadSHA },
		"merge is base":   func(m *domain.ProspectiveMergeIdentity) { m.MergeSHA = m.BaseSHA },
	} {
		other := merge
		mutate(&other)
		if advanced.AllowsProspectiveMerge(other) {
			t.Fatalf("base_advanced re-entry admitted a merge with %s", name)
		}
	}

	headChanged := reentrySuccessor()
	headChanged.Reentry.Reason = domain.ReadinessInvalidationHeadChanged
	// The v3 shape with coordinates that match, under an origin or version the
	// re-entry rules refuse: a decoded record is never trusted on its fields.
	wrongOrigin := reentrySuccessor()
	wrongOrigin.Origin = domain.PublicationSuccessorExternalReview
	retargeted := reentrySuccessor()
	retargeted.Reentry.Reason = domain.ReadinessInvalidationRetargeted
	external := externalReviewSuccessor()
	externalMerge := domain.ProspectiveMergeIdentity{
		BaseSHA: external.Reentry.BaseSHA, HeadSHA: external.Reentry.HeadSHA, MergeSHA: "merge-1",
	}
	feedback := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: "run-1", CommandID: "return-1",
		FeedbackInvocationID: "inv-feedback-1", PredecessorItemID: "production-ready-run-1",
		PriorReviewInvocationID: "review-1", ReviewRound: 2,
	}
	continuation := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorRemediation,
		RunID: "run-1", CommandID: "approve-1", ReevaluationCommandID: "rerun-1",
		PredecessorItemID: "production-ready-run-1", PriorReviewInvocationID: "review-2", ReviewRound: 3,
	}
	for name, test := range map[string]struct {
		authority domain.PublicationSuccessor
		merge     domain.ProspectiveMergeIdentity
	}{
		"head_changed re-entry":  {headChanged, merge},
		"external_review":        {external, externalMerge},
		"feedback successor":     {feedback, merge},
		"remediation successor":  {continuation, merge},
		"retargeted re-entry":    {retargeted, merge},
		"mislabelled origin":     {wrongOrigin, merge},
		"zero authority":         {domain.PublicationSuccessor{}, merge},
		"zero authority, v3 tag": {domain.PublicationSuccessor{Version: domain.PublicationReentryVersion}, merge},
	} {
		if test.authority.AllowsProspectiveMerge(test.merge) {
			t.Fatalf("%s admitted a prospective merge", name)
		}
	}
}
