package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var (
	_ stage.UsageExtractor         = claudeProvider{}
	_ stage.QuotaRefusalClassifier = claudeProvider{}
)

// ClassifyQuotaRefusal reads a failed writer's transcript for the provider's
// refusal of one quota window. Anything else, or any doubt, returns nil.
func (claudeProvider) ClassifyQuotaRefusal(transcript []byte) *domain.QuotaRefusal {
	return exec.ClassifyClaudeQuotaRefusal(transcript, ward.RejectDuplicateJSONKeys)
}

// ExtractUsage reads numbers-only telemetry from the last Claude terminal
// result envelope. Missing or malformed telemetry returns nil.
func ExtractUsage(transcript []byte, observedAt time.Time) []exec.UsageMeasurement {
	return exec.ExtractClaudeUsage(
		transcript, observedAt, domain.UsageSourceAdapterTranscript,
		ward.RejectDuplicateJSONKeys,
	)
}

// ExtractUsageCapture reads the writer turn's provider windows from a
// transcript. Missing, refused, or malformed answers return nil.
func ExtractUsageCapture(transcript []byte, observedAt time.Time) *exec.ClaudeUsageCapture {
	return exec.ExtractClaudeUsageCapture(transcript, observedAt, ward.RejectDuplicateJSONKeys)
}

// ExtractUsage reads both from the one verified agent transcript, so the
// capture and the measurements describe the same bytes.
func (claudeProvider) ExtractUsage(
	evidenceDir string,
	evidence export.EvidenceManifest,
	observedAt time.Time,
) ([]exec.UsageMeasurement, *exec.ClaudeUsageCapture, error) {
	for _, entry := range evidence.Entries {
		if entry.Label != "agent-transcript" {
			continue
		}
		hexDigits, ok := contentaddr.Parse(string(entry.Digest))
		if !ok {
			return nil, nil, fmt.Errorf("agent transcript digest %q is not canonical", entry.Digest)
		}
		path := filepath.Join(evidenceDir, "sha256", hexDigits)
		body, err := os.ReadFile(path) //nolint:gosec // verified evidence root and content-addressed entry
		if err != nil {
			return nil, nil, fmt.Errorf("read agent transcript: %w", err)
		}
		if int64(len(body)) != entry.Size {
			return nil, nil, fmt.Errorf("agent transcript size %d, manifest records %d", len(body), entry.Size)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != hexDigits {
			return nil, nil, fmt.Errorf("agent transcript does not match digest %s", entry.Digest)
		}
		return ExtractUsage(body, observedAt), ExtractUsageCapture(body, observedAt), nil
	}
	return nil, nil, nil
}
