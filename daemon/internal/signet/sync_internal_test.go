package signet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestNormalizeAttentionItemCarriesReadinessAndLegacyNull(t *testing.T) {
	for name, readiness := range map[string]*domain.ReadinessSummary{
		"degraded": {
			Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation",
		},
		"legacy": nil,
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(normalizeAttentionItem(domain.AttentionItem{Readiness: readiness}))
			if err != nil {
				t.Fatal(err)
			}
			want := `"readiness":null`
			if readiness != nil {
				want = `"readiness":{"class":"ready_degraded","evaluation_set_digest":"sha256:evaluation"}`
			}
			if !strings.Contains(string(body), want) {
				t.Fatalf("sync item = %s, want %s", body, want)
			}
		})
	}
}

func TestNormalizeAttentionItemCarriesReadinessDetailAndLegacyNull(t *testing.T) {
	recipe := domain.Digest("sha256:recipe")
	for name, detail := range map[string]*domain.ReadinessDetail{
		"present": {
			EvaluationSetDigest: "sha256:evaluation", CandidateHead: "head",
			Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "base"},
			Requirements: []domain.ReadinessRequirement{{
				RequirementKey: "clean-verification", CheckClass: domain.CheckClassCleanVerification,
				Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
				ProofRecipeDigest: &recipe,
			}},
		},
		"legacy": nil,
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(normalizeAttentionItem(domain.AttentionItem{ReadinessDetail: detail}))
			if err != nil {
				t.Fatal(err)
			}
			want := `"readiness_detail":null`
			if detail != nil {
				want = `"readiness_detail":{"evaluation_set_digest":"sha256:evaluation","candidate_head":"head",` +
					`"base":{"base_ref":"main","base_sha":"base"},"requirements":[{"requirement_key":"clean-verification",` +
					`"check_class":"clean_verification","kind":"required","state":"passed",` +
					`"proof_recipe_digest":"sha256:recipe","waiver":null}]}`
			}
			if !strings.Contains(string(body), want) {
				t.Fatalf("sync item = %s, want %s", body, want)
			}
		})
	}
}

func TestAuthenticateSpecificationDiscussionArtifact(t *testing.T) {
	id := domain.ArtifactID("spec-discussion-command-1")
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	producer := domain.InvocationID("inv-specify-run-1")
	valid := domain.Artifact{
		ID: id, Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: producer,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1,
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Source:    domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}
	if !authenticatesSpecificationDiscussionArtifact(valid, id, digest, producer) {
		t.Fatal("valid discussion artifact was rejected")
	}
	for name, mutate := range map[string]func(*domain.Artifact){
		"retargeted id": func(artifact *domain.Artifact) { artifact.ID = "spec-discussion-other" },
		"wrong digest":  func(artifact *domain.Artifact) { artifact.Digest = "sha256:" + domain.Digest(strings.Repeat("b", 64)) },
		"wrong kind":    func(artifact *domain.Artifact) { artifact.Type = domain.ArtifactKindSpecification },
		"wrong producer": func(artifact *domain.Artifact) {
			artifact.Provenance.ProducerInvocationID = "inv-specify-other"
		},
		"agent producer": func(artifact *domain.Artifact) {
			artifact.Provenance.ProducerClass = domain.ProducerAgent
		},
		"head bound": func(artifact *domain.Artifact) {
			artifact.Provenance.HeadBinding = domain.HeadBound
			artifact.Provenance.SourceHeadSHA = "cafebabe"
		},
		"sensitive": func(artifact *domain.Artifact) {
			artifact.Provenance.SensitivityClass = domain.SensitivitySensitive
		},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := valid
			mutate(&artifact)
			if authenticatesSpecificationDiscussionArtifact(artifact, id, digest, producer) {
				t.Fatalf("corrupt discussion artifact authenticated: %+v", artifact)
			}
		})
	}
}

