package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	legacyRun        = domain.RunID("run-elaboration-abc")
	legacyStage      = domain.StageID("elaborate-run-elaboration-abc")
	legacyInvocation = domain.InvocationID("inv-elaborate-run-elaboration-abc-3")
)

func TestSpecificationIdentityFollowsTheRunFamily(t *testing.T) {
	t.Parallel()
	if !domain.LegacySpecificationRun(legacyRun) || domain.LegacySpecificationRun("run-specification-abc") ||
		domain.LegacySpecificationRun("specification-run") {
		t.Fatal("family detection disagrees with the run prefix")
	}
	if got := domain.SpecificationStageID(legacyRun); got != legacyStage {
		t.Fatalf("legacy stage id = %q", got)
	}
	if got := domain.SpecificationStageID("specification-run"); got != "specify-specification-run" {
		t.Fatalf("current stage id = %q", got)
	}
	if got := domain.SpecificationInvocationID(legacyRun, 3); got != legacyInvocation {
		t.Fatalf("legacy invocation id = %q", got)
	}
	if got := domain.SpecificationInvocationID("specification-run", 1); got != "inv-specify-specification-run-1" {
		t.Fatalf("current invocation id = %q", got)
	}
	for _, id := range []domain.InvocationID{legacyInvocation, "inv-specify-specification-run-1"} {
		runID, ok := domain.SpecificationRunIDFromInvocationID(id)
		if !ok || domain.SpecificationInvocationID(runID, 3) != id && domain.SpecificationInvocationID(runID, 1) != id {
			t.Fatalf("parse %q = %q, %t", id, runID, ok)
		}
	}
	for _, id := range []domain.InvocationID{
		"inv-specify-run-elaboration-abc-1", // a legacy run under the current prefix
		"inv-elaborate-specification-run-1", // a current run under the legacy prefix
		"inv-elaborate-run-elaboration-abc-01",
		"inv-elaborate-run-elaboration-abc-",
		"inv-elaborate-run-elaboration-abc-99999999999999999999",
		"inv-implement-run-1",
	} {
		if runID, ok := domain.SpecificationRunIDFromInvocationID(id); ok {
			t.Fatalf("parse %q accepted as run %q", id, runID)
		}
	}
	implementation := domain.RunID("implementation-run")
	current := domain.SpecificationRunIDForImplementation(implementation)
	legacy := domain.LegacySpecificationRunIDForImplementation(implementation)
	if current == legacy || domain.LegacySpecificationRun(current) || !domain.LegacySpecificationRun(legacy) {
		t.Fatalf("derivations = %q, %q", current, legacy)
	}
	if !domain.SpecificationRunIDMatchesImplementation(current, implementation) ||
		!domain.SpecificationRunIDMatchesImplementation(legacy, implementation) ||
		domain.SpecificationRunIDMatchesImplementation(legacyRun, implementation) {
		t.Fatal("derivation match disagrees with the two families")
	}
	for _, key := range []string{"specification-discussion-cmd-1", "elaboration-discussion-cmd-1"} {
		if commandID, ok := domain.SpecificationDiscussionCommandID(key); !ok || commandID != "cmd-1" {
			t.Fatalf("discussion command from %q = %q, %t", key, commandID, ok)
		}
	}
	if _, ok := domain.SpecificationDiscussionCommandID("inv-cmd-1"); ok {
		t.Fatal("client discuss invocation parsed as a discussion identity")
	}
}

func TestLegacyStageNameCanonicalizesOnDecode(t *testing.T) {
	t.Parallel()
	var facts struct {
		Stage domain.StageName `json:"stage"`
	}
	if err := json.Unmarshal([]byte(`{"stage":"elaboration"}`), &facts); err != nil {
		t.Fatal(err)
	}
	if facts.Stage != domain.StageNameSpecification {
		t.Fatalf("decoded stage = %q", facts.Stage)
	}
	if err := json.Unmarshal([]byte(`{"stage":"implementation"}`), &facts); err != nil || facts.Stage != domain.StageNameImplementation {
		t.Fatalf("current stage = %q, %v", facts.Stage, err)
	}
	run := domain.Run{Stages: []domain.Stage{{Name: "elaboration"}, {Name: "implement"}}}
	run.CanonicalizeStoredRow()
	if run.Stages[0].Name != string(domain.StageNameSpecification) || run.Stages[1].Name != "implement" {
		t.Fatalf("canonicalized stages = %+v", run.Stages)
	}
	if role, err := domain.CanonicalStageRole("elaboration"); err != nil || role != domain.StageNameSpecification {
		t.Fatalf("legacy role = %q, %v", role, err)
	}
	if err := domain.ValidateLineupPolicyKeys([]domain.PolicyKey{
		{Key: domain.LineupRoleKeyPrefix + "elaboration", Value: "agent@sha256:" + string(make([]byte, 0))},
	}); errors.Is(err, domain.ErrInvalidStageName) {
		t.Fatalf("legacy lineup role rejected as a stage name: %v", err)
	}
}

func TestLegacySpecificationSourceDecodes(t *testing.T) {
	t.Parallel()
	var source domain.SpecificationSource
	if err := json.Unmarshal([]byte(`{"kind":"spec_artifact","spec_artifact_id":"art-1","issue_subject":null}`), &source); err != nil {
		t.Fatal(err)
	}
	if source.Kind != domain.SpecificationSourceWorkItemArtifact || source.WorkItemArtifactID != "art-1" {
		t.Fatalf("decoded legacy source = %+v", source)
	}
	if err := source.Validate(); err != nil {
		t.Fatalf("legacy source invalid after canonicalization: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"kind":"work_item_artifact","work_item_artifact_id":"art-1","spec_artifact_id":"art-2","issue_subject":null}`), &source); !errors.Is(err, domain.ErrSpecificationSourceInconsistent) {
		t.Fatalf("both spellings with different values = %v", err)
	}
	encoded, err := json.Marshal(domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "art-1"})
	if err != nil || string(encoded) != `{"kind":"work_item_artifact","work_item_artifact_id":"art-1","issue_subject":null}` {
		t.Fatalf("encoded source = %s, %v", encoded, err)
	}
}
