package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func passedRequirement(key domain.RequirementKey, class domain.VerificationCheckClass) domain.ReadinessRequirement {
	recipe := domain.Digest("sha256:recipe-" + string(key))
	return domain.ReadinessRequirement{
		RequirementKey: key, CheckClass: class, Kind: domain.RequirementRequired,
		State: domain.ReadinessRequirementPassed, ProofRecipeDigest: &recipe,
	}
}

func waivedRequirement(key domain.RequirementKey) domain.ReadinessRequirement {
	return domain.ReadinessRequirement{
		RequirementKey: key, CheckClass: domain.CheckClassRepoChangePolicy,
		Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
		Waiver: &domain.ReadinessWaiver{
			ID: "waiver-1", Dimension: "repo_change_policy",
			Authority: domain.WaiverAuthorityHumanApproval,
			GrantedAt: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC),
		},
	}
}

func cleanDetail(digest domain.Digest, head string) domain.ReadinessDetail {
	return domain.ReadinessDetail{
		EvaluationSetDigest: digest, CandidateHead: head,
		Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "base"},
		Requirements: []domain.ReadinessRequirement{
			passedRequirement("clean-verification", domain.CheckClassCleanVerification),
			passedRequirement("independent-review", domain.CheckClassIndependentReview),
		},
	}
}

// readyDetailInput is a valid ready item whose summary and detail agree on
// the clean evaluation set and the published head.
func readyDetailInput() domain.AttentionItemInput {
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "head"
	in.Readiness = &domain.ReadinessSummary{
		Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:evaluation",
	}
	detail := cleanDetail("sha256:evaluation", in.PRHeadSHA)
	in.ReadinessDetail = &detail
	return in
}

func TestReadinessDetailClassFollowsTheEvaluationRule(t *testing.T) {
	t.Parallel()
	detail := cleanDetail("sha256:evaluation", "head")
	if err := detail.Validate(); err != nil {
		t.Fatal(err)
	}
	if detail.Class() != domain.ReadinessReadyClean {
		t.Fatalf("clean detail class = %q", detail.Class())
	}
	degraded := detail
	degraded.Requirements = append(degraded.Requirements, waivedRequirement("repo-change-policy"),
		domain.ReadinessRequirement{
			RequirementKey: "style", CheckClass: domain.CheckClassRepoChangePolicy,
			Kind: domain.RequirementOptional, State: domain.ReadinessRequirementNotRun,
		})
	if err := degraded.Validate(); err != nil {
		t.Fatal(err)
	}
	if degraded.Class() != domain.ReadinessReadyDegraded {
		t.Fatalf("degraded detail class = %q", degraded.Class())
	}
	blocked := detail
	blocked.Requirements = append(blocked.Requirements, domain.ReadinessRequirement{
		RequirementKey: "repo-change-policy", CheckClass: domain.CheckClassRepoChangePolicy,
		Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
	})
	if err := blocked.Validate(); err != nil {
		t.Fatal(err)
	}
	if blocked.Class() != domain.ReadinessBlocked {
		t.Fatalf("blocked detail class = %q", blocked.Class())
	}
}

