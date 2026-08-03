package exec_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestGolden covers the serialized shape of the two committed result
// contracts (the shapes store and api will carry); interfaces and fakes have
// no serialized form. Each fixture is a fixed, valid value, so the goldens
// double as validation-positive cases (the domain golden convention).
// Regenerate with: go test ./internal/exec -run TestGolden -update.
func TestGolden(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	stage := exec.StageResult{
		InvocationID: "inv-1",
		Status:       exec.StatusCompleted,
		HeadSHA:      "cafebabe",
		Artifacts:    []domain.Digest{"sha256:transcript", "sha256:diff"},
		Summary:      "implemented the fix and its regression test",
	}
	review := exec.ReviewResult{
		InvocationID:        "inv-2",
		BaseSHA:             "beefcafe",
		HeadSHA:             "cafebabe",
		Provider:            "openai",
		ModelConfiguration:  "gpt-5.2-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner",
		CompletedAt:         ts,
		CompletionEvidence:  domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Findings: []domain.Finding{{
			ID:        "finding-1",
			RunID:     "run-1",
			Source:    "codex",
			Severity:  "medium",
			Location:  "daemon/internal/exec/driver.go:12",
			Message:   "possible off-by-one in retry ordinal",
			RawText:   "P2: possible off-by-one in retry ordinal",
			CreatedAt: ts,
		}},
	}

	// The widened start spec, rendered from the record that authorizes it:
	// pinning the spec's shape this way also pins the mapping, so a field
	// added to the record and forgotten here shows up as a golden diff.
	identity := domain.AuthIdentityID("auth-claude-owner")
	conversationDigest := execStageDigest("7")
	vendorDigest := execStageDigest("8")
	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         execStageDigest("1"),
		SpecificationDigest: execStageDigest("2"),
		PromptPackageDigest: execStageDigest("3"),
		PolicyDigest:        execStageDigest("4"),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{execStageDigest("5")},
		ImageInputDigests:    []domain.Digest{execStageDigest("6")},
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace, domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     execStageDigest("2"), PolicyDigest: execStageDigest("4"), InputDigest: execStageDigest("1"),
		StageInputs:    &stageInputs,
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "freeside-handoff-run-1-ws",
		AuthIdentityID: &identity,
		AdmittedAt:     ts,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		value any
	}{
		{"stage_result", stage},
		{"review_result", review},
		{"start_spec", exec.StartSpecFromAdmission(admission)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// StartSpec has no Validate (see driver.go): it is valid because
			// the admission it was rendered from is.
			if v, ok := tc.value.(interface{ Validate() error }); ok {
				if err := v.Validate(); err != nil {
					t.Fatalf("fixture must be valid: %v", err)
				}
			}
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