func TestNormalizeAttentionItemCarriesYieldHistoryWithoutMutation(t *testing.T) {
	history := &domain.ReviewYieldHistory{
		Rounds:          []domain.ReviewYieldRound{{Round: 1, Outcome: domain.ReviewClean}},
		TerminalOutcome: domain.ReviewClean,
	}
	item := domain.AttentionItem{YieldHistory: history}

	normalized := normalizeAttentionItem(item)
	if normalized.YieldHistory == nil || len(normalized.YieldHistory.Rounds) != 1 {
		t.Fatalf("normalized yield history = %+v", normalized.YieldHistory)
	}
	normalized.YieldHistory.Rounds[0].Round = 2
	if item.YieldHistory.Rounds[0].Round != 1 {
		t.Fatalf("input yield history was mutated: %+v", item.YieldHistory)
	}
	legacy, err := json.Marshal(normalizeAttentionItem(domain.AttentionItem{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), `"yield_history":null`) {
		t.Fatalf("legacy sync item = %s, want yield_history null", legacy)
	}
}

func TestPublicationAuthorityExclusivityRejectsReadyAndBlocked(t *testing.T) {
	err := validatePublicationAuthorityExclusivity("run-1", true, true, false, true)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("validatePublicationAuthorityExclusivity() = %v, want ErrParentKeyMismatch", err)
	}
	if err := validatePublicationAuthorityExclusivity("run-1", true, true, true, true); err != nil {
		t.Fatalf("resolved rerun authority = %v, want nil", err)
	}
	if err := validatePublicationAuthorityExclusivity("run-1", true, true, true, false); err == nil {
		t.Fatal("ready before the last publication block passed authentication")
	}
}

func TestAuthoritativeStatusOverridesLaggingObservation(t *testing.T) {
	invocation := domain.InvocationID("invocation-1")
	observedAt := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	observation := domain.RunObservation{
		RunID: "run-1",
		Milestones: []domain.RunMilestone{{
			RunID: "run-1", Kind: domain.MilestoneExecutionExportRecorded,
			InvocationID: &invocation, RecordedAt: observedAt.Add(-time.Minute),
		}},
		Invocations: []domain.InvocationObservation{{
			InvocationID: invocation, RunID: "run-1", Status: domain.ObservedStatusRunning,
			Live: true, ObservedAt: observedAt,
		}},
	}

	projected := withAuthoritativeInvocationStatuses(observation)
	if projected.Invocations[0].Status != domain.ObservedStatusCompleted || projected.Invocations[0].Live {
		t.Fatalf("projected invocation = %+v, want completed and not live", projected.Invocations[0])
	}
	if projected.Invocations[0].ObservedAt != observedAt {
		t.Fatalf("projected observed_at = %v, want %v", projected.Invocations[0].ObservedAt, observedAt)
	}
	if observation.Invocations[0].Status != domain.ObservedStatusRunning || !observation.Invocations[0].Live {
		t.Fatalf("input observation was mutated: %+v", observation.Invocations[0])
	}
}

func TestNormalizeAttentionItemNormalizesNestedAdjudicationArraysWithoutMutation(t *testing.T) {
	item := domain.AttentionItem{
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			Proposals: []domain.FindingAdjudicationProposal{{}},
		},
	}

	normalized := normalizeAttentionItem(item)
	proposal := normalized.FindingAdjudication.Proposals[0]
	if proposal.Evidence == nil || proposal.CitedRules == nil || proposal.Assumptions == nil ||
		proposal.OpenQuestions == nil || proposal.OfferedAlternatives == nil {
		t.Fatalf("normalized proposal retains nil arrays: %+v", proposal)
	}
	original := item.FindingAdjudication.Proposals[0]
	if original.Evidence != nil || original.CitedRules != nil || original.Assumptions != nil ||
		original.OpenQuestions != nil || original.OfferedAlternatives != nil {
		t.Fatalf("input proposal mutated: %+v", original)
	}
}
