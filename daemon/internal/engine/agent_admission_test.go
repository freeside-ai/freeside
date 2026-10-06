package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// The lineup admitter (plan §5.4, issue #867): a ward stage is admitted
// through its role's lineup line, the admission records the agent binding,
// and an identity the flags still select keeps admitting as it did.

const (
	agentTestInterimVolume    = "claude-interim-auth"
	agentTestGenerationVolume = "claude-generation-auth"
	agentTestInputArtifact    = domain.ArtifactID("input-1")
)

var (
	agentTestAt     = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	agentTestEgress = []string{"api.anthropic.com:443"}
)

func agentTestDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

type agentAdmissionFixture struct {
	engine     *Engine
	store      *store.Store
	adapters   *wardstore.Adapters
	selection  AgentSelection
	identity   domain.AuthIdentity
	enrollment domain.ClientEnrollment
	generation domain.EnrollmentGeneration
	run        domain.Run
	binding    invocationBinding
	stage      domain.Stage
}

var agentTestPrompts = map[domain.RoleName]agentbaseline.Prompt{
	domain.RoleSpecifier:   {Name: "specifier", Digest: agentTestDigest("7")},
	domain.RoleImplementer: {Name: "implementer", Digest: agentTestDigest("3")},
	domain.RoleRemediator:  {Name: "remediator", Digest: agentTestDigest("8")},
}

