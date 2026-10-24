package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestRecordOutcomeCarriesFailureCause pins how a failed result's cause
// reaches the durable outcome: a driver-reported cause is recorded as is, and
// a failed result without one records unknown.
func TestRecordOutcomeCarriesFailureCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status exec.Status
		cause  domain.FailureCause
		want   domain.FailureCause
	}{
		{"failed with a cause", exec.StatusFailed, domain.FailureCauseQuota, domain.FailureCauseQuota},
		{"failed without a cause", exec.StatusFailed, "", domain.FailureCauseUnknown},
		{"canceled", exec.StatusCanceled, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			exports := newStubExports()
			d := newTestDriver(t, &stubGate{}, exports)
			in := intent{InvocationID: testInvoke, Spec: testStartSpec()}
			result := exec.StageResult{
				InvocationID: testInvoke, Status: tc.status, Summary: "stage ended", Cause: tc.cause,
			}
			if err := d.recordOutcome(ctx, in, result); err != nil {
				t.Fatalf("recordOutcome: %v", err)
			}
			outcome := exports.outcomes[testInvoke]
			if outcome.Cause != tc.want {
				t.Fatalf("recorded cause = %q, want %q", outcome.Cause, tc.want)
			}
			if err := outcome.Validate(); err != nil {
				t.Fatalf("recorded outcome invalid: %v", err)
			}
			if err := d.authenticateOutcome(ctx, in, result); err != nil {
				t.Fatalf("authenticateOutcome: %v", err)
			}
		})
	}
}

// TestRecordOutcomeConvergesOnCauselessStoredOutcome covers the upgrade: a
// failed outcome stored before causes existed has none, and a replay of the
// same failed result must converge on it rather than refuse as a changed
// write-once record.
func TestRecordOutcomeConvergesOnCauselessStoredOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	in := intent{InvocationID: testInvoke, Spec: testStartSpec()}
	exports.outcomes[testInvoke] = domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: in.Spec.AdmissionID,
		Status: domain.ExecutionOutcomeFailed, Summary: "stage ended", RecordedAt: fixedNow,
	}
	result := exec.StageResult{InvocationID: testInvoke, Status: exec.StatusFailed, Summary: "stage ended"}
	if err := d.recordOutcome(ctx, in, result); err != nil {
		t.Fatalf("replay against a causeless stored outcome: %v", err)
	}
	if err := d.authenticateOutcome(ctx, in, result); err != nil {
		t.Fatalf("authenticate against a causeless stored outcome: %v", err)
	}

	// A positive cause still disagrees with the unknown one on record.
	result.Cause = domain.FailureCauseQuota
	if err := d.recordOutcome(ctx, in, result); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("replay with a different cause = %v, want ErrImmutableTransition", err)
	}
}

// TestRecoverExportedKeepsRecordedFailureCause covers a crash after a failed
// outcome with a positive cause was recorded but before its phase commit:
// recovery rebuilds the result with that cause, so the result still
// authenticates against the outcome on record.
func TestRecoverExportedKeepsRecordedFailureCause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	if err := exports.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		Status: domain.ExecutionOutcomeFailed, Summary: "quota exhausted",
		Cause: domain.FailureCauseQuota, RecordedAt: fixedNow,
	}); err != nil {
		t.Fatalf("seed prior failed outcome: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-cause"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})

	for pass := range 2 {
		if err := d.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile pass %d: %v", pass+1, err)
		}
	}
	in, err := d.loadIntent(ctx, testInvoke)
	if err != nil || in.Result == nil {
		t.Fatalf("recovered intent = %+v (err=%v)", in, err)
	}
	if in.Result.Cause != domain.FailureCauseQuota {
		t.Fatalf("recovered result cause = %q, want quota", in.Result.Cause)
	}
	if err := d.authenticateOutcome(ctx, in, *in.Result); err != nil {
		t.Fatalf("recovered result does not authenticate: %v", err)
	}
}

// TestAdoptRecordedOutcomeKeepsFailureCause is the export-recovery twin of
// the test above: adopting an already-written failed outcome commits a result
// with the outcome's cause.
func TestAdoptRecordedOutcomeKeepsFailureCause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-adopt-cause"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	outcome := domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		Status: domain.ExecutionOutcomeFailed, Summary: "quota exhausted",
		Cause: domain.FailureCauseQuota, RecordedAt: fixedNow,
	}
	if err := d.adoptRecordedOutcome(ctx, in, outcome); err != nil {
		t.Fatalf("adoptRecordedOutcome: %v", err)
	}
	adopted, err := d.loadIntent(ctx, testInvoke)
	if err != nil || adopted.Result == nil {
		t.Fatalf("adopted intent = %+v (err=%v)", adopted, err)
	}
	if adopted.Result.Cause != domain.FailureCauseQuota {
		t.Fatalf("adopted result cause = %q, want quota", adopted.Result.Cause)
	}
}

// TestPipelineClassifiesOnlyTheHandoffRefusal pins the driver's one
// classifier (#2210): a handoff spec the driver refuses to build for an
// admitted start records invalid_configuration, and a writer that exits
// nonzero still records unknown, since its transcript is untrusted text.
func TestPipelineClassifiesOnlyTheHandoffRefusal(t *testing.T) {
	t.Parallel()
	refused := func(d *Driver) {
		// The test provider refuses a start with no auth identity.
		d.provider = testProvider{
			volumes:            stubVolumes{volume: testAuthVol},
			handoffInputMutate: func(in *ProviderHandoffInput) { in.Spec.AuthIdentityID = "" },
		}
	}
	writerExit := func(d *Driver) {
		d.gate = &stubGate{
			handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) { return nil, ward.ErrWriterFailed },
			recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
				return &ward.RecoveryResult{Outcome: ward.RecoveryFailed, FailureStatus: 1}, nil
			},
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(*Driver)
		want  domain.FailureCause
	}{
		{"refused handoff spec", refused, domain.FailureCauseInvalidConfiguration},
		{"writer exit", writerExit, domain.FailureCauseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			outcomes := newStubExports()
			d := newTestDriver(t, &stubGate{}, outcomes)
			d.seeder = &recordingSeeder{}
			in := orphan(t, d, phaseSeeding, nil)
			tc.setup(d)
			d.runPipeline(ctx, in)
			got, err := d.Collect(ctx, testInvoke)
			if err != nil || got.Status != exec.StatusFailed {
				t.Fatalf("result = %+v, %v; want failed", got, err)
			}
			if outcome := outcomes.outcomes[testInvoke]; outcome.Cause != tc.want {
				t.Fatalf("recorded cause = %q, want %q", outcome.Cause, tc.want)
			}
		})
	}
}
