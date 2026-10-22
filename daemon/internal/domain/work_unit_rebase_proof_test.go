package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validRebaseProof() domain.WorkUnitRebaseProof {
	return domain.WorkUnitRebaseProof{
		UnitID: "workunit-run-1", RepositoryID: 84958515, PRNumber: 450,
		ReviewedBaseSHA: "facefeed", ReviewedHeadSHA: "cafebabe", ProvenHeadSHA: "c0ffee00",
		ReviewedTreeSHA: "feedf00d", Proven: true,
		RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestWorkUnitRebaseProofValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.WorkUnitRebaseProof)
		wantErr error
	}{
		{"no unit", func(p *domain.WorkUnitRebaseProof) { p.UnitID = "" }, domain.ErrEmptyID},
		{"non-positive repository id", func(p *domain.WorkUnitRebaseProof) { p.RepositoryID = 0 }, domain.ErrNonPositive},
		{"non-positive pr number", func(p *domain.WorkUnitRebaseProof) { p.PRNumber = 0 }, domain.ErrNonPositive},
		{"no reviewed base", func(p *domain.WorkUnitRebaseProof) { p.ReviewedBaseSHA = "" }, domain.ErrEmptyField},
		{"no reviewed head", func(p *domain.WorkUnitRebaseProof) { p.ReviewedHeadSHA = "" }, domain.ErrEmptyField},
		{"no proven head", func(p *domain.WorkUnitRebaseProof) { p.ProvenHeadSHA = "" }, domain.ErrEmptyField},
		{"proves the reviewed head itself", func(p *domain.WorkUnitRebaseProof) {
			p.ProvenHeadSHA = p.ReviewedHeadSHA
		}, domain.ErrRebaseProofInconsistent},
		{"proven without a tree", func(p *domain.WorkUnitRebaseProof) { p.ReviewedTreeSHA = "" }, domain.ErrRebaseProofInconsistent},
		{"proven with a failure", func(p *domain.WorkUnitRebaseProof) { p.Detail = "tree differs" }, domain.ErrRebaseProofInconsistent},
		{"refused without a failure", func(p *domain.WorkUnitRebaseProof) { p.Proven = false }, domain.ErrRebaseProofInconsistent},
		{"oversized detail", func(p *domain.WorkUnitRebaseProof) {
			p.Proven, p.Detail = false, strings.Repeat("x", domain.WorkUnitRebaseProofDetailLimit+1)
		}, domain.ErrRebaseProofInconsistent},
		{"detail not UTF-8", func(p *domain.WorkUnitRebaseProof) {
			p.Proven, p.Detail = false, "\xff"
		}, domain.ErrRebaseProofInconsistent},
		{"zero recorded_at", func(p *domain.WorkUnitRebaseProof) { p.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC recorded_at", func(p *domain.WorkUnitRebaseProof) {
			p.RecordedAt = p.RecordedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proof := validRebaseProof()
			tc.mutate(&proof)
			if err := proof.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("refusal before the merge was computed is valid", func(t *testing.T) {
		proof := validRebaseProof()
		proof.Proven, proof.ReviewedTreeSHA, proof.Detail = false, "", "the reviewed head is unavailable"
		if err := proof.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}