// newAgentAdmissionFixture seeds an adopted Claude identity whose generation
// mounts a volume other than the identity's interim one, the baseline tree
// over it, and the baseline conformance records, and returns an engine that
// admits through that tree.
func newAgentAdmissionFixture(t *testing.T) *agentAdmissionFixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	f := &agentAdmissionFixture{store: st, adapters: adapters}
	f.identity = domain.AuthIdentity{
		ID: "claude-main", Provider: "claude", AccountBinding: "acct-fixture-0001",
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: agentTestInterimVolume, RefreshStrategy: domain.RefreshExternal,
		},
	}
	f.enrollment = domain.ClientEnrollment{
		ID: "claude-main/claude_code", AuthIdentityID: f.identity.ID,
		HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic-subscription",
		AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, AccountBinding: f.identity.AccountBinding,
	}
	manifest := agentTestDigest("5")
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, f.identity, agentTestAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, f.enrollment, agentTestAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, f.identity.ID, "inv-adopt",
			&domain.LeaseGenerationBinding{
				EnrollmentID: f.enrollment.ID, Generation: 0,
				AuthStoreVolume: agentTestGenerationVolume, StoreManifestDigest: manifest,
			}, agentTestAt.Add(-2*time.Minute), agentTestAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		f.generation, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: f.enrollment.ID, AuthStoreVolume: agentTestGenerationVolume,
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: f.identity.AccountBinding, RecordedAt: agentTestAt.Add(-time.Minute),
		}, agentTestAt.Add(-time.Minute))
		if err != nil {
			return err
		}
		records, err := agentbaseline.ConformanceRecords(agentTestAt.Add(-time.Minute))
		if err != nil {
			return err
		}
		for _, record := range records {
			if _, err := tx.RecordAdapterConformance(ctx, record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: f.enrollment, TermsBasisDate: "2026-08-01",
		PricingRevision: "pricing-2026-08", OfferNotAfter: agentTestAt.Add(30 * 24 * time.Hour),
		Prompts: agentTestPrompts,
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		t.Fatal(err)
	}
	// Admission reads the tree as the daemon loads it: parsed from its files.
	if tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	f.selection = AgentSelection{
		Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
		EffectiveEgress: slices.Clone(agentTestEgress), AttemptBudget: time.Hour, ExpiryMargin: 5 * time.Minute,
	}
	vendorPath := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(vendorPath, []byte("# Host instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.run = seedRunPolicy(t, st, domain.Run{
		ID: "run-1", ProjectID: "project-1", SpecDigest: agentTestDigest("4"),
	}, nil)
	f.stage = domain.Stage{ID: productionStageID(f.run.ID), RunID: f.run.ID, Name: productionStageName}
	input, err := domain.NewArtifact(domain.ArtifactInput{
		ID: agentTestInputArtifact, Type: domain.ArtifactKindEvidence, Digest: agentTestDigest("1"),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-producer",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, input) }); err != nil {
		t.Fatal(err)
	}
	invocation, err := domain.NewAgentInvocation(
		productionInvocationID(f.run.ID), []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.binding = invocationBinding{run: f.run, invocation: invocation}
	selection := f.selection
	f.engine = &Engine{
		store: st, signet: signet.NewService(st, signet.WithBlobStore(blobs)),
		specification: &specificationWorkflow{
			promptPackage: agentTestPrompts[domain.RoleSpecifier].Digest, blobs: blobs,
		},
		productionPublication: &productionPublicationWorkflow{
			remediationPromptPackage: agentTestPrompts[domain.RoleRemediator].Digest,
		},
		admission: &admitter{
			backend: stageInputBackend{},
			floor:   []exec.Capability{exec.CapPostExitExport},
			environment: AdmissionEnvironment{
				OperatingMode:       domain.ModeAttendedDev,
				CredentialMode:      domain.CredentialSubscriptionContained,
				EgressProfile:       domain.EgressProviderOnly,
				ImageRef:            domain.ImageRef("agent@sha256:" + strings.Repeat("ab", 32)),
				PromptPackageDigest: agentTestPrompts[domain.RoleImplementer].Digest,
				VendorInstructions: VendorInstructionConfig{
					Vendor:   domain.AgentVendorClaude,
					Delivery: domain.VendorInstructionDeliveryAppendFile,
					HostPath: vendorPath,
				},
				Base: domain.BaseRevision{
					Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
				},
				Workspace: "workspace-1", Agents: &selection,
				// WithAdmission defaults this set; a directly built admitter
				// states it.
				EnforceableEgressProfiles: []domain.EgressProfile{domain.EgressProviderOnly},
			},
			now: func() time.Time { return agentTestAt },
		},
	}
	return f
}

// record stores the admission as the engine does, with its attempt, so the
// store's own gates run over it.
func (f *agentAdmissionFixture) record(t *testing.T, admission domain.ExecutionAdmission) {
	t.Helper()
	ctx := context.Background()
	run := f.run
	stage := f.stage
	stage.Attempts = []domain.Attempt{{
		ID: admission.AttemptID, StageID: stage.ID, Number: 1, InvocationID: admission.InvocationID,
	}}
	run.Stages = []domain.Stage{stage}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record admission: %v", err)
	}
}

func TestLineupAdmissionBindsTheRolesAgent(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	admission, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	binding := admission.AgentBinding
	if binding == nil {
		t.Fatal("a lineup admission recorded no agent binding")
	}
	lineup, err := f.selection.Tree.ResolveLineup()
	if err != nil {
		t.Fatal(err)
	}
	line, _ := lineup.Line(domain.RoleImplementer)
	launch, err := agentbaseline.RoleLaunch(domain.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if binding.AgentDigest != line.AgentDigest || binding.LaunchDigest != launch.Digest ||
		binding.LineupRevision != f.selection.LineupRevision ||
		binding.EnrollmentID != f.enrollment.ID || binding.EnrollmentGeneration != f.generation.Ordinal ||
		binding.StoreManifestDigest != f.generation.StoreManifestDigest ||
		binding.PricingRevision != "pricing-2026-08" || !binding.Attended ||
		!slices.Equal(binding.EffectiveEgress, agentTestEgress) {
		t.Fatalf("agent binding = %+v", *binding)
	}
	if admission.AuthIdentityID == nil || *admission.AuthIdentityID != f.identity.ID ||
		admission.CredentialMode != f.enrollment.CredentialMode {
		t.Fatalf("admitted identity %v mode %q, want the enrollment's", admission.AuthIdentityID, admission.CredentialMode)
	}
	spec := exec.StartSpecFromAdmission(admission)
	if spec.AuthIdentityID != f.identity.ID || spec.RouteModelID != binding.RouteModelID ||
		spec.NativeEffort != binding.NativeEffort {
		t.Fatalf("start spec = identity %q model %q effort %q", spec.AuthIdentityID, spec.RouteModelID, spec.NativeEffort)
	}

	// The store's write gate accepts the binding, and the attempt then mounts
	// the generation it was admitted against, not the identity's interim
	// volume.
	f.record(t, admission)
	volume, err := f.adapters.Leaser.AuthStoreVolume(ctx, f.identity.ID, admission.InvocationID)
	if err != nil || volume != agentTestGenerationVolume {
		t.Fatalf("agent-bound volume = %q, %v; want %q", volume, err, agentTestGenerationVolume)
	}
	if _, err := f.adapters.Leaser.AuthStoreVolume(ctx, "another-identity", admission.InvocationID); !errors.Is(
		err, domain.ErrAdmissionDerivationMismatch) {
		t.Fatalf("volume under another identity = %v, want a derivation mismatch", err)
	}
	// A holder with no admission (an enrollment or refresh mutation) reads
	// the interim binding.
	volume, err = f.adapters.Leaser.AuthStoreVolume(ctx, f.identity.ID, "inv-refresh")
	if err != nil || volume != agentTestInterimVolume {
		t.Fatalf("unadmitted holder volume = %q, %v; want %q", volume, err, agentTestInterimVolume)
	}
}

// TestFlagSelectedAdmissionStillAdmitsAnAdoptedIdentity is the dual-read
// guarantee: adoption changes nothing a flag-selected admission reads, so a
// daemon started without the tree admits the adopted identity as before and
// its attempt mounts the interim volume.
func TestFlagSelectedAdmissionStillAdmitsAnAdoptedIdentity(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	identity := f.identity.ID
	f.engine.admission.environment.Agents = nil
	f.engine.admission.environment.AuthIdentityID = &identity
	admission, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	if admission.AgentBinding != nil || admission.AuthIdentityID == nil || *admission.AuthIdentityID != identity {
		t.Fatalf("flag-selected admission = binding %v identity %v", admission.AgentBinding, admission.AuthIdentityID)
	}
	f.record(t, admission)
	volume, err := f.adapters.Leaser.AuthStoreVolume(ctx, identity, admission.InvocationID)
	if err != nil || volume != agentTestInterimVolume {
		t.Fatalf("legacy volume = %q, %v; want %q", volume, err, agentTestInterimVolume)
	}
}

func TestLineupAdmissionPicksTheRoleFromTheAttempt(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	launchDigest := func(role domain.RoleName) domain.Digest {
		t.Helper()
		launch, err := agentbaseline.RoleLaunch(role)
		if err != nil {
			t.Fatal(err)
		}
		return launch.Digest
	}
	remediationID := remediationInvocationID(f.run.ID, 1)
	cases := []struct {
		name       string
		stage      domain.Stage
		invocation domain.InvocationID
		role       domain.RoleName
	}{
		{
			"specification",
			domain.Stage{ID: specificationStageID(f.run.ID), Name: specificationStageName},
			"inv-specification", domain.RoleSpecifier,
		},
		{
			"remediation",
			domain.Stage{ID: remediationStageID(f.run.ID, 1), Name: productionStageName},
			remediationID, domain.RoleRemediator,
		},
		{
			"operator feedback",
			domain.Stage{ID: operatorFeedbackStageID("inv-feedback"), Name: productionStageName},
			"inv-feedback", domain.RoleRemediator,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := f.binding
			inputs := []domain.ArtifactID{agentTestInputArtifact}
			var conversationID *domain.ConversationID
			prefix := 0
			if tc.role == domain.RoleSpecifier {
				// A specification attempt reads the task conversation, not
				// prior artifacts.
				message, err := domain.NewMessage(
					"message-1", "conversation-1", domain.AuthorUser, "please specify", nil, agentTestAt)
				if err != nil {
					t.Fatal(err)
				}
				conversation, _ := domain.Conversation{ID: "conversation-1", Status: domain.ConversationIdle}.Append(message)
				binding.conversation = conversation
				inputs, conversationID, prefix = nil, &conversation.ID, 1
			}
			invocation, err := domain.NewAgentInvocation(tc.invocation, inputs, conversationID, prefix)
			if err != nil {
				t.Fatal(err)
			}
			binding.invocation = invocation
			admission, admitted, err := f.engine.admitAttempt(ctx, binding, tc.stage, tc.invocation)
			if err != nil || !admitted || admission.AgentBinding == nil {
				t.Fatalf("admitAttempt = %t, %v", admitted, err)
			}
			if admission.AgentBinding.LaunchDigest != launchDigest(tc.role) ||
				admission.StageInputs.PromptPackageDigest != agentTestPrompts[tc.role].Digest {
				t.Fatalf("admission launch %s prompt %s, want role %s",
					admission.AgentBinding.LaunchDigest, admission.StageInputs.PromptPackageDigest, tc.role)
			}
		})
	}

	// A stage no ward role runs has no agent to bind.
	other, err := domain.NewAgentInvocation("inv-other", []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	binding := f.binding
	binding.invocation = other
	if _, admitted, err := f.engine.admitAttempt(
		ctx, binding, domain.Stage{ID: "stage-other", Name: "verify"}, other.ID,
	); admitted || !errors.Is(err, ErrAgentNotAdmissible) {
		t.Fatalf("non-ward stage = %t, %v", admitted, err)
	}
}

func TestLineupAdmissionRefusals(t *testing.T) {
	ctx := context.Background()
	implementerPrompt := agentTestPrompts[domain.RoleImplementer].Digest
	cases := []struct {
		name   string
		mode   domain.OperatingMode
		prompt domain.Digest
		at     time.Time
		mutate func(t *testing.T, f *agentAdmissionFixture, selection *AgentSelection)
		want   string
	}{
		{
			name:   "the attempt runs a prompt the line does not select",
			prompt: agentTestDigest("9"), want: "the lineup selects prompt",
		},
		{
			name: "unattended without an attended mark",
			mode: domain.ModeUnattended,
			mutate: func(_ *testing.T, _ *agentAdmissionFixture, selection *AgentSelection) {
				selection.Tree.Marks = nil
			},
			want: "no attended mark",
		},
		{
			name: "the identity is disabled",
			mutate: func(t *testing.T, f *agentAdmissionFixture, _ *AgentSelection) {
				disabled := f.identity
				disabled.Enabled = false
				if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
					return tx.RecordAuthIdentity(ctx, disabled, agentTestAt)
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: "is disabled",
		},
		{
			name: "the offer lapses before the attempt's deadline",
			at:   agentTestAt.Add(30 * 24 * time.Hour),
			want: "not_after precedes the attempt deadline",
		},
		{
			name: "a provider endpoint outside the route",
			mutate: func(_ *testing.T, _ *agentAdmissionFixture, selection *AgentSelection) {
				selection.EffectiveEgress = []string{"api.anthropic.com:443", "example.com:443"}
			},
			want: "outside the route's inference authorities",
		},
		{
			name: "the line names an agent digest the tree no longer resolves",
			mutate: func(_ *testing.T, _ *agentAdmissionFixture, selection *AgentSelection) {
				lineup := slices.Clone(selection.Tree.Lineup)
				for i := range lineup {
					lineup[i].Selection.AgentDigest = agentTestDigest("e")
				}
				selection.Tree.Lineup = lineup
			},
			want: "the lineup selects agent digest",
		},
		{
			name: "the launch source refuses the role",
			mutate: func(_ *testing.T, _ *agentAdmissionFixture, selection *AgentSelection) {
				selection.Launch = func(domain.RoleName) (domain.LaunchSpec, error) {
					return domain.LaunchSpec{}, errors.New("no launch")
				}
			},
			want: "no launch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			selection := f.selection
			mode, prompt, at := tc.mode, tc.prompt, tc.at
			if mode == "" {
				mode = domain.ModeAttendedDev
			}
			if prompt == "" {
				prompt = implementerPrompt
			}
			if at.IsZero() {
				at = agentTestAt
			}
			if tc.mutate != nil {
				tc.mutate(t, f, &selection)
			}
			_, err := resolveAgentAdmission(ctx, f.store, selection, domain.RoleImplementer, prompt, mode, at)
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolve = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestLineupAdmissionRequiresAdapterConformance records a later failed pass:
// the latest record decides, so the earlier passed one no longer proves the
// launch.
func TestLineupAdmissionRequiresAdapterConformance(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	adapters, err := agentbaseline.Adapters()
	if err != nil {
		t.Fatal(err)
	}
	failedAt := agentTestAt
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, adapter := range adapters {
			record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
				AdapterDigest: adapter.Digest, Outcome: domain.ConformanceFailed, ProvedAt: failedAt,
			})
			if err != nil {
				return err
			}
			if _, err := tx.RecordAdapterConformance(ctx, record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = resolveAgentAdmission(ctx, f.store, f.selection, domain.RoleImplementer, agentTestPrompts[domain.RoleImplementer].Digest,
		domain.ModeAttendedDev, agentTestAt,
	)
	if !errors.Is(err, ErrAgentNotAdmissible) {
		t.Fatalf("resolve after a failed conformance pass = %v", err)
	}
}

func TestUnattendedLineupAdmissionRecordsTheMark(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	resolved, err := resolveAgentAdmission(
		context.Background(), f.store, f.selection, domain.RoleImplementer,
		agentTestPrompts[domain.RoleImplementer].Digest, domain.ModeUnattended, agentTestAt,
	)
	if err != nil || resolved.binding.Attended {
		t.Fatalf("unattended resolve = attended %t, %v", resolved.binding.Attended, err)
	}
}

func TestWithAdmissionRefusesAgentSelectionBesideAnIdentity(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	identity := f.identity.ID
	env := f.engine.admission.environment
	env.AuthIdentityID = &identity
	err := WithAdmission(stageInputBackend{}, []exec.Capability{exec.CapPostExitExport}, env, time.Now)(&Engine{})
	if err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("WithAdmission = %v, want the exclusivity refusal", err)
	}
}

// TestLineupAdmissionGateHoldsEveryAttempt closes the gate: no attempt is
// admitted, and the refusal carries the gate's reason.
func TestLineupAdmissionGateHoldsEveryAttempt(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	held := errors.New("selection is not active")
	f.engine.admission.environment.Agents.Gate = func(context.Context) error { return held }
	_, admitted, err := f.engine.admitAttempt(context.Background(), f.binding, f.stage, f.binding.invocation.ID)
	if admitted || !errors.Is(err, ErrAgentNotAdmissible) || !errors.Is(err, held) {
		t.Fatalf("admitAttempt behind a closed gate = %t, %v", admitted, err)
	}
	// The refusal holds the invocation in either mode. Left unclassified it
	// would return from dispatch as an error and stop the workflow loop.
	if !invocationDispatchHold(err) {
		t.Fatal("a closed gate was not classified as an invocation hold")
	}
	if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldAdmissionPolicyRefused {
		t.Fatalf("closed gate hold reason = %q, %t", reason, ok)
	}
	// The startup check decides whether to open the gate, so it ignores it.
	if _, err := f.engine.admission.environment.Agents.CheckRole(
		context.Background(), f.store, domain.RoleImplementer,
		agentTestPrompts[domain.RoleImplementer].Digest, domain.ModeAttendedDev, agentTestAt,
	); err != nil {
		t.Fatalf("CheckRole behind a closed gate = %v", err)
	}
}

// TestCutoverLeavesLegacyAdmissionsAndBindsQueuedRuns is the cutover seen
// from two runs. The first was admitted by the flag-selected daemon: after the
// cutover its admission still names only its identity, and its attempt keeps
// mounting the interim volume. The second was queued before the cutover with
// no admission: the lineup daemon admits it with an agent binding.
func TestCutoverLeavesLegacyAdmissionsAndBindsQueuedRuns(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	identity := f.identity.ID
	agents := f.engine.admission.environment.Agents

	queued := domain.Run{
		ID: "run-queued", ProjectID: f.run.ProjectID,
		SpecDigest: f.run.SpecDigest, PolicyDigest: f.run.PolicyDigest,
	}
	queuedStage := domain.Stage{ID: productionStageID(queued.ID), RunID: queued.ID, Name: productionStageName}
	queued.Stages = []domain.Stage{queuedStage}
	queued = seedRunPolicy(t, f.store, queued, nil)

	// Before the cutover: the daemon runs on -auth-identity.
	f.engine.admission.environment.Agents = nil
	f.engine.admission.environment.AuthIdentityID = &identity
	legacy, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
	if err != nil || !admitted || legacy.AgentBinding != nil {
		t.Fatalf("flag-selected admitAttempt = %t, %v, binding %v", admitted, err, legacy.AgentBinding)
	}
	f.record(t, legacy)

	// The cutover: the same store, a daemon that selects through the lineup.
	f.engine.admission.environment.AuthIdentityID = nil
	f.engine.admission.environment.Agents = agents

	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var found bool
		var err error
		stored, found, err = tx.LookupExecutionAdmission(ctx, legacy.InvocationID)
		if err == nil && !found {
			err = errors.New("the legacy admission is gone")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stored.AgentBinding != nil || stored.AuthIdentityID == nil || *stored.AuthIdentityID != identity {
		t.Fatalf("legacy admission after the cutover = binding %v identity %v", stored.AgentBinding, stored.AuthIdentityID)
	}
	volume, err := f.adapters.Leaser.AuthStoreVolume(ctx, identity, legacy.InvocationID)
	if err != nil || volume != agentTestInterimVolume {
		t.Fatalf("legacy volume after the cutover = %q, %v; want %q", volume, err, agentTestInterimVolume)
	}

	invocation, err := domain.NewAgentInvocation(
		productionInvocationID(queued.ID), []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	admission, admitted, err := f.engine.admitAttempt(
		ctx, invocationBinding{run: queued, invocation: invocation}, queuedStage, invocation.ID)
	if err != nil || !admitted || admission.AgentBinding == nil {
		t.Fatalf("queued run admitAttempt = %t, %v", admitted, err)
	}
	if admission.AgentBinding.EnrollmentID != f.enrollment.ID ||
		admission.AgentBinding.LineupRevision != f.selection.LineupRevision {
		t.Fatalf("queued run binding = %+v", *admission.AgentBinding)
	}
	queuedStage.Attempts = []domain.Attempt{{
		ID: admission.AttemptID, StageID: queuedStage.ID, Number: 1, InvocationID: admission.InvocationID,
	}}
	queued.Stages = []domain.Stage{queuedStage}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, queued); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record the queued run's admission: %v", err)
	}
	volume, err = f.adapters.Leaser.AuthStoreVolume(ctx, identity, admission.InvocationID)
	if err != nil || volume != agentTestGenerationVolume {
		t.Fatalf("queued run volume = %q, %v; want %q", volume, err, agentTestGenerationVolume)
	}
}

// TestLineupAdmissionRefusesAMarkedGeneration is §5.4 admission rule 4's
// integrity half (issue #1624): a current generation the credential-integrity
// probe marked is not credentialed, the refusal is typed through every
// resolution path, and re-enrollment's new generation clears it.
func TestLineupAdmissionRefusesAMarkedGeneration(t *testing.T) {
	ctx := context.Background()
	for _, finding := range domain.AllCredentialIntegrityFindings {
		t.Run(string(finding), func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			mark := domain.GenerationIntegrityMark{
				EnrollmentID: f.enrollment.ID, Ordinal: f.generation.Ordinal,
				Finding: finding, ObservedAt: agentTestAt.Add(-30 * time.Second),
			}
			if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.RecordGenerationIntegrityMark(ctx, mark)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertRefusal := func(path string, err error) {
				t.Helper()
				if !errors.Is(err, ErrAgentNotAdmissible) || !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
					t.Fatalf("%s = %v, want %v and %v",
						path, err, ErrAgentNotAdmissible, domain.ErrGenerationIntegrityMarked)
				}
				var marked *domain.GenerationIntegrityMarkedError
				if !errors.As(err, &marked) || marked.Mark != mark {
					t.Fatalf("%s refusal mark = %+v, want %+v", path, marked, mark)
				}
				// The startup role check prints this error as its hold reason.
				for _, want := range []string{string(f.enrollment.ID), "generation 1", string(finding)} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s refusal %q does not name %q", path, err, want)
					}
				}
			}
			_, err := ResolveRole(ctx, f.store, f.selection.Tree, domain.RoleImplementer)
			assertRefusal("ResolveRole", err)
			_, err = f.selection.CheckRole(ctx, f.store, domain.RoleImplementer,
				agentTestPrompts[domain.RoleImplementer].Digest, domain.ModeAttendedDev, agentTestAt)
			assertRefusal("CheckRole", err)
			_, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
			if admitted {
				t.Fatal("an attempt was admitted on a marked generation")
			}
			assertRefusal("admitAttempt", err)

			// Re-enrollment appends generation 2, which carries no mark.
			var next domain.EnrollmentGeneration
			if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if err := tx.ReleaseAuthStoreMutationLease(
					ctx, f.identity.ID, "inv-adopt", f.generation.LeaseFence, agentTestAt.Add(-25*time.Second)); err != nil {
					return err
				}
				lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, f.identity.ID, "inv-reenroll",
					&domain.LeaseGenerationBinding{
						EnrollmentID: f.enrollment.ID, Generation: f.generation.Ordinal,
						AuthStoreVolume:     agentTestGenerationVolume,
						StoreManifestDigest: f.generation.StoreManifestDigest,
					}, agentTestAt.Add(-20*time.Second), agentTestAt.Add(10*time.Minute))
				if err != nil {
					return err
				}
				next, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
					EnrollmentID: f.enrollment.ID, AuthStoreVolume: agentTestGenerationVolume,
					StoreManifestDigest: f.generation.StoreManifestDigest, LeaseFence: lease.Fence,
					AccountBinding: f.identity.AccountBinding, RecordedAt: agentTestAt.Add(-10 * time.Second),
				}, agentTestAt.Add(-10*time.Second))
				return err
			}); err != nil {
				t.Fatalf("re-enroll: %v", err)
			}
			if _, err := ResolveRole(ctx, f.store, f.selection.Tree, domain.RoleImplementer); err != nil {
				t.Fatalf("ResolveRole after re-enrollment: %v", err)
			}
			admission, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
			if err != nil || !admitted {
				t.Fatalf("admitAttempt after re-enrollment = %t, %v", admitted, err)
			}
			if admission.AgentBinding == nil || admission.AgentBinding.EnrollmentGeneration != next.Ordinal ||
				next.Ordinal != f.generation.Ordinal+1 {
				t.Fatalf("admitted binding = %+v, want generation %d", admission.AgentBinding, f.generation.Ordinal+1)
			}
			f.record(t, admission)
			// Generation 1's mark is history: still there, still refusing.
			err = f.store.Read(ctx, func(tx *store.ReadTx) error {
				return tx.RequireGenerationUnmarked(ctx, f.enrollment.ID, f.generation.Ordinal)
			})
			var marked *domain.GenerationIntegrityMarkedError
			if !errors.As(err, &marked) || marked.Mark != mark {
				t.Fatalf("generation 1 after re-enrollment = %v, want mark %+v", err, mark)
			}
		})
	}
}

