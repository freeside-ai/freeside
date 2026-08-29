package engine

import (
	"os"
	"strings"
	"testing"
)

// TestProductionRetryGatesUseAuthenticatedConclusion pins both retry doors to
// the resolution-aware classifier. The first allocates a new attempt; the
// second authenticates an interrupted allocation before its run is created.
func TestProductionRetryGatesUseAuthenticatedConclusion(t *testing.T) {
	for _, path := range []string{"production_attempt.go", "production_workflow.go"} {
		body, err := os.ReadFile(path) //nolint:gosec // fixed package-local source file
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, "domain.ConcludeRun(") {
			t.Fatalf("%s still uses milestone-only run conclusion", path)
		}
		if strings.Count(text, "AuthenticatedProductionRunConclusion(") != 1 {
			t.Fatalf("%s authenticated conclusion call count changed", path)
		}
	}
}
