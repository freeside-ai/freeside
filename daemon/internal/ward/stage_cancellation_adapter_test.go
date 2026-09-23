package ward_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type cancellationAuthority struct{}

func (cancellationAuthority) AuthenticateAdmission(context.Context, domain.InvocationID, exec.StartSpec) error {
	return nil
}

func (cancellationAuthority) AuthenticateStart(context.Context, domain.InvocationID, exec.StartSpec) error {
	return nil
}

func (cancellationAuthority) AuthenticateImport(context.Context, domain.InvocationID, exec.StartSpec) error {
	return errors.New("canceled handoff reached import")
}

func (cancellationAuthority) ImportOptions(
	context.Context, domain.InvocationID, exec.StartSpec, importer.Options,
) (importer.Options, error) {
	return importer.Options{}, errors.New("canceled handoff reached import")
}

func (cancellationAuthority) ImportOptionsRecord(
	context.Context, domain.InvocationID, exec.StartSpec, importer.Options,
) (importer.Options, error) {
	return importer.Options{}, errors.New("canceled handoff reached import")
}

type cancellationRecords struct {
	mu      sync.Mutex
	outcome *domain.ExecutionOutcome
}

func (*cancellationRecords) RecordExecutionExport(context.Context, domain.ExecutionExport, stage.ExecutionReplay) error {
	return errors.New("canceled handoff recorded export")
}

func (*cancellationRecords) LookupExecutionExport(context.Context, domain.InvocationID) (domain.ExecutionExport, bool, error) {
	return domain.ExecutionExport{}, false, nil
}

func (r *cancellationRecords) LookupExecutionExportRecord(ctx context.Context, id domain.InvocationID) (domain.ExecutionExport, bool, error) {
	return r.LookupExecutionExport(ctx, id)
}

func (*cancellationRecords) RecordCurrentImportStart(context.Context, domain.CurrentImportStart) error {
	return errors.New("canceled handoff reached import")
}

func (*cancellationRecords) LookupCurrentImportStart(context.Context, domain.InvocationID) (domain.CurrentImportStart, bool, error) {
	return domain.CurrentImportStart{}, false, nil
}

func (r *cancellationRecords) RecordExecutionOutcome(_ context.Context, outcome domain.ExecutionOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome != nil {
		return domain.ErrImmutableTransition
	}
	r.outcome = &outcome
	return nil
}

func (r *cancellationRecords) LookupExecutionOutcome(_ context.Context, id domain.InvocationID) (domain.ExecutionOutcome, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome == nil || r.outcome.InvocationID != id {
		return domain.ExecutionOutcome{}, false, nil
	}
	return *r.outcome, true, nil
}

func (r *cancellationRecords) LookupExecutionOutcomeRecord(ctx context.Context, id domain.InvocationID) (domain.ExecutionOutcome, bool, error) {
	return r.LookupExecutionOutcome(ctx, id)
}

func (*cancellationRecords) RecordExportRejection(context.Context, domain.ExportRejection) error {
	return errors.New("canceled handoff recorded export rejection")
}

func (*cancellationRecords) LookupExportRejection(context.Context, domain.InvocationID) (domain.ExportRejection, bool, error) {
	return domain.ExportRejection{}, false, nil
}

type cancellationArtifacts struct{ stage.Artifacts }

type cancellationBlobs map[domain.Digest][]byte

