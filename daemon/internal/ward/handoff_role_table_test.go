package ward

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/wardname"
)

// handoffRoleOrder is the observable behavior of every hand-kept list of
// handoff roles: the order teardown and recovery's adoption reap act in,
// which claims recovery marks attempted for each spec feature, and the name
// sets the exported namespace functions return. Its golden is written
// against the hand-kept lists and must not change when they move to one
// table.
type handoffRoleOrder struct {
	Teardown             []string                 `json:"teardown"`
	RecoveryAdoptionReap []string                 `json:"recovery_adoption_reap"`
	RecoveryAttempted    []recoveryAttemptedClaim `json:"recovery_attempted"`
	RuntimeResourceNames RuntimeResourceNames     `json:"runtime_resource_names"`
	FullConformanceNames RuntimeResourceNames     `json:"full_conformance_runtime_resource_names"`
}

type recoveryAttemptedClaim struct {
	Spec    string   `json:"spec"`
	Deletes []string `json:"deletes"`
}

const handoffRoleRegistryRunID = "golden-registry"

func TestHandoffRoleOrder(t *testing.T) {
	t.Parallel()
	got := handoffRoleOrder{
		Teardown:             handoffRoleTeardownDeletes(t),
		RecoveryAdoptionReap: handoffRoleAdoptionReaps(t),
		RuntimeResourceNames: sortedResourceNames(RuntimeResourceNamesFor(testHandoffSpec().RunID)),
		FullConformanceNames: sortedResourceNames(FullConformanceRuntimeResourceNamesFor(testHandoffSpec().RunID, handoffRoleRegistryRunID)),
	}
	for _, tc := range []struct {
		name   string
		enable func(*HandoffSpec)
	}{
		{"plain", func(*HandoffSpec) {}},
		{"claude_clean", func(hs *HandoffSpec) {
			hs.Agent.LaunchState = LaunchStateClaudeClean
		}},
		{"claude_clean_prompt_file", func(hs *HandoffSpec) {
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
		}},
		{"claude_clean_prompt_file_inputs", func(hs *HandoffSpec) {
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
			hs.Agent.Inputs, _ = testInputs("task/shot.png", "pixels")
		}},
	} {
		got.RecoveryAttempted = append(got.RecoveryAttempted, recoveryAttemptedClaim{
			Spec:    tc.name,
			Deletes: handoffRoleCanceledRecoveryDeletes(t, tc.enable),
		})
	}
	body, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "handoff-role-order", append(body, '\n'))
}

// handoffRoleTeardownDeletes runs teardown with every claim attempted and
// every handoff object standing as this run's, and returns the deletes in
// the order teardown issued them.
func handoffRoleTeardownDeletes(t *testing.T) []string {
	t.Helper()
	fx := newRecoveryFixture(t)
	runID := testHandoffSpec().RunID
	names := namesFor(runID)
	standEveryHandoffObject(t, fx, runID, names)
	st := &runState{ownershipLabel: Label{Key: ownershipLabelKey, Value: testRecoveryToken}}
	for _, claim := range []*objectClaim{
		&st.prompt, &st.promptSeeder, &st.promptObserver,
		&st.inputs, &st.inputsSeeder, &st.inputsObserver,
		&st.workspace, &st.instructions, &st.configRoot, &st.continuity, &st.sessionScratch,
		&st.seeder, &st.observer, &st.instructionSeeder, &st.instructionObserver,
		&st.configRootSeeder, &st.configRootObserver, &st.continuityObserver, &st.scratchObserver,
		&st.writerObserver, &st.credObsPre, &st.credObsPost, &st.agent, &st.exporter, &st.network,
	} {
		claim.attempted = true
	}
	start := fx.rt.callCount()
	if err := fx.backend(t).teardown(context.Background(), names, st); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	deletes := handoffRoleDeletes(fx, runID, start, "")
	if len(deletes) != 25 {
		t.Fatalf("teardown deleted %d objects, want all 25: %q", len(deletes), deletes)
	}
	return deletes
}

