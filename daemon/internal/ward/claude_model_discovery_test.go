package ward

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// claudeInitializeAnswer is a control_response as the pinned CLI prints it,
// cut to three models and the default pointer: one alias, one full id, and
// one model the CLI reports without effort support by omitting both fields.
const claudeInitializeAnswer = `{"type":"control_response","response":{"subtype":"success",` +
	`"request_id":"freeside-model-discovery","response":{"commands":[],"models":[` +
	`{"value":"default","resolvedModel":"claude-opus-5-5","displayName":"Default (recommended)",` +
	`"supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},` +
	`{"value":"opus","resolvedModel":"claude-opus-5-5","displayName":"Opus 5.5",` +
	`"supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},` +
	`{"value":"claude-sonnet-4-6","resolvedModel":"claude-sonnet-4-6","displayName":"Sonnet 4.6",` +
	`"supportsEffort":true,"supportedEffortLevels":["low","medium","high","max"]},` +
	`{"value":"claude-haiku-4-5-20251001","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku 4.5"}` +
	`],"account":{"tokenSource":"CLAUDE_CODE_OAUTH_TOKEN"}},"pending_permission_requests":[]}}`

// jsonEscaped spells every byte of s as a JSON \u escape, so no run of s
// appears in the raw output while decoding restores it.
func jsonEscaped(s string) string {
	var b strings.Builder
	for i := range len(s) {
		fmt.Fprintf(&b, "\\u%04x", s[i])
	}
	return b.String()
}

func initializeOutput(stdout string, status int) LineupProbeOutput {
	return LineupProbeOutput{Version: []byte("2.1.282 (Claude Code)\n"), Stdout: []byte(stdout), ExitStatus: status}
}

func wantClaudeModels() []domain.DiscoveredModel {
	return []domain.DiscoveredModel{
		{
			ID: "opus", ResolvedModel: new("claude-opus-5-5"), DisplayName: "Opus 5.5",
			EffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
		},
		{
			ID: "claude-sonnet-4-6", ResolvedModel: new("claude-sonnet-4-6"), DisplayName: "Sonnet 4.6",
			EffortLevels: []string{"low", "medium", "high", "max"},
		},
		{
			ID: "claude-haiku-4-5-20251001", ResolvedModel: new("claude-haiku-4-5-20251001"),
			DisplayName: "Haiku 4.5", EffortLevels: []string{},
		},
	}
}

