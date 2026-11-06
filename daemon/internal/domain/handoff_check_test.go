package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// movedHandoffCheck is a proved handoff at a tip that moved past the
// evidence base.
func movedHandoffCheck() domain.HandoffCheck {
	return domain.HandoffCheck{
		RunID: "run-1", PublicationID: "publish-run-1", HeadSHA: "head-1",
		EvidenceBaseSHA: "base-1", TipSHA: "base-2", WorkflowAuditID: 7,
		Merge: &domain.HandoffMerge{
			MergeSHA: "merge-1", VerificationProofDigest: "sha256:proof",
		},
	}
}

func TestHandoffCheckGolden(t *testing.T) {
	for name, check := range map[string]domain.HandoffCheck{
		"handoff_check":           movedHandoffCheck(),
		"handoff_check_unchanged": {RunID: "run-1", PublicationID: "publish-run-1", HeadSHA: "head-1", EvidenceBaseSHA: "base-1", TipSHA: "base-1", WorkflowAuditID: 7},
	} {
		if err := check.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, err := json.MarshalIndent(check, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden.Assert(t, name, append(body, '\n'))
	}
}

func TestHandoffCheckValidation(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		mutate  func(*domain.HandoffCheck)
		wantErr error
	}{
		"no run":                {func(c *domain.HandoffCheck) { c.RunID = "" }, domain.ErrEmptyField},
		"no publication":        {func(c *domain.HandoffCheck) { c.PublicationID = "" }, domain.ErrEmptyField},
		"no head":               {func(c *domain.HandoffCheck) { c.HeadSHA = "" }, domain.ErrEmptyField},
		"no evidence base":      {func(c *domain.HandoffCheck) { c.EvidenceBaseSHA = "" }, domain.ErrEmptyField},
		"no tip":                {func(c *domain.HandoffCheck) { c.TipSHA = "" }, domain.ErrEmptyField},
		"no merge commit":       {func(c *domain.HandoffCheck) { c.Merge.MergeSHA = "" }, domain.ErrEmptyField},
		"no merge proof":        {func(c *domain.HandoffCheck) { c.Merge.VerificationProofDigest = "" }, domain.ErrEmptyField},
		"merge at unmoved tip":  {func(c *domain.HandoffCheck) { c.TipSHA = c.EvidenceBaseSHA }, domain.ErrHandoffCheckInconsistent},
		"merge is the head":     {func(c *domain.HandoffCheck) { c.Merge.MergeSHA = c.HeadSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"merge is the tip":      {func(c *domain.HandoffCheck) { c.Merge.MergeSHA = c.TipSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
		"head is the moved tip": {func(c *domain.HandoffCheck) { c.HeadSHA = c.TipSHA }, domain.ErrProspectiveMergeIdentityInconsistent},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			check := movedHandoffCheck()
			test.mutate(&check)
			if err := check.Validate(); !errors.Is(err, test.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

// TestHandoffCheckProved pins when the record alone satisfies the handoff:
// an unchanged tip needs nothing, and a moved tip needs a recorded merge.
func TestHandoffCheckProved(t *testing.T) {
	t.Parallel()
	moved := movedHandoffCheck()
	if !moved.Moved() || !moved.Proved() {
		t.Fatalf("moved check with a merge: Moved %v, Proved %v", moved.Moved(), moved.Proved())
	}
	moved.Merge = nil
	if err := moved.Validate(); err != nil {
		t.Fatalf("a sealed tip awaiting its merge is a valid record: %v", err)
	}
	if moved.Proved() {
		t.Fatal("moved check without a merge counted as proved")
	}
	unchanged := movedHandoffCheck()
	unchanged.TipSHA, unchanged.Merge = unchanged.EvidenceBaseSHA, nil
	if unchanged.Moved() || !unchanged.Proved() {
		t.Fatalf("unchanged check: Moved %v, Proved %v", unchanged.Moved(), unchanged.Proved())
	}
}
