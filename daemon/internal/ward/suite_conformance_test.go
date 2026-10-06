package ward

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// recordingConformance is the test ConformanceRecorder: it captures every
// record and can fail on demand.
type recordingConformance struct {
	records []domain.BackendConformance
	err     error
}

func (r *recordingConformance) RecordBackendConformance(
	_ context.Context, record domain.BackendConformance,
) error {
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, record)
	return nil
}

// TestSuiteFullRecordsPassedConformance: a green Full declares both
// suite-earned capabilities and hands the recorder one passed record carrying
// the explicit proven set, unpersisted (generation zero, store-assigned).
func TestSuiteFullRecordsPassedConformance(t *testing.T) {
	s, rt := newSuiteTest(t)
	rec := &recordingConformance{}
	WithConformanceRecorder(rec)(s)
	scriptHappyProbes(s, rt)
	if err := s.Full(context.Background()); err != nil {
		t.Fatalf("Full = %v, want nil", err)
	}
	for _, c := range []exec.Capability{exec.CapNetworklessExport, exec.CapEnforcedProviderEgress} {
		if !s.b.Capabilities().Has(c) {
			t.Errorf("successful Full did not declare %q", c)
		}
	}
	if len(rec.records) != 2 {
		t.Fatalf("recorder got %d records, want the superseding marker then the pass", len(rec.records))
	}
	if marker := rec.records[0]; marker.Outcome != domain.ConformanceSuperseded || marker.Capabilities != nil {
		t.Errorf("first record = %+v, want a bare superseding marker", marker)
	}
	got := rec.records[1]
	if got.Backend != domain.BackendFreshVMReadOnlyVolumeHandoff || got.Outcome != domain.ConformancePassed {
		t.Errorf("record = %+v, want a passed fresh-vm record", got)
	}
	if want := domain.NewCapabilitySnapshot(provenCapabilities()...); !slices.Equal(got.Capabilities, want) {
		t.Errorf("recorded capabilities = %v, want %v", got.Capabilities, want)
	}
	if got.Generation != 0 {
		t.Errorf("recorded generation = %d, want 0 (store-assigned)", got.Generation)
	}
	if got.ProvedAt.IsZero() {
		t.Error("recorded proved_at is zero")
	}
}

// TestSuiteFullRecordsFailedConformance: a failed pass records the failure
// with nil capabilities and drops both declarations — the pass is
// all-or-nothing, so any check failure withdraws every suite-earned
// capability, the egress declaration included.
func TestSuiteFullRecordsFailedConformance(t *testing.T) {
	s, rt := newSuiteTest(t)
	rec := &recordingConformance{}
	WithConformanceRecorder(rec)(s)
	exporter := namesFor("conf-run").Exporter
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == exporter {
			rep.Mounts = append(rep.Mounts, Mount{Type: MountBind, Source: "/etc", Target: "/etc", ReadOnly: true})
		}
		return rep, nil
	}
	if err := s.Full(context.Background()); err == nil {
		t.Fatal("Full = nil, want failure")
	}
	for _, c := range []exec.Capability{exec.CapNetworklessExport, exec.CapEnforcedProviderEgress} {
		if s.b.Capabilities().Has(c) {
			t.Errorf("failed Full retained %q", c)
		}
	}
	if len(rec.records) != 2 {
		t.Fatalf("recorder got %d records, want the superseding marker then the failure", len(rec.records))
	}
	got := rec.records[1]
	if got.Outcome != domain.ConformanceFailed || got.Capabilities != nil {
		t.Errorf("record = %+v, want failed with nil capabilities", got)
	}
}

// TestSuiteFullRecorderFailureFailsThePass: an unpersisted proof is not a
// proof. When the recorder cannot durably record a green pass, Full fails and
// the in-memory declaration is withdrawn, so the backend cannot declare a
// capability the store could not gate an admission against.
func TestSuiteFullRecorderFailureFailsThePass(t *testing.T) {
	s, rt := newSuiteTest(t)
	rec := &recordingConformance{err: errors.New("store unavailable")}
	WithConformanceRecorder(rec)(s)
	scriptHappyProbes(s, rt)
	err := s.Full(context.Background())
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("Full = %v, want the recorder failure", err)
	}
	for _, c := range []exec.Capability{exec.CapNetworklessExport, exec.CapEnforcedProviderEgress} {
		if s.b.Capabilities().Has(c) {
			t.Errorf("unrecorded pass still declares %q", c)
		}
	}
}

// TestSuiteFullRegistryHandoffFailureWithholdsCapabilities: the provider_only
// handoff passing does not earn supports_enforced_provider_egress by itself.
// A provider_registry handoff whose proxy does not realize the declared
// registry fails the pass, so no capability is declared or recorded.
func TestSuiteFullRegistryHandoffFailureWithholdsCapabilities(t *testing.T) {
	s, rt := newSuiteTest(t)
	rec := &recordingConformance{}
	WithConformanceRecorder(rec)(s)
	scriptHappyProbes(s, rt)
	s.b.cfg.readEgressAllowlist = func(p *connectProxy) ([]string, []string) {
		providers, _ := p.Allowlist()
		return providers, nil
	}

	wantCheckFailure(t, s.Full(context.Background()), CheckAgentEgress)
	if !slices.Contains(rt.calls, "create-container "+namesFor(s.fx.RunID).Exporter) {
		t.Error("the provider_only handoff did not run to its export before the registry handoff failed")
	}
	if slices.Contains(rt.calls, "create-container "+namesFor(s.fx.RegistryRunID).Agent) {
		t.Error("the provider_registry writer started behind a proxy missing its registry")
	}
	for _, c := range conformancePendingCapabilities {
		if s.b.Capabilities().Has(c) {
			t.Errorf("Full declared %q without the provider_registry proof", c)
		}
	}
	if len(rec.records) != 2 || rec.records[1].Outcome != domain.ConformanceFailed {
		t.Fatalf("recorder = %+v, want the marker then the failed record", rec.records)
	}
	s.assertReaped(t, rt)
}

// TestSuiteFullPanicRecordsFailure: the publish defer's panic path records
// the failure (best-effort) and still re-raises.
func TestSuiteFullPanicRecordsFailure(t *testing.T) {
	s, rt := newSuiteTest(t)
	rec := &recordingConformance{}
	WithConformanceRecorder(rec)(s)
	scriptHappyProbes(s, rt)
	rt.onCreateVolume = func(string) error { panic("runtime imploded") }
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("Full swallowed the panic")
			}
		}()
		_ = s.Full(context.Background())
	}()
	if len(rec.records) != 2 || rec.records[1].Outcome != domain.ConformanceFailed {
		t.Fatalf("recorder after panic = %+v, want the marker then the failed record", rec.records)
	}
	if s.b.Capabilities().Has(exec.CapEnforcedProviderEgress) {
		t.Error("panicked Full still declares supports_enforced_provider_egress")
	}
}
