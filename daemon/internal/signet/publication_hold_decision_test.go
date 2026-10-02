package signet

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// A hold is told apart from a definitive block by its exact action list, so
// the list the engine writes for a hold that carries a pull request has to be
// recognized here and nowhere wider: any other list must keep reading as a
// different item.
func TestPublicationHoldDecision(t *testing.T) {
	reference := &domain.PRReference{Repo: "owner/repo", Number: 450}
	for _, tc := range []struct {
		name      string
		actions   []domain.Action
		reference *domain.PRReference
		want      bool
	}{
		{"inspect only", []domain.Action{domain.ActionInspectTrustFailure}, nil, true},
		{"inspect only with a reference", []domain.Action{domain.ActionInspectTrustFailure}, reference, true},
		{
			"inspect and open with a reference",
			[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionOpenPR},
			reference, true,
		},
		{
			"inspect and open without a reference",
			[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionOpenPR},
			nil, false,
		},
		{
			"open before inspect",
			[]domain.Action{domain.ActionOpenPR, domain.ActionInspectTrustFailure},
			reference, false,
		},
		{
			"definitive block",
			[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
			reference, false,
		},
		{
			"definitive block with open",
			[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionOpenPR, domain.ActionStop},
			reference, false,
		},
		{"open only", []domain.Action{domain.ActionOpenPR}, reference, false},
		{"no actions", nil, reference, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := domain.AttentionItem{RequestedDecision: tc.actions, PRReference: tc.reference}
			if got := PublicationHoldDecision(item); got != tc.want {
				t.Fatalf("PublicationHoldDecision = %v, want %v", got, tc.want)
			}
		})
	}
}
