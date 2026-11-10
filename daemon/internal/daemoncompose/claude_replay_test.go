package daemoncompose

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// TestProductionReplayCarriesEveryDriverField pins that the export recorder
// hands the engine every field of the driver's replay, under the same name.
// The Claude usage capture was once set by the driver and dropped here
// silently (#2716). A field added to either struct fails this test until the
// mapping and the fixture below carry it.
func TestProductionReplayCarriesEveryDriverField(t *testing.T) {
	t.Parallel()
	evidenceDigest := domain.Digest("sha256:evidence")
	planDigest := domain.Digest("sha256:plan")
	observedAt := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	driver := claude.ExecutionReplay{
		InvocationID: "inv-replay", ObservedBaseSHA: "base", HeadSHA: "head",
		Manifest: export.Manifest{Version: "1"}, ManifestDigest: "sha256:manifest",
		Evidence:               export.EvidenceManifest{Version: export.EvidenceManifestVersion},
		EvidenceManifestDigest: &evidenceDigest, CommitPlanDigest: &planDigest,
		ImportOptions: importer.Options{BaseSHA: "base"},
		Usage: []exec.UsageMeasurement{{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "input_tokens", Unit: "tokens", Quantity: 3, Sequence: 1, ObservedAt: observedAt,
		}},
		ClaudeUsage: &exec.ClaudeUsageCapture{ObservedAt: observedAt},
	}
	engineReplay := productionReplayFrom(driver)
	from, to := reflect.ValueOf(driver), reflect.ValueOf(engineReplay)
	if from.NumField() != to.NumField() {
		t.Fatalf("driver replay has %d fields, engine replay %d", from.NumField(), to.NumField())
	}
	for i := range from.NumField() {
		name := from.Type().Field(i).Name
		if from.Field(i).IsZero() {
			t.Fatalf("fixture leaves %s zero, so the test cannot see it carried", name)
		}
		carried := to.FieldByName(name)
		if !carried.IsValid() || !reflect.DeepEqual(carried.Interface(), from.Field(i).Interface()) {
			t.Fatalf("engine replay %s = %v, want the driver's %v", name, carried, from.Field(i))
		}
	}
}