// TestMarkAfterResolutionHoldsTheInvocation covers the race the admitting
// transaction's check exists for: role resolution read an unmarked
// generation, and the mark landed before the admission was recorded. The
// store's refusal arrives without ErrAgentNotAdmissible, and it must hold
// the invocation as the read-side refusal does, not fail the reconcile pass.
func TestMarkAfterResolutionHoldsTheInvocation(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	admission, admitted, err := f.engine.admitAttempt(ctx, f.binding, f.stage, f.binding.invocation.ID)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordGenerationIntegrityMark(ctx, domain.GenerationIntegrityMark{
			EnrollmentID: f.enrollment.ID, Ordinal: f.generation.Ordinal,
			Finding: domain.CredentialIntegrityCorruption, ObservedAt: agentTestAt,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run, stage := f.run, f.stage
	stage.Attempts = []domain.Attempt{{
		ID: admission.AttemptID, StageID: stage.ID, Number: 1, InvocationID: admission.InvocationID,
	}}
	run.Stages = []domain.Stage{stage}
	err = f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	})
	if !errors.Is(err, domain.ErrGenerationIntegrityMarked) || errors.Is(err, ErrAgentNotAdmissible) {
		t.Fatalf("record after the mark = %v, want the store's %v alone", err, domain.ErrGenerationIntegrityMarked)
	}
	if !invocationDispatchHold(err) {
		t.Fatal("the store's marked-generation refusal does not hold the invocation")
	}
	if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldAdmissionPolicyRefused {
		t.Fatalf("hold reason = %q, %t, want %q", reason, ok, domain.HoldAdmissionPolicyRefused)
	}
}