// handoffRoleAdoptionReaps runs recovery's adoption on a writer-complete
// record with every reapable container standing as this run's, and returns
// the container deletes up to the first create, which is where adoption
// starts rerunning roles. The agent stays absent: a standing writer
// contradicts the completion claim and refuses adoption before the reaps.
func handoffRoleAdoptionReaps(t *testing.T) []string {
	t.Helper()
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	names := namesFor(hs.RunID)
	standEveryHandoffObject(t, fx, hs.RunID, names)
	fx.rt.mu.Lock()
	delete(fx.rt.ctrs, names.Agent)
	fx.rt.mu.Unlock()
	start := fx.rt.callCount()
	// The outcome is not the subject here; only the reaps before the first
	// rerun are.
	_, _ = fx.recover(t, hs.RunID, hs)
	reaps := handoffRoleDeletes(fx, hs.RunID, start, "create-container ")
	if len(reaps) != 16 {
		t.Fatalf("adoption reaped %d containers, want 16: %q", len(reaps), reaps)
	}
	return reaps
}

// handoffRoleCanceledRecoveryDeletes runs recovery on a cancellation-
// requested record with every handoff object standing as this run's.
// Recovery returns before acting, so its deferred teardown deletes exactly
// the claims it marked attempted. Objects it did not mark survive and fail
// recovery's token audit; that error is expected and not the subject.
func handoffRoleCanceledRecoveryDeletes(t *testing.T, enable func(*HandoffSpec)) []string {
	t.Helper()
	fx := newRecoveryFixture(t)
	hs := fx.seed(t)
	enable(&hs)
	if len(hs.Agent.Inputs) > 0 {
		_, fx.cfg.InputBlobs = testInputs("task/shot.png", "pixels")
	}
	fx.openRecord(t, hs)
	if err := fx.j.MarkCancellationRequested(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	names := namesFor(hs.RunID)
	standEveryHandoffObject(t, fx, hs.RunID, names)
	start := fx.rt.callCount()
	_, _ = fx.recover(t, hs.RunID, hs)
	return handoffRoleDeletes(fx, hs.RunID, start, "")
}

// standEveryHandoffObject places every object a handoff may create in the
// fake runtime, labeled as this run's, so a delete list shows exactly which
// names a path acts on.
func standEveryHandoffObject(t *testing.T, fx *recoveryFixture, runID string, names handoffNames) {
	t.Helper()
	ctx := context.Background()
	labels := fx.runLabels(runID)
	for _, name := range []string{
		names.PromptSeeder, names.PromptObserver, names.InputsSeeder, names.InputsObserver,
		names.Seeder, names.Observer, names.InstructionSeeder, names.InstructionObserver,
		names.ConfigRootSeeder, names.ConfigRootObserver, names.ContinuityObserver,
		names.ScratchObserver, names.WriterObserver, names.CredObsPre, names.CredObsPost,
		names.Agent, names.Exporter,
	} {
		fx.worldContainer(t, ContainerSpec{
			Size:    testContainerSize,
			Name:    name,
			Image:   "example.test/img@sha256:" + strings.Repeat("9", 64),
			Command: []string{"sh"},
			Labels:  labels,
		}, false)
	}
	for _, name := range []string{
		names.Prompt, names.Inputs, names.Workspace, names.Instructions,
		names.ConfigRoot, names.Continuity, names.SessionScratch,
	} {
		fx.worldVolume(t, name, labels)
	}
	if err := fx.rt.CreateNetwork(ctx, names.Network, labels); err != nil {
		t.Fatal(err)
	}
}

// handoffRoleDeletes returns the runtime deletes recorded since start, each
// as its kind and role, stopping at the first call that begins with stop
// when stop is set.
func handoffRoleDeletes(fx *recoveryFixture, runID string, start int, stop string) []string {
	fx.rt.mu.Lock()
	defer fx.rt.mu.Unlock()
	prefix := wardname.Name(wardname.HandoffPrefix, runID, "")
	var deletes []string
	for _, call := range fx.rt.calls[start:] {
		if stop != "" && strings.HasPrefix(call, stop) {
			break
		}
		kind, name, ok := strings.Cut(call, " ")
		if !ok || !strings.HasPrefix(kind, "delete-") {
			continue
		}
		deletes = append(deletes, kind+" "+strings.TrimPrefix(name, prefix))
	}
	return deletes
}

func sortedResourceNames(r RuntimeResourceNames) RuntimeResourceNames {
	return RuntimeResourceNames{
		Containers: slices.Sorted(slices.Values(r.Containers)),
		Volumes:    slices.Sorted(slices.Values(r.Volumes)),
		Networks:   slices.Sorted(slices.Values(r.Networks)),
	}
}
