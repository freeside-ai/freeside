package inference

import (
	"testing"
	"time"
)

// TestNormalizedSeverityMapsUnmappedP0ToFallback pins a consequence of the
// P0-P3 review finding severity domain (#679): the classifier's native mapping
// table covers only p1-p3, so a P0 finding is unmapped and normalizes to the
// UnknownSeverityFallback ceiling rather than silently dropping below it.
func TestNormalizedSeverityMapsUnmappedP0ToFallback(t *testing.T) {
	contract := ClassifierSite(Budget{Window: time.Hour}).Annotation
	if got := contract.normalizedSeverity("codex_local", "P0"); got != contract.UnknownSeverityFallback {
		t.Fatalf("normalizedSeverity(P0) = %q, want fallback %q", got, contract.UnknownSeverityFallback)
	}
	// Control: a mapped severity resolves through the table, not the fallback.
	if got := contract.normalizedSeverity("codex_local", "P1"); got != "high" {
		t.Fatalf("normalizedSeverity(P1) = %q, want high", got)
	}
}