func TestDecodeClaudeModelReport(t *testing.T) {
	t.Parallel()
	report, err := DecodeClaudeModelReport(initializeOutput(claudeInitializeAnswer+"\n", 0), lineupProbeTestToken(t))
	if err != nil {
		t.Fatal(err)
	}
	want := ModelReport{CLIBuild: "2.1.282 (Claude Code)", Models: wantClaudeModels()}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	catalog, err := report.Catalog("claude-a/claude_code", at)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.HarnessBuild != want.CLIBuild || !catalog.ObservedAt.Equal(at) || len(catalog.Models) != 3 {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestDecodeClaudeModelReportRefusals(t *testing.T) {
	t.Parallel()
	answer := func(models string) string {
		return `{"type":"control_response","response":{"subtype":"success","request_id":"freeside-model-discovery",` +
			`"response":{"models":[` + models + `]}}}`
	}
	model := func(value string) string {
		return `{"value":"` + value + `","displayName":"` + value + `"}`
	}
	// Bad values carry MARKER, which no error may quote.
	withToken := initializeOutput(answer(`{"value":"opus","displayName":"`+claudeEnrollmentTestToken+`"}`), 0)
	tokenInVersion := initializeOutput(claudeInitializeAnswer, 0)
	tokenInVersion.Version = []byte(claudeEnrollmentTestToken + "\n")
	for _, tc := range []struct {
		name string
		out  LineupProbeOutput
		want error
	}{
		{"no control response", initializeOutput(`{"type":"system","subtype":"init"}`, 0), ErrNoModelReport},
		{"another request's answer", initializeOutput(
			strings.Replace(claudeInitializeAnswer, "freeside-model-discovery", "other", 1), 0), ErrNoModelReport},
		{"duplicate key", initializeOutput(
			strings.Replace(claudeInitializeAnswer, `{"type":"control_response",`,
				`{"type":"control_response","type":"control_response",`, 1), 0), ErrModelReportMalformed},
		{"not JSON", initializeOutput("Error: MARKER\n"+claudeInitializeAnswer, 0), ErrModelReportMalformed},
		{"answered twice", initializeOutput(claudeInitializeAnswer+"\n"+claudeInitializeAnswer, 0), ErrModelReportMalformed},
		{"error answer", initializeOutput(
			strings.Replace(claudeInitializeAnswer, `"subtype":"success"`, `"subtype":"MARKER"`, 1), 0), ErrModelReportRefused},
		{"nonzero exit", initializeOutput(claudeInitializeAnswer, 1), ErrModelReportRefused},
		{"empty models", initializeOutput(answer(""), 0), domain.ErrNoDiscoveredModels},
		{"only the default pointer", initializeOutput(answer(model("default")), 0), domain.ErrNoDiscoveredModels},
		{"duplicate value", initializeOutput(answer(model("MARKER")+","+model("MARKER")), 0), domain.ErrDuplicate},
		{
			"duplicate effort level", initializeOutput(answer(
				`{"value":"opus","displayName":"Opus","supportsEffort":true,"supportedEffortLevels":["MARKER","MARKER"]}`), 0),
			domain.ErrDuplicate,
		},
		{"token in a model", withToken, ErrModelReportCarriesCredential},
		{"token in the version", tokenInVersion, ErrModelReportCarriesCredential},
		{
			"escaped token in a display name", initializeOutput(answer(
				`{"value":"opus","displayName":"`+jsonEscaped(claudeEnrollmentTestToken)+`"}`), 0),
			ErrModelReportCarriesCredential,
		},
		{"option-like id", initializeOutput(answer(model("--MARKER")), 0), ErrModelReportMalformed},
		{"id with a space", initializeOutput(answer(model("opus MARKER")), 0), ErrModelReportMalformed},
		{
			"effort level with a space", initializeOutput(answer(
				`{"value":"opus","displayName":"Opus","supportsEffort":true,"supportedEffortLevels":["high MARKER"]}`), 0),
			ErrModelReportMalformed,
		},
		{"resolved model with a space", initializeOutput(answer(
			`{"value":"opus","resolvedModel":"claude MARKER","displayName":"Opus"}`), 0), ErrModelReportMalformed},
		{"display name with a newline", initializeOutput(answer(
			`{"value":"opus","displayName":"Opus\nMARKER"}`), 0), ErrModelReportMalformed},
		{"oversized display name", initializeOutput(answer(
			`{"value":"opus","displayName":"`+strings.Repeat("M", 257)+`MARKER"}`), 0), ErrModelReportMalformed},
		{"empty value", initializeOutput(answer(model("")), 0), domain.ErrEmptyField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeClaudeModelReport(tc.out, lineupProbeTestToken(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "MARKER") || strings.Contains(err.Error(), claudeEnrollmentTestToken[20:]) {
				t.Fatalf("err %q quotes the CLI's output", err)
			}
		})
	}
}

// TestDiscoverClaudeModelsRunsOneContainedCall drives discovery through the
// fake runtime: the credential is staged and gone, nothing is left behind,
// and an answer past the prompt probe's 64 KiB cap is read whole.
func TestDiscoverClaudeModelsRunsOneContainedCall(t *testing.T) {
	t.Parallel()
	padding := `{"type":"system","subtype":"status","padding":"` + strings.Repeat("x", 80<<10) + `"}` + "\n"
	rt := newLineupProbeFakeRuntime(t,
		lineupProbeOutputFiles("2.1.282 (Claude Code)\n", padding+claudeInitializeAnswer+"\n", "", "0\n")...)
	token := lineupProbeTestToken(t)
	var credential []byte
	rt.onCopyIntoContainer = func(_, hostDir, targetDir string) error {
		if targetDir == lineupProbeStageDir {
			body, err := os.ReadFile(filepath.Join(hostDir, lineupProbeCredName)) //nolint:gosec // G304: the stage directory the probe under test hands the fake runtime
			if err != nil {
				return err
			}
			credential = body
		}
		return nil
	}
	var spec ContainerSpec
	rt.onCreateContainer = func(s ContainerSpec) error { spec = s; return nil }

	report, err := DiscoverClaudeModels(t.Context(), rt, lineupProbeTestAgentImage, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Models, wantClaudeModels()) {
		t.Fatalf("models = %+v", report.Models)
	}
	if string(credential) != string(token.Reveal()) {
		t.Fatal("the container received a different credential")
	}
	if spec.Image != lineupProbeTestAgentImage || !strings.Contains(spec.Command[2], "--input-format stream-json") {
		t.Fatalf("spec = %+v", spec)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.ctrs) != 0 || len(rt.nets) != 0 {
		t.Fatalf("left %d containers and %d networks behind", len(rt.ctrs), len(rt.nets))
	}
}

func TestDiscoverClaudeModelsRefusesAnExportStillHoldingTheCredential(t *testing.T) {
	t.Parallel()
	files := append(lineupProbeOutputFiles("2.1.282 (Claude Code)\n", claudeInitializeAnswer, "", "0\n"),
		tarFile{lineupProbeStageDir + "/" + lineupProbeCredName, []byte("token")})
	rt := newLineupProbeFakeRuntime(t, files...)
	_, err := DiscoverClaudeModels(t.Context(), rt, lineupProbeTestAgentImage, lineupProbeTestToken(t), nil)
	if err == nil || !strings.Contains(err.Error(), "still holds the credential") {
		t.Fatalf("err = %v", err)
	}
}

// TestLineupProbeKeepsItsOutputCap: only an initialize probe gets the raised
// cap; a prompt probe's oversized stdout is still refused.
func TestLineupProbeKeepsItsOutputCap(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", lineupProbeOutputCap+1)
	rt := newLineupProbeFakeRuntime(t, lineupProbeOutputFiles("v\n", big, "", "0\n")...)
	if _, err := ProbeLineupImage(t.Context(), rt, claudeWriterProbe(t), nil); err == nil ||
		!strings.Contains(err.Error(), "within the cap") {
		t.Fatalf("err = %v", err)
	}
}
