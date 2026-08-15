package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/contract"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type stageContractHarness struct {
	root      string
	driver    *Driver
	adapter   exec.StageDriver
	gate      *stubGate
	records   *stubExports
	artifacts *stubArtifacts
	source    blobSource
	scenario  contract.Scenario
	ready     chan struct{}
	release   chan struct{}
}

type terminalStageContractSeeder struct {
	entered chan struct{}
	release chan struct{}
}

func (s terminalStageContractSeeder) FetchBase(
	ctx context.Context, _, _, _, _ string,
) error {
	return s.finish(ctx)
}

func (s terminalStageContractSeeder) FetchBaseWorktree(
	ctx context.Context, _, _, _, _ string,
) error {
	return s.finish(ctx)
}

func (s terminalStageContractSeeder) finish(ctx context.Context) error {
	close(s.entered)
	select {
	case <-s.release:
		return errors.New("contract stage terminal failure")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newStageContractHarness(t *testing.T) contract.StageDriverHarness {
	t.Helper()
	h := &stageContractHarness{
		root:      t.TempDir(),
		records:   newStubExports(),
		artifacts: newStubArtifacts(),
	}
	t.Cleanup(func() {
		if h.driver != nil {
			_ = h.driver.Close(context.Background())
		}
	})
	return h
}

func (h *stageContractHarness) Prepare(
	t *testing.T, id domain.InvocationID, scenario contract.Scenario,
) exec.StartSpec {
	t.Helper()
	h.scenario = scenario
	spec, source := contractStageSpec(t, id)
	h.source = source
	h.gate = &stubGate{
		cancelFn: func(string) error { return ward.ErrJournalRecordNotFound },
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
		},
	}
	var seeder Seeder = stubSeeder{}
	switch scenario.Outcome {
	case contract.OutcomeCrashBeforeResult:
		h.gate.handoffFn = func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("contract crash before result")
		}
	case contract.OutcomeFail, contract.OutcomeCrashAfterResult:
		h.ready = make(chan struct{})
		h.release = make(chan struct{})
		seeder = terminalStageContractSeeder{entered: h.ready, release: h.release}
	case contract.OutcomeComplete:
		h.ready = make(chan struct{})
		seeder = cancelSeeder{entered: h.ready}
	default:
		t.Fatalf("unknown stage contract outcome %q", scenario.Outcome)
	}
	h.build(t, seeder)
	return spec
}

func (h *stageContractHarness) Driver() exec.StageDriver { return h.adapter }

func (h *stageContractHarness) AwaitReady(t *testing.T, _ domain.InvocationID) {
	t.Helper()
	if h.ready == nil {
		return
	}
	select {
	case <-h.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("stage contract scenario did not reach its hold point")
	}
}

func (h *stageContractHarness) Finish(t *testing.T, id domain.InvocationID) {
	t.Helper()
	if h.scenario.Outcome == contract.OutcomeCrashBeforeResult {
		waitSessionDone(t, h.driver, id)
		return
	}
	if h.scenario.Outcome == contract.OutcomeFail ||
		h.scenario.Outcome == contract.OutcomeCrashAfterResult {
		close(h.release)
		waitSessionDone(t, h.driver, id)
		return
	}
	if err := h.adapter.Cancel(t.Context(), id); err != nil {
		t.Fatalf("finish stage contract scenario: %v", err)
	}
}

func (h *stageContractHarness) Restart(t *testing.T) exec.StageDriver {
	t.Helper()
	if h.driver != nil {
		if err := h.driver.Close(t.Context()); err != nil {
			t.Fatalf("close stage driver for restart: %v", err)
		}
	}
	h.build(t, stubSeeder{})
	return h.adapter
}

func (h *stageContractHarness) build(t *testing.T, seeder Seeder) {
	t.Helper()
	driver, err := New(Config{
		ErrorPrefix: "contract stage driver", DisplayName: "Contract",
		Provider:        testProvider{volumes: stubVolumes{volume: testAuthVol}},
		CredentialMount: testCredentialMountPolicy,
		Lifetime:        context.Background(),
		Dir:             filepath.Join(h.root, "driver"),
		SeedRoot:        filepath.Join(h.root, "seeds"),
		ExportRoot:      filepath.Clean(os.TempDir()),
		Gate:            h.gate,
		Seeder:          seeder,
		Exports:         h.records,
		ImportStarts:    h.records,
		Outcomes:        h.records,
		Authority:       stubAuthority{},
		Artifacts:       h.artifacts,
		Import:          importer.Options{},
		Now:             func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	materializer, err := exec.NewMaterializer(h.source, exec.MaterializerOptions{
		MaxInputBytes: 1 << 20, MaxTotalBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := exec.NewMaterializingStageDriver(materializer, driver)
	if err != nil {
		t.Fatal(err)
	}
	h.driver = driver
	h.adapter = adapter
}

func contractStageSpec(
	t *testing.T, id domain.InvocationID,
) (exec.StartSpec, blobSource) {
	t.Helper()
	specBody := []byte("# Contract work item\n")
	promptBody := []byte("Run the contract scenario.\n")
	policyBody := []byte(`[{"key":"paths","value":"daemon/**"}]`)
	vendorBody := []byte("# Contract host instructions\n")
	vendorDigest := digestOf(vendorBody)
	spec := testStartSpec()
	spec.AttemptID = domain.AttemptID("attempt-" + string(id))
	spec.Workspace = testWorkspaceFor(id)
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         spec.InputDigest,
		SpecificationDigest: digestOf(specBody),
		PromptPackageDigest: digestOf(promptBody),
		PolicyDigest:        digestOf(policyBody),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest: &vendorDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest = snapshot.SpecificationDigest
	spec.PolicyDigest = snapshot.PolicyDigest
	spec.StageInputs = &snapshot
	return spec, blobSource{
		snapshot.SpecificationDigest: specBody,
		snapshot.PromptPackageDigest: promptBody,
		snapshot.PolicyDigest:        policyBody,
		vendorDigest:                 vendorBody,
	}
}

func TestStageDriverContract(t *testing.T) {
	contract.RunStageDriverContract(t, contract.StageDriverFactory{
		New: newStageContractHarness,
		KnownDivergences: []contract.KnownDivergence{
			{Case: contract.StageCaseStatusVocabulary, Issue: 661, Failure: "first Inspect = exec.Inspection{Status:\"running\", Live:true}, want StatusPending"},
			{Case: contract.StageCaseCrashAfterResult, Issue: 662, Failure: "Inspect after crash-after-result = exec.Inspection{Status:\"failed\", Live:false}, want StatusGone"},
			{Case: contract.StageCaseStreamReplay, Issue: 666, Failure: "first stream = \"\", want \"first line\\nsecond line\\n\""},
		},
	})
}

var _ contract.StageDriverHarness = (*stageContractHarness)(nil)
