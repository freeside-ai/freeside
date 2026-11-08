package domaintest

import (
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// The builders below each return the value behind one golden in
// internal/domain/golden_test.go, and TestGolden takes those fixtures from
// them, so "valid" has one definition. An option edits the value, or for a
// constructed type the constructor's input, before validation. A builder
// panics when its options produce an invalid value, as MustConclude does: that
// is a broken fixture, not a case under test. A test that needs an invalid
// value changes the returned value instead.

// goldenTime is the instant the golden fixtures are stamped with.
var goldenTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// Run returns the golden run, a production retry with one implementation
// stage, after applying opts.
func Run(opts ...func(*domain.Run)) domain.Run {
	run := domain.Run{
		TaskID: "task-1", ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec",
		PolicyDigest: goldenPolicy().Digest,
		CampaignID:   "campaign-1", AttemptNumber: 2,
		AttemptReason: "Retry after repairing the acceptance rig", ParentRunID: "run-0",
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
		}},
	}
	for _, opt := range opts {
		opt(&run)
	}
	if err := run.Validate(); err != nil {
		panic(fmt.Sprintf("domaintest.Run(%s): %v", run.ID, err))
	}
	return run
}

// ReviewRecord returns the golden review record, a round-one review with one
// finding attributed by the seeded reviewer's admission snapshot. opts edit
// the input to domain.NewReviewRecord, so its canonical finding order holds.
// The admission snapshot names the run and invocation, so an option that
// changes either also replaces Admission, as fake.SeededReviewAdmission does.
func ReviewRecord(opts ...func(*domain.ReviewRecord)) domain.ReviewRecord {
	in := domain.ReviewRecord{
		InvocationID: "review-run-1-1", RunID: "run-1", Round: 1,
		Admission:           fake.SeededReviewAdmission("run-1", "review-run-1-1"),
		ConfigurationDigest: goldenDigest("c"),
		InstructionDigest:   goldenDigest("d"),
		BaseSHA:             "beefcafe", HeadSHA: "cafebabe",
		CompletedAt: goldenTime, CompletionEvidence: goldenDigest("e"),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"find-1"},
	}
	for _, opt := range opts {
		opt(&in)
	}
	record, err := domain.NewReviewRecord(in)
	if err != nil {
		panic(fmt.Sprintf("domaintest.ReviewRecord(%s): %v", in.InvocationID, err))
	}
	return record
}

// ExecutionAdmission returns the golden execution admission, an attended
// Claude implementation attempt on the handoff backend. opts edit the input
// to domain.NewExecutionAdmission, so the admission's ID covers them. Its
// capabilities are listed unsorted to exercise the constructor's
// canonicalization.
func ExecutionAdmission(opts ...func(*domain.ExecutionAdmissionInput)) domain.ExecutionAdmission {
	stageInputs := goldenStageInputs()
	authIdentity := AuthIdentity().ID
	in := domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend: "fresh_vm_read_only_volume_handoff",
		Capabilities: domain.CapabilitySnapshot{
			domain.CapPostExitExport, domain.CapDetachableWorkspace, domain.CapReadOnlyRemount,
		},
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     goldenDigest("2"), PolicyDigest: goldenPolicy().Digest, InputDigest: goldenDigest("1"),
		StageInputs:    &stageInputs,
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "freeside-handoff-run-1-ws",
		AuthIdentityID: &authIdentity,
		AdmittedAt:     goldenTime,
	}
	for _, opt := range opts {
		opt(&in)
	}
	admission, err := domain.NewExecutionAdmission(in)
	if err != nil {
		panic(fmt.Sprintf("domaintest.ExecutionAdmission(%s): %v", in.InvocationID, err))
	}
	return admission
}

// AuthIdentity returns the golden auth identity, the owner's enabled Claude
// subscription identity, after applying opts.
func AuthIdentity(opts ...func(*domain.AuthIdentity)) domain.AuthIdentity {
	identity := domain.AuthIdentity{
		ID: "auth-claude-owner", Provider: "claude",
		AccountBinding: "acct-claude-owner-7f3a", UsagePool: "claude-owner-subscription",
		Budget: 500_000, AuthStoreMutationLease: true,
		Enabled: true, CostOwner: "owner",
		Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-owner-credentials", RefreshStrategy: domain.RefreshOnDemand},
	}
	for _, opt := range opts {
		opt(&identity)
	}
	if err := identity.Validate(); err != nil {
		panic(fmt.Sprintf("domaintest.AuthIdentity(%s): %v", identity.ID, err))
	}
	return identity
}

// goldenPolicy is the resolved policy the golden run and admission bind.
func goldenPolicy() domain.ResolvedPolicy {
	policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
		Key: "rein", Value: "tight",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
	}})
	if err != nil {
		panic(fmt.Sprintf("domaintest: golden policy: %v", err))
	}
	return policy
}

// goldenStageInputs is the stage input snapshot the golden admission binds.
func goldenStageInputs() domain.StageInputSnapshot {
	conversationDigest := goldenDigest("7")
	vendorDigest := goldenDigest("8")
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         goldenDigest("1"),
		SpecificationDigest: goldenDigest("2"),
		PromptPackageDigest: goldenDigest("3"),
		PolicyDigest:        goldenPolicy().Digest,
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{goldenDigest("4"), goldenDigest("5")},
		ImageInputDigests:    []domain.Digest{goldenDigest("6")},
	})
	if err != nil {
		panic(fmt.Sprintf("domaintest: golden stage inputs: %v", err))
	}
	return snapshot
}

// goldenDigest is the content address of 64 copies of the hex digit fill,
// the shape the golden fixtures use for placeholder digests.
func goldenDigest(fill string) domain.Digest {
	addr, ok := contentaddr.FromHex(strings.Repeat(fill, 64))
	if !ok {
		panic(fmt.Sprintf("domaintest: golden digest fill %q is not one hex digit", fill))
	}
	return domain.Digest(addr)
}
