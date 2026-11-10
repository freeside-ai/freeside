package ward

import (
	"errors"
	"testing"
)

// TestAttestHostOnlyNetwork pins the one host-only network check the
// provider egress, the lineup probe, and the Codex review observation
// share: every refusal is a ConformanceFailure for CheckAgentEgress, and a
// good report returns its creation date as the fingerprint.
func TestAttestHostOnlyNetwork(t *testing.T) {
	t.Parallel()
	owner := testOwnershipLabel()
	const name = "freeside-test-egress"
	good := NetworkReport{
		NetworkSummary: NetworkSummary{
			Name: name, Mode: NetworkHostOnly,
			Labels: []Label{owner}, LabelsObserved: true, CreationDate: "2026-11-09T12:00:00Z",
		},
		IPv4Gateway: "192.168.64.1", IPv4Subnet: "192.168.64.0/24",
	}
	for _, tc := range []struct {
		name   string
		change func(*NetworkReport)
	}{
		{"wrong name", func(r *NetworkReport) { r.Name = "freeside-other-egress" }},
		{"NAT mode", func(r *NetworkReport) { r.Mode = NetworkNAT }},
		{"labels not observed", func(r *NetworkReport) { r.LabelsObserved = false }},
		{"missing ownership label", func(r *NetworkReport) { r.Labels = runLabels("run-1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := good
			report.Labels = append([]Label(nil), good.Labels...)
			tc.change(&report)
			fingerprint, err := attestHostOnlyNetwork(report, name, owner, "test")
			var cf *ConformanceFailure
			if !errors.As(err, &cf) || cf.Check != CheckAgentEgress {
				t.Fatalf("attestHostOnlyNetwork = %v, want a %s conformance failure", err, CheckAgentEgress)
			}
			if fingerprint != "" {
				t.Fatalf("refusal returned fingerprint %q", fingerprint)
			}
		})
	}
	fingerprint, err := attestHostOnlyNetwork(good, name, owner, "test")
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != good.CreationDate {
		t.Fatalf("fingerprint = %q, want the creation date %q", fingerprint, good.CreationDate)
	}
}
