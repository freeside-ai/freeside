package stage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestFailedWriterEvidenceCommitsLiveAndRecovers(t *testing.T) {
	body := []byte("Provider returned a diagnostic before any edit.\n")
	recovered := &ward.RecoveryResult{
		Outcome: ward.RecoveryFailed, FailureStatus: 1,
		FailureEvidence: &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)},
	}
	outcomes := newStubExports()
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) { return nil, ward.ErrWriterFailed },
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) { return recovered, nil },
	}
	d := newTestDriver(t, gate, outcomes)
	d.seeder = &recordingSeeder{}
	in := orphan(t, d, phaseSeeding, nil)
	d.runPipeline(context.Background(), in)
	got, err := d.Collect(context.Background(), testInvoke)
	if err != nil || got.Status != exec.StatusFailed || got.HeadSHA != "" || len(got.Artifacts) != 1 || len(outcomes.records) != 0 {
		t.Fatalf("result: %+v, %v", got, err)
	}
	claims, found, err := d.artifacts.LookupClaims(context.Background(), testInvoke)
	if err != nil || !found || len(claims) != 1 || claims[0].Text != nil || claims[0].Provenance.SensitivityClass != domain.SensitivitySensitive || claims[0].Metadata.MediaType != domain.EvidenceMediaTextPlain {
		t.Fatalf("claims: %+v, %v", claims, err)
	}
	if !bytes.Equal(d.artifacts.(*stubArtifacts).blobs[got.Artifacts[0]], body) {
		t.Fatal("transcript bytes changed")
	}
	// A crash after outcome persistence but before private phase persistence
	// must reconstruct the same diagnostic, without calling a provider.
	in.Phase = phaseRunning
	restored, ok, err := d.restoreDurableOutcome(context.Background(), in)
	if err != nil || !ok || len(restored.Result.Artifacts) != 1 || restored.Result.Artifacts[0] != got.Artifacts[0] {
		t.Fatalf("restored: %+v, %v, %v", restored, ok, err)
	}
	if _, err := d.failedWriterResult(context.Background(), in, recovered); err != nil {
		t.Fatalf("idempotent evidence persistence: %v", err)
	}
}

func TestFailedWriterEvidencePersistenceFailureRemainsRetryable(t *testing.T) {
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := orphan(t, d, phaseRunning, nil)
	d.artifacts.(*stubArtifacts).err = errors.New("temporary blob storage failure")
	body := []byte("diagnostic")
	_, err := d.failedWriterResult(context.Background(), in, &ward.RecoveryResult{
		Outcome: ward.RecoveryFailed, FailureStatus: 1,
		FailureEvidence: &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)},
	})
	if !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("error: %v", err)
	}
	if _, err := d.Collect(context.Background(), testInvoke); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("terminalized storage failure: %v", err)
	}
}

func TestFailedWriterRejectsConflictingEvidence(t *testing.T) {
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := orphan(t, d, phaseRunning, nil)
	for _, evidence := range []*ward.FailureEvidence{
		{Body: []byte("changed"), Digest: contentaddr.Sum([]byte("original"))},
		{Body: []byte("hidden"), Unavailable: true},
		{Body: []byte{0xff}, Digest: contentaddr.Sum([]byte{0xff})},
	} {
		if _, err := d.failedWriterResult(context.Background(), in, &ward.RecoveryResult{Outcome: ward.RecoveryFailed, FailureStatus: 1, FailureEvidence: evidence}); err == nil {
			t.Fatal("accepted invalid evidence")
		}
	}
}
