package signet_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// TestContractDigestMatchesSpec is the daemon-side drift gate: the
// compiled-in APIContractDigest must equal the SHA-256 of the spec's exact
// bytes, so a spec edit that forgets to regenerate the constant fails here
// (daemon-ci.yml runs on any daemon/** change).
func TestContractDigestMatchesSpec(t *testing.T) {
	body, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if signet.APIContractDigest != want {
		t.Fatalf("APIContractDigest = %s, want %s (run scripts/api-contract-digest.sh --write)",
			signet.APIContractDigest, want)
	}
}

// TestHealthResponseGolden pins the /health wire shape. The digest is a
// fixed placeholder so the golden does not churn on every spec change; the
// real digest is gated by TestContractDigestMatchesSpec instead.
func TestHealthResponseGolden(t *testing.T) {
	fixture := signet.HealthResponse{
		Status:         "ok",
		ContractDigest: "sha256:" + strings.Repeat("0", 64),
		Version:        "0.1.0",
		StartedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	got, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "health", got)
}
