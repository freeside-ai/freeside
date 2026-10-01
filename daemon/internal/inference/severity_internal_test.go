package inference

import (
	"testing"
	"time"
)

func TestAnnotateSiteCarriesExactlyOneAuthorityContract(t *testing.T) {
	limits := Limits{Calls: 1, ComputeUnits: 1, Starvation: time.Second}
	budget := Budget{
		Window: time.Second, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 1, MaxStarvationPerRoot: time.Second,
	}
	site := AdjudicatorSite(budget)
	site.Annotation = ClassifierSite(budget).Annotation
	if err := site.validate(); err == nil {
		t.Fatal("annotate site accepted multiple authority contracts")
	}
	site.Annotation = nil
	site.DriftAudit = DriftAuditorSite(budget).DriftAudit
	if err := site.validate(); err == nil {
		t.Fatal("annotate site accepted a drift audit contract beside another")
	}
	site.Adjudication = nil
	if err := site.validate(); err != nil {
		t.Fatalf("annotate site with one drift audit contract: %v", err)
	}
	site.DriftAudit = nil
	if err := site.validate(); err == nil {
		t.Fatal("annotate site accepted no authority contract")
	}
	site = DriftAuditorSite(budget)
	site.DriftAudit.Verdicts = append(site.DriftAudit.Verdicts, "simplify")
	if err := site.validate(); err == nil {
		t.Fatal("drift audit contract accepted a verdict outside the domain")
	}
	explain := PublicationAuthorExplainSite(budget)
	explain.DriftAudit = DriftAuditorSite(budget).DriftAudit
	if err := explain.validate(); err == nil {
		t.Fatal("non-annotation site accepted a drift audit contract")
	}
}

// TestNormalizedSeverityMappings pins the shared P0-P3 normalization used to
// compare the production and shadow review arms. P0 is deliberately unmapped
// and therefore fails protective to the UnknownSeverityFallback ceiling.
func TestNormalizedSeverityMappings(t *testing.T) {
	contract := ClassifierSite(Budget{Window: time.Hour}).Annotation
	tests := []struct {
		source, native, want string
	}{
		{"codex_local", "P1", "high"},
		{"codex_local", "P2", "medium"},
		{"codex_local", "P3", "low"},
		{"codex_local", "P0", contract.UnknownSeverityFallback},
		{"claude_local", "P1", "high"},
		{"claude_local", "P2", "medium"},
		{"claude_local", "P3", "low"},
		{"claude_local", "P0", contract.UnknownSeverityFallback},
	}
	for _, test := range tests {
		t.Run(test.source+"/"+test.native, func(t *testing.T) {
			if got := contract.normalizedSeverity(test.source, test.native); got != test.want {
				t.Fatalf("normalizedSeverity(%q, %q) = %q, want %q", test.source, test.native, got, test.want)
			}
		})
	}
}