func TestReadinessDetailValidateRejectsInconsistentEntries(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*domain.ReadinessDetail)
		want   error
	}{
		"empty head": {func(d *domain.ReadinessDetail) { d.CandidateHead = "" }, domain.ErrEmptyField},
		"empty base": {func(d *domain.ReadinessDetail) { d.Base.BaseSHA = "" }, domain.ErrEmptyField},
		"no requirements": {
			func(d *domain.ReadinessDetail) { d.Requirements = nil }, domain.ErrRequirementSetEmpty,
		},
		"out of key order": {
			func(d *domain.ReadinessDetail) {
				d.Requirements[0], d.Requirements[1] = d.Requirements[1], d.Requirements[0]
			}, domain.ErrReadinessDetailInconsistent,
		},
		"duplicate key": {
			func(d *domain.ReadinessDetail) { d.Requirements[1].RequirementKey = d.Requirements[0].RequirementKey },
			domain.ErrReadinessDetailInconsistent,
		},
		"proof on a failure": {
			func(d *domain.ReadinessDetail) { d.Requirements[0].State = domain.ReadinessRequirementFailed },
			domain.ErrReadinessDetailInconsistent,
		},
		"pass without a proof": {
			func(d *domain.ReadinessDetail) { d.Requirements[0].ProofRecipeDigest = nil },
			domain.ErrReadinessDetailInconsistent,
		},
		"waiver on a pass": {
			func(d *domain.ReadinessDetail) { d.Requirements[0].Waiver = waivedRequirement("x").Waiver },
			domain.ErrReadinessDetailInconsistent,
		},
		"waiver on an ineligible class": {
			func(d *domain.ReadinessDetail) {
				d.Requirements[0].State = domain.ReadinessRequirementFailed
				d.Requirements[0].ProofRecipeDigest = nil
				d.Requirements[0].Waiver = waivedRequirement("x").Waiver
			}, domain.ErrReadinessDetailInconsistent,
		},
		"waiver on an optional requirement": {
			func(d *domain.ReadinessDetail) {
				waived := waivedRequirement("repo-change-policy")
				waived.Kind = domain.RequirementOptional
				d.Requirements = append(d.Requirements, waived)
			}, domain.ErrReadinessDetailInconsistent,
		},
		"waiver without an authority": {
			func(d *domain.ReadinessDetail) {
				waived := waivedRequirement("repo-change-policy")
				waived.Waiver.Authority = ""
				d.Requirements = append(d.Requirements, waived)
			}, domain.ErrInvalidWaiverGrantingAuthority,
		},
		"waiver granted in local time": {
			func(d *domain.ReadinessDetail) {
				waived := waivedRequirement("repo-change-policy")
				waived.Waiver.GrantedAt = waived.Waiver.GrantedAt.In(time.FixedZone("x", 3600))
				d.Requirements = append(d.Requirements, waived)
			}, domain.ErrTimestampNotUTC,
		},
		"unknown state": {
			func(d *domain.ReadinessDetail) {
				d.Requirements[0].State = "skipped"
				d.Requirements[0].ProofRecipeDigest = nil
			}, domain.ErrInvalidReadinessRequirementState,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detail := cleanDetail("sha256:evaluation", "head")
			tc.mutate(&detail)
			if err := detail.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadyItemBindsReadinessDetailToItsSummary(t *testing.T) {
	t.Parallel()
	in := readyDetailInput()
	detail := in.ReadinessDetail
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if item.ReadinessDetail == nil || item.ReadinessDetail.Class() != domain.ReadinessReadyClean {
		t.Fatalf("ready item detail = %+v", item.ReadinessDetail)
	}
	// The constructor clones the detail so the caller's slice cannot reach it.
	detail.Requirements[0].State = domain.ReadinessRequirementFailed
	if err := item.Validate(); err != nil {
		t.Fatalf("item shares its caller's detail: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate func(*domain.AttentionItemInput)
		want   error
	}{
		"blocked detail": {
			func(in *domain.AttentionItemInput) {
				blocked := cleanDetail("sha256:evaluation", in.PRHeadSHA)
				blocked.Requirements = append(blocked.Requirements, domain.ReadinessRequirement{
					RequirementKey: "repo-change-policy", CheckClass: domain.CheckClassRepoChangePolicy,
					Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
				})
				in.ReadinessDetail = &blocked
			}, domain.ErrReadinessDetailInconsistent,
		},
		"class mismatch": {
			func(in *domain.AttentionItemInput) {
				degraded := cleanDetail("sha256:evaluation", in.PRHeadSHA)
				degraded.Requirements = append(degraded.Requirements, waivedRequirement("repo-change-policy"))
				in.ReadinessDetail = &degraded
			}, domain.ErrReadinessDetailInconsistent,
		},
		"digest mismatch": {
			func(in *domain.AttentionItemInput) {
				other := cleanDetail("sha256:other", in.PRHeadSHA)
				in.ReadinessDetail = &other
			}, domain.ErrReadinessDetailInconsistent,
		},
		"head mismatch": {
			func(in *domain.AttentionItemInput) {
				other := cleanDetail("sha256:evaluation", "other-head")
				in.ReadinessDetail = &other
			}, domain.ErrReadinessDetailInconsistent,
		},
		"detail without a summary": {
			func(in *domain.AttentionItemInput) { in.Readiness = nil }, domain.ErrReadinessDetailInconsistent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := readyDetailInput()
			tc.mutate(&in)
			if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, tc.want) {
				t.Fatalf("NewAttentionItem() error = %v, want %v", err, tc.want)
			}
		})
	}

	foreign := validItemInput(domain.AttentionBlocked)
	foreign.ReadinessDetail = detail
	if _, err := domain.NewAttentionItem(foreign, nil); !errors.Is(err, domain.ErrReadinessDetailInconsistent) {
		t.Fatalf("detail on a blocked item error = %v, want ErrReadinessDetailInconsistent", err)
	}
}

func TestValidateAttentionItemReadinessDetailImmutable(t *testing.T) {
	t.Parallel()
	in := readyDetailInput()
	stored := mustItem(t, in)

	for name, mutate := range map[string]func(*domain.AttentionItem){
		"remove": func(item *domain.AttentionItem) { item.ReadinessDetail = nil },
		"replace": func(item *domain.AttentionItem) {
			replaced := cleanDetail("sha256:evaluation", item.PRHeadSHA)
			replaced.Base.BaseSHA = "other-base"
			item.ReadinessDetail = &replaced
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			updated := stored
			updated.ItemVersion++
			mutate(&updated)
			if err := domain.ValidateAttentionItemTransition(stored, updated); !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("readiness detail %s error = %v, want ErrImmutableTransition", name, err)
			}
		})
	}

	legacyInput := in
	legacyInput.ReadinessDetail = nil
	legacy := mustItem(t, legacyInput)
	backfilled := legacy
	backfilled.ItemVersion++
	backfilled.ReadinessDetail = in.ReadinessDetail
	if err := domain.ValidateAttentionItemTransition(legacy, backfilled); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("backfilling legacy readiness detail = %v, want ErrImmutableTransition", err)
	}
}
