package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func readinessResolution(t *testing.T, key domain.RequirementKey, class domain.VerificationCheckClass, kind domain.RequirementKind, baseDependent bool) domain.RequirementResolution {
	t.Helper()
	r, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: key, CheckClass: class, Kind: kind, Applicable: true,
		BaseDependent: baseDependent, RequirementSetDigest: "sha256:requirement-set",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readinessBase(sha string) *domain.BaseRevision {
	return &domain.BaseRevision{Repo: "freeside-ai/freeside", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: sha}
}

func readinessTarget(base *domain.BaseRevision) domain.EvaluationTarget {
	return domain.EvaluationTarget{CandidateHead: "head", Base: base}
}

func TestReadinessEnumsRegistered(t *testing.T) {
	t.Parallel()
	if len(domain.AllRequirementKinds) != 2 || len(domain.AllAdvisoryOutcomes) != 2 ||
		len(domain.AllReadinessVerdictClasses) != 3 || len(domain.AllWaiverGrantingAuthorities) != 2 ||
		len(domain.AllVerificationCheckClasses) != 3 || len(domain.AllWaiverLifecycleStatuses) != 3 ||
		len(domain.AllReadinessRequirementStates) != 4 {
		t.Fatal("readiness enum registry lost a member")
	}
}

func TestEvaluateReadinessBlocksOnAbsentRecord(t *testing.T) {
	t.Parallel()
	r := readinessResolution(t, "verify", domain.CheckClassCleanVerification, domain.RequirementOptional, true)
	v, err := domain.EvaluateReadiness(readinessTarget(nil), []domain.RequirementResolution{r}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Class != domain.ReadinessBlocked || len(v.Reasons) != 1 || !v.Reasons[0].AbsentRecord || v.Reasons[0].Outcome != domain.AdvisoryNotRun {
		t.Fatalf("verdict = %+v, want absent required-not-run block", v)
	}
}

func TestEvaluateReadinessRejectsMixedRequirementSetBindings(t *testing.T) {
	t.Parallel()
	first := readinessResolution(t, "first", domain.CheckClassCleanVerification, domain.RequirementRequired, false)
	second := readinessResolution(t, "second", domain.CheckClassIndependentReview, domain.RequirementRequired, false)
	var err error
	second, err = domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: second.RequirementKey, CheckClass: second.CheckClass, Kind: second.Kind,
		Applicable: second.Applicable, BaseDependent: second.BaseDependent,
		RequirementSetDigest:    "sha256:other-set",
		FloorRegistryGeneration: second.FloorRegistryGeneration,
		ResolvedPolicyDigest:    second.ResolvedPolicyDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	proofA, err := domain.NewCheckProof(first, "head", nil, "sha256:first")
	if err != nil {
		t.Fatal(err)
	}
	proofB, err := domain.NewCheckProof(second, "head", nil, "sha256:second")
	if err != nil {
		t.Fatal(err)
	}
	stateA, err := domain.NewPassedCheckState(first, proofA)
	if err != nil {
		t.Fatal(err)
	}
	stateB, err := domain.NewPassedCheckState(second, proofB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.EvaluateReadiness(readinessTarget(nil), []domain.RequirementResolution{first, second}, []domain.CheckState{stateA, stateB}, nil); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("EvaluateReadiness() error = %v, want requirement-set mismatch", err)
	}
}

func TestEvaluateReadinessPreservesCleanAndDegraded(t *testing.T) {
	t.Parallel()
	required := readinessResolution(t, "verification", domain.CheckClassCleanVerification, domain.RequirementRequired, true)
	optional := readinessResolution(t, "optional", domain.CheckClassRepoChangePolicy, domain.RequirementOptional, false)
	proof, err := domain.NewCheckProof(required, "head", readinessBase("base"), "sha256:recipe")
	if err != nil {
		t.Fatal(err)
	}
	passed, err := domain.NewPassedCheckState(required, proof)
	if err != nil {
		t.Fatal(err)
	}
	optionalProof, err := domain.NewCheckProof(optional, "head", nil, "sha256:optional")
	if err != nil {
		t.Fatal(err)
	}
	optionalPassed, err := domain.NewPassedCheckState(optional, optionalProof)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := domain.EvaluateReadiness(readinessTarget(readinessBase("base")), []domain.RequirementResolution{optional, required}, []domain.CheckState{passed, optionalPassed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Class != domain.ReadinessReadyClean || clean.EvaluationSetDigest == "" {
		t.Fatalf("clean = %+v", clean)
	}
	optionalFailed, err := domain.NewNonPassingCheckState(optional, domain.AdvisoryFailed, nil)
	if err != nil {
		t.Fatal(err)
	}
	degraded, err := domain.EvaluateReadiness(readinessTarget(readinessBase("base")), []domain.RequirementResolution{required, optional}, []domain.CheckState{optionalFailed, passed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if degraded.Class != domain.ReadinessReadyDegraded || len(degraded.AdvisoryOutcomes) != 1 || degraded.EvaluationSetDigest == clean.EvaluationSetDigest {
		t.Fatalf("degraded = %+v, clean = %+v", degraded, clean)
	}

	// The card-facing projection reproduces each verdict's class and carries
	// its digest, head, and base without re-evaluating anything.
	cleanDetail, err := domain.NewReadinessDetail(readinessTarget(readinessBase("base")), clean, []domain.CheckState{passed, optionalPassed})
	if err != nil {
		t.Fatal(err)
	}
	if cleanDetail.Class() != clean.Class || cleanDetail.EvaluationSetDigest != clean.EvaluationSetDigest ||
		cleanDetail.CandidateHead != "head" || cleanDetail.Base != (domain.ReadinessBoundBase{BaseRef: "refs/heads/main", BaseSHA: "base"}) ||
		len(cleanDetail.Requirements) != 2 || cleanDetail.Requirements[0].RequirementKey != "optional" ||
		cleanDetail.Requirements[1].State != domain.ReadinessRequirementPassed ||
		cleanDetail.Requirements[1].ProofRecipeDigest == nil || *cleanDetail.Requirements[1].ProofRecipeDigest != "sha256:recipe" {
		t.Fatalf("clean detail = %+v", cleanDetail)
	}
	degradedDetail, err := domain.NewReadinessDetail(readinessTarget(readinessBase("base")), degraded, []domain.CheckState{optionalFailed, passed})
	if err != nil {
		t.Fatal(err)
	}
	if degradedDetail.Class() != degraded.Class || degradedDetail.EvaluationSetDigest != degraded.EvaluationSetDigest ||
		degradedDetail.Requirements[0].State != domain.ReadinessRequirementFailed || degradedDetail.Requirements[0].Waiver != nil {
		t.Fatalf("degraded detail = %+v", degradedDetail)
	}
	// Recorded states that imply another class than the verdict are refused,
	// so the projection cannot contradict the summary it explains.
	if _, err := domain.NewReadinessDetail(readinessTarget(readinessBase("base")), clean, []domain.CheckState{optionalFailed, passed}); !errors.Is(err, domain.ErrReadinessDetailInconsistent) {
		t.Fatalf("NewReadinessDetail() with a degrading state under a clean verdict error = %v, want ErrReadinessDetailInconsistent", err)
	}
	if _, err := domain.NewReadinessDetail(readinessTarget(nil), clean, []domain.CheckState{passed, optionalPassed}); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("NewReadinessDetail() without a base error = %v, want ErrEmptyField", err)
	}
}

func TestWaiverIsLimitedToEligibleRequiredFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	eligible := readinessResolution(t, "policy", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, false)
	lifecycle, err := domain.NewWaiverLifecycleEvent("waiver-1", 1, domain.WaiverLifecycleGranted, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	waiver, err := domain.NewValidatedDegradedWaiver(eligible, "waiver-1", "repo_change_policy", domain.WaiverAuthorityHumanApproval, "sha256:grant", lifecycle, now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := domain.NewNonPassingCheckState(eligible, domain.AdvisoryFailed, &waiver)
	if err != nil {
		t.Fatal(err)
	}
	v, err := domain.EvaluateReadiness(readinessTarget(nil), []domain.RequirementResolution{eligible}, []domain.CheckState{state}, func(r domain.RequirementResolution, w domain.ValidatedDegradedWaiver) error {
		return domain.ValidateDegradedWaiver(r, lifecycle, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Class != domain.ReadinessReadyDegraded || len(v.WaiverIDs) != 1 {
		t.Fatalf("verdict = %+v", v)
	}
	nonWaivable := readinessResolution(t, "review", domain.CheckClassIndependentReview, domain.RequirementRequired, true)
	if _, err := domain.NewValidatedDegradedWaiver(nonWaivable, "waiver-2", "review", domain.WaiverAuthorityHumanApproval, "sha256:grant", lifecycle, now); !errors.Is(err, domain.ErrWaiverInconsistent) {
		t.Fatalf("non-waivable error = %v", err)
	}
}

func TestEvaluateReadinessReGatesWaiverLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	resolution := readinessResolution(t, "policy", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, false)
	grant, err := domain.NewWaiverLifecycleEvent("waiver-1", 1, domain.WaiverLifecycleGranted, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	waiver, err := domain.NewValidatedDegradedWaiver(resolution, grant.WaiverID, "policy", domain.WaiverAuthorityHumanApproval, "sha256:grant", grant, now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := domain.NewNonPassingCheckState(resolution, domain.AdvisoryFailed, &waiver)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := domain.NewWaiverLifecycleEvent(waiver.ID, 2, domain.WaiverLifecycleRevoked, &grant.EventDigest, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = domain.EvaluateReadiness(readinessTarget(nil), []domain.RequirementResolution{resolution}, []domain.CheckState{state}, func(r domain.RequirementResolution, w domain.ValidatedDegradedWaiver) error {
		return domain.ValidateDegradedWaiver(r, revoked, w)
	})
	if !errors.Is(err, domain.ErrWaiverInconsistent) {
		t.Fatalf("revoked waiver evaluation error = %v, want current-gate rejection", err)
	}
}

func TestCheckStateJSONRoundTripPreservesHiddenWaiver(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	r := readinessResolution(t, "policy", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, false)
	lifecycle, _ := domain.NewWaiverLifecycleEvent("waiver-1", 1, domain.WaiverLifecycleGranted, nil, now)
	waiver, _ := domain.NewValidatedDegradedWaiver(r, "waiver-1", "policy", domain.WaiverAuthorityTrustedConfig, "sha256:grant", lifecycle, now)
	state, _ := domain.NewNonPassingCheckState(r, domain.AdvisoryNotRun, &waiver)
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.CheckState
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := decoded.Applicable.Failure.Waiver(); got == nil || got.ID != "waiver-1" {
		t.Fatalf("waiver = %+v", got)
	}
}

func TestBaseAdvanceChangesProofAndEvaluationIdentity(t *testing.T) {
	t.Parallel()
	r := readinessResolution(t, "review", domain.CheckClassIndependentReview, domain.RequirementRequired, true)
	proofA, _ := domain.NewCheckProof(r, "head", readinessBase("base-a"), "sha256:review")
	proofB, _ := domain.NewCheckProof(r, "head", readinessBase("base-b"), "sha256:review")
	if proofA.Digest == proofB.Digest {
		t.Fatal("base advance did not change proof digest")
	}
	stateA, _ := domain.NewPassedCheckState(r, proofA)
	stateB, _ := domain.NewPassedCheckState(r, proofB)
	verdictA, err := domain.EvaluateReadiness(readinessTarget(readinessBase("base-a")), []domain.RequirementResolution{r}, []domain.CheckState{stateA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	verdictB, err := domain.EvaluateReadiness(readinessTarget(readinessBase("base-b")), []domain.RequirementResolution{r}, []domain.CheckState{stateB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if verdictA.EvaluationSetDigest == verdictB.EvaluationSetDigest {
		t.Fatal("base advance did not change evaluation digest")
	}
}

// TestEvaluateReadinessRejectsUnboundTargets enumerates the target-binding
// axes: an empty requirement set, an unnamed target, and evidence whose head
// or base does not cover the evaluated candidate all fail closed instead of
// combining into readiness.
func TestEvaluateReadinessRejectsUnboundTargets(t *testing.T) {
	t.Parallel()
	r := readinessResolution(t, "verification", domain.CheckClassCleanVerification, domain.RequirementRequired, true)
	proofFor := func(head, baseSHA string) domain.CheckState {
		proof, err := domain.NewCheckProof(r, head, readinessBase(baseSHA), "sha256:recipe")
		if err != nil {
			t.Fatal(err)
		}
		state, err := domain.NewPassedCheckState(r, proof)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	for _, test := range []struct {
		name        string
		target      domain.EvaluationTarget
		resolutions []domain.RequirementResolution
		recorded    []domain.CheckState
		wantErr     error
	}{
		{
			name:    "empty requirement set",
			target:  readinessTarget(readinessBase("base")),
			wantErr: domain.ErrRequirementSetEmpty,
		},
		{
			name:        "unnamed target head",
			target:      domain.EvaluationTarget{Base: readinessBase("base")},
			resolutions: []domain.RequirementResolution{r},
			recorded:    []domain.CheckState{proofFor("head", "base")},
			wantErr:     domain.ErrEmptyField,
		},
		{
			name:        "foreign candidate head",
			target:      readinessTarget(readinessBase("base")),
			resolutions: []domain.RequirementResolution{r},
			recorded:    []domain.CheckState{proofFor("other-head", "base")},
			wantErr:     domain.ErrEvaluationTargetMismatch,
		},
		{
			name:        "foreign base",
			target:      readinessTarget(readinessBase("base")),
			resolutions: []domain.RequirementResolution{r},
			recorded:    []domain.CheckState{proofFor("head", "other-base")},
			wantErr:     domain.ErrEvaluationTargetMismatch,
		},
		{
			name:        "base-dependent proof without target base",
			target:      readinessTarget(nil),
			resolutions: []domain.RequirementResolution{r},
			recorded:    []domain.CheckState{proofFor("head", "base")},
			wantErr:     domain.ErrEvaluationTargetMismatch,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := domain.EvaluateReadiness(test.target, test.resolutions, test.recorded, nil)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("EvaluateReadiness() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestReadinessVerdictRejectsForgedPayloads(t *testing.T) {
	t.Parallel()
	for _, verdict := range []domain.ReadinessVerdict{
		{Class: domain.ReadinessBlocked, Reasons: []domain.ReadinessBlockReason{{Outcome: domain.AdvisoryNotRun}}},
		{Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation", WaiverIDs: []domain.WaiverID{"waiver", "waiver"}},
		{Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation", AdvisoryOutcomes: []domain.AdvisoryOutcomeRecord{{RequirementResolutionDigest: "sha256:resolution", Outcome: "invented"}}},
	} {
		if err := verdict.Validate(); !errors.Is(err, domain.ErrReadinessVerdictInconsistent) {
			t.Fatalf("forged verdict %+v error = %v", verdict, err)
		}
	}
}

func TestReadinessSummaryAcceptsOnlyReadyClassesWithDigest(t *testing.T) {
	for _, summary := range []domain.ReadinessSummary{
		{Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:clean"},
		{Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:degraded"},
	} {
		if err := summary.Validate(); err != nil {
			t.Fatalf("valid summary %+v rejected: %v", summary, err)
		}
	}
	for _, summary := range []domain.ReadinessSummary{
		{Class: domain.ReadinessBlocked, EvaluationSetDigest: "sha256:blocked"},
		{Class: domain.ReadinessReadyClean},
		{Class: "invented", EvaluationSetDigest: "sha256:invented"},
	} {
		if err := summary.Validate(); err == nil {
			t.Fatalf("invalid summary %+v accepted", summary)
		}
	}
}
