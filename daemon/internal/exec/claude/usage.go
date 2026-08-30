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

var _ stage.UsageExtractor = claudeProvider{}

// ExtractUsage reads numbers-only telemetry from the last Claude terminal
// result envelope. Missing or malformed telemetry returns nil.
func ExtractUsage(transcript []byte, observedAt time.Time) []exec.UsageMeasurement {
	return exec.ExtractClaudeUsage(
		transcript, observedAt, domain.UsageSourceAdapterTranscript,
		ward.RejectDuplicateJSONKeys,
	)
}

func (claudeProvider) ExtractUsage(
	evidenceDir string,
	evidence export.EvidenceManifest,
	observedAt time.Time,
) ([]exec.UsageMeasurement, error) {
	for _, entry := range evidence.Entries {
		if entry.Label != "agent-transcript" {
			continue
		}
		hexDigits, ok := contentaddr.Parse(string(entry.Digest))
		if !ok {
			return nil, fmt.Errorf("agent transcript digest %q is not canonical", entry.Digest)
		}
		path := filepath.Join(evidenceDir, "sha256", hexDigits)
		body, err := os.ReadFile(path) //nolint:gosec // verified evidence root and content-addressed entry
		if err != nil {
			return nil, fmt.Errorf("read agent transcript: %w", err)
		}
		if int64(len(body)) != entry.Size {
			return nil, fmt.Errorf("agent transcript size %d, manifest records %d", len(body), entry.Size)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != hexDigits {
			return nil, fmt.Errorf("agent transcript does not match digest %s", entry.Digest)
		}
		return ExtractUsage(body, observedAt), nil
	}
	return nil, nil
}
