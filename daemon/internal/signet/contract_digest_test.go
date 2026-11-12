package signet_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// TestContractDigestMatchesSpec is the daemon-side drift gate: the
// compiled-in APIContractDigest must equal the SHA-256 of the spec's exact
// bytes, so a spec edit that forgets to regenerate the constant fails here
// (daemon-ci.yml runs on any daemon/** change).
func TestContractDigestMatchesSpec(t *testing.T) {
	t.Parallel()
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

// TestContractCompatibilityMatchesSpec is the drift gate for the
// compatibility constants /health reports: each must equal its line under
// the spec's info, so a level or floor edit that forgets to regenerate
// fails here as the digest does.
func TestContractCompatibilityMatchesSpec(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Info struct {
			Level *int `yaml:"x-compatibility-level"`
			Floor *int `yaml:"x-compatibility-floor"`
		} `yaml:"info"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if document.Info.Level == nil || document.Info.Floor == nil {
		t.Fatal("api/openapi.yaml info lacks x-compatibility-level or x-compatibility-floor")
	}
	if signet.APIContractCompatibilityLevel != *document.Info.Level ||
		signet.APIContractCompatibilityFloor != *document.Info.Floor {
		t.Fatalf("compiled-in level %d, floor %d; spec level %d, floor %d (run scripts/api-contract-digest.sh --write)",
			signet.APIContractCompatibilityLevel, signet.APIContractCompatibilityFloor,
			*document.Info.Level, *document.Info.Floor)
	}
}

// TestContractCompatibilityFloorWithinLevel fails a build whose floor
// exceeds its level: such a daemon would refuse an app built from its own
// spec. scripts/api-contract-digest.sh refuses to write one; this holds the
// rule if the constants are edited by hand.
func TestContractCompatibilityFloorWithinLevel(t *testing.T) {
	t.Parallel()
	if signet.APIContractCompatibilityFloor > signet.APIContractCompatibilityLevel {
		t.Fatalf("APIContractCompatibilityFloor %d exceeds APIContractCompatibilityLevel %d",
			signet.APIContractCompatibilityFloor, signet.APIContractCompatibilityLevel)
	}
}

// TestHealthResponseGolden pins the /health wire shape. The digest, level,
// and floor are fixed placeholders so the golden does not churn on every
// spec change; the real values are gated by TestContractDigestMatchesSpec
// and TestContractCompatibilityMatchesSpec instead. The level and floor
// differ so the golden shows which field carries which.
func TestHealthResponseGolden(t *testing.T) {
	t.Parallel()
	fixture := signet.HealthResponse{
		Status:             "ok",
		ContractDigest:     "sha256:" + strings.Repeat("0", 64),
		CompatibilityLevel: 1,
		CompatibilityFloor: 0,
		Features:           []domain.DaemonFeature{domain.DaemonFeatureParkedTaskMove},
		Version:            "0.1.0",
		StartedAt:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	got, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "health", got)
}