func (b cancellationBlobs) OpenContext(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	body, ok := b[digest]
	if !ok {
		return nil, errors.New("missing fixture blob")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func cancellationDigest(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func cancellationInputs(t *testing.T, spec *exec.StartSpec) exec.StageInputs {
	t.Helper()
	specBody := []byte("# Work item\nDo the thing.\n")
	promptBody := []byte("You are the specification writer.\n")
	policyBody := []byte(`[{"key":"paths","value":"daemon/**"}]`)
	vendorBody := []byte("# Host instructions\n")
	vendorDigest := cancellationDigest(vendorBody)
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest: spec.InputDigest, SpecificationDigest: cancellationDigest(specBody),
		PromptPackageDigest: cancellationDigest(promptBody), PolicyDigest: cancellationDigest(policyBody),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest: &vendorDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest, spec.PolicyDigest, spec.StageInputs = snapshot.SpecificationDigest, snapshot.PolicyDigest, &snapshot
	materializer, err := exec.NewMaterializer(cancellationBlobs{
		snapshot.SpecificationDigest: specBody, snapshot.PromptPackageDigest: promptBody,
		snapshot.PolicyDigest: policyBody, vendorDigest: vendorBody,
	}, exec.MaterializerOptions{MaxInputBytes: 1 << 20, MaxTotalBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := materializer.Materialize(t.Context(), *spec)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func TestTaskCancellationRealStageAndJournaledWard(t *testing.T) {
	fixture := ward.NewStageCancellationTestFixture(t)
	records := &cancellationRecords{}
	dir := filepath.Join(t.TempDir(), "driver")
	var logs bytes.Buffer
	newDriver := func(gate *ward.Backend) *claude.Driver {
		t.Helper()
		d, err := claude.New(claude.Config{
			Lifetime: context.Background(), Dir: dir, SeedRoot: fixture.SeedRoot,
			ExportRoot: fixture.ExportRoot, Gate: gate, Seeder: fixture,
			Exports: records, ImportStarts: records, Outcomes: records,
			Authority: cancellationAuthority{}, Artifacts: cancellationArtifacts{}, Volumes: fixture,
			Now:    func() time.Time { return time.Now().UTC() },
			Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		d.SetRecoveryLauncher(func(context.Context, domain.RunID, func(context.Context) error) error {
			return stage.ErrTaskCancelled
		})
		return d
	}
	d := newDriver(fixture.Backend)
	id := domain.InvocationID("inv-real-ward-cancellation")
	spec := exec.StartSpec{
		RunID: "run-real-ward-cancel", StageID: "specification", AttemptID: "attempt-" + domain.AttemptID(id),
		InputDigest: domain.Digest("sha256:" + strings.Repeat("11", 32)),
		Base:        fixture.Base(), Workspace: claude.WorkspaceFor(id),
		ImageRef:       domain.ImageRef("example.test/agent@sha256:" + strings.Repeat("ab", 32)),
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly, AuthIdentityID: "identity-fixture",
		AdmissionID: domain.Digest("sha256:" + strings.Repeat("44", 32)),
	}
	inputs := cancellationInputs(t, &spec)
	if err := d.StartWithInputs(t.Context(), id, spec, func(context.Context) (exec.StageInputs, error) {
		return inputs, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.Waiting:
	case <-time.After(30 * time.Second):
		inspection, inspectErr := d.Inspect(t.Context(), id)
		result, collectErr := d.Collect(t.Context(), id)
		t.Fatalf("ward writer did not enter its wait: inspection=%+v inspectErr=%v result=%+v collectErr=%v logs=%s",
			inspection, inspectErr, result, collectErr, logs.String())
	}
	if err := d.CancelAndConfirm(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// The coordinator runs StopRun again for its final inventory. The second
	// call must accept the already closed canceled journal.
	if err := d.CancelAndConfirm(t.Context(), id); err != nil {
		t.Fatalf("coordinator second StopRun: %v", err)
	}
	runID := claude.RunIDFor(id)
	fixture.AssertCanceled(t, runID)
	result, err := d.Collect(t.Context(), id)
	if err != nil || result.Status != exec.StatusCanceled {
		t.Fatalf("committed result = %+v, %v", result, err)
	}
	outcome, found, err := records.LookupExecutionOutcome(t.Context(), id)
	if err != nil || !found || outcome.Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("durable execution outcome = %+v, found=%v, err=%v", outcome, found, err)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened := newDriver(fixture.ReopenBackend(t))
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if err := reopened.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CancelAndConfirm(t.Context(), id); err != nil {
		t.Fatalf("reopened cancellation retry: %v", err)
	}
	result, err = reopened.Collect(t.Context(), id)
	if err != nil || result.Status != exec.StatusCanceled || fixture.WriterStarts(runID) != 1 {
		t.Fatalf("reopened result = %+v, err=%v, writer starts=%d", result, err, fixture.WriterStarts(runID))
	}
}
