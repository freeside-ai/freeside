package ward

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

//nolint:gosec // G101 false positive: fixture tokens, not credentials.
const (
	codexModelListTestAccessToken = "codex-access-token-0123456789abcdefghijklmnop"
	codexModelListTestIDToken     = "codex-id-token-ZYXWVUTSRQPONMLKJIHGFEDCBA"
)

func codexModelListCredential() LineupProbeCredential {
	return LineupProbeCredential{
		mode: CodexAuthSubscription,
		body: []byte(`{"tokens":{"id_token":"` + codexModelListTestIDToken + `","access_token":"` + codexModelListTestAccessToken + `","refresh_token":""}}`),
	}
}

// codexModelListTranscript is an app-server session as the probe sees it:
// the initialize answer, a notification, a server-sent request that reuses
// a page's id, and two model/list pages answered out of order. The models
// are cut from a hand run of codex-cli 0.162.1.
var codexModelListTranscript = strings.Join([]string{
	`{"id":1,"result":{"userAgent":"freeside-model-discovery/0.162.1","platformFamily":"unix","platformOs":"linux"}}`,
	`{"method":"remoteControl/status/changed","params":{"status":"disabled"}}`,
	`{"id":3,"result":{"data":[` +
		`{"id":"gpt-6-astra","model":"gpt-6-astra","displayName":"GPT-6 Astra","hidden":false,"isDefault":false,` +
		`"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"l"}]},` +
		`{"id":"codex-auto","model":"codex-auto","displayName":"Codex Auto","hidden":false,"isDefault":false,` +
		`"defaultReasoningEffort":"medium","supportedReasoningEfforts":[]}` +
		`],"nextCursor":null}}`,
	`{"id":3,"method":"item/tool/requestUserInput","params":{}}`,
	`{"id":2,"result":{"data":[` +
		`{"id":"gpt-6.1-sol","model":"gpt-6.1-sol","displayName":"GPT-6.1 Sol","hidden":false,"isDefault":true,` +
		`"defaultReasoningEffort":"medium","supportedReasoningEfforts":[` +
		`{"reasoningEffort":"low","description":"l"},{"reasoningEffort":"medium","description":"m"},` +
		`{"reasoningEffort":"high","description":"h"},{"reasoningEffort":"xhigh","description":"x"},` +
		`{"reasoningEffort":"max","description":"m"},{"reasoningEffort":"ultra","description":"u"}]}` +
		`],"nextCursor":"1"}}`,
}, "\n") + "\n"

func codexModelListOutput(stdout string, status int) LineupProbeOutput {
	return LineupProbeOutput{Version: []byte("codex-cli 0.162.1\n"), Stdout: []byte(stdout), ExitStatus: status}
}

func wantCodexModels() []domain.DiscoveredModel {
	return []domain.DiscoveredModel{
		{ID: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol", EffortLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{ID: "gpt-6-astra", DisplayName: "GPT-6 Astra", EffortLevels: []string{"low"}},
		{ID: "codex-auto", DisplayName: "Codex Auto", EffortLevels: []string{}},
	}
}

func TestDecodeCodexModelReport(t *testing.T) {
	t.Parallel()
	report, err := DecodeCodexModelReport(codexModelListOutput(codexModelListTranscript, 0), codexModelListCredential())
	if err != nil {
		t.Fatal(err)
	}
	want := ModelReport{CLIBuild: "codex-cli 0.162.1", Models: wantCodexModels()}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	for _, model := range report.Models {
		if model.ResolvedModel != nil {
			t.Fatalf("model %s names a resolved model", model.ID)
		}
	}
}

func TestDecodeCodexModelReportRefusals(t *testing.T) {
	t.Parallel()
	const initialize = `{"id":1,"result":{}}`
	page := func(id, models, cursor string) string {
		return `{"id":` + id + `,"result":{"data":[` + models + `],"nextCursor":` + cursor + `}}`
	}
	model := func(id string) string {
		return `{"id":"` + id + `","model":"` + id + `","displayName":"` + id + `","supportedReasoningEfforts":[]}`
	}
	session := func(lines ...string) LineupProbeOutput {
		return codexModelListOutput(strings.Join(lines, "\n"), 0)
	}
	one := page("2", model("gpt-6.1-sol"), "null")
	tokenInVersion := session(initialize, one)
	tokenInVersion.Version = []byte(codexModelListTestAccessToken + "\n")
	nonzero := session(initialize, one)
	nonzero.ExitStatus = 1
	// Bad values carry MARKER, which no error may quote.
	for _, tc := range []struct {
		name string
		out  LineupProbeOutput
		want error
	}{
		{"no initialize answer", session(one), ErrNoModelReport},
		{"only notifications", session(`{"method":"MARKER"}`), ErrNoModelReport},
		{"initialize error", session(`{"id":1,"error":{"code":-32600,"message":"MARKER"}}`, one), ErrModelReportRefused},
		{"model/list error", session(initialize, `{"id":2,"error":{"code":-32600,"message":"MARKER"}}`), ErrModelReportRefused},
		{"initialize without a result", session(`{"id":1}`, one), ErrModelReportMalformed},
		{"no page", session(initialize), ErrModelReportIncomplete},
		{
			"missing page", session(initialize, page("2", model("a"), `"1"`), page("4", model("b"), "null")),
			ErrModelReportIncomplete,
		},
		{"last page carries a cursor", session(initialize, page("2", model("a"), `"MARKER"`)), ErrModelReportIncomplete},
		{
			"middle page ends the list", session(initialize, page("2", model("a"), "null"), page("3", model("b"), "null")),
			ErrModelReportMalformed,
		},
		{"answered twice", session(initialize, one, one), ErrModelReportMalformed},
		{"an id never sent", session(initialize, one, page("99", model("a"), "null")), ErrModelReportMalformed},
		{"a string id", session(initialize, one, `{"id":"MARKER","result":{}}`), ErrModelReportMalformed},
		{"page without data", session(initialize, `{"id":2,"result":{"nextCursor":null}}`), ErrModelReportMalformed},
		{"non-string cursor", session(initialize, page("2", model("a"), "7")), ErrModelReportMalformed},
		{"duplicate key", session(initialize, strings.Replace(one, `"id":2`, `"id":2,"id":2`, 1)), ErrModelReportMalformed},
		{"not JSON", session("Error: MARKER", initialize, one), ErrModelReportMalformed},
		{"duplicate model", session(initialize, page("2", model("MARKER")+","+model("MARKER"), "null")), domain.ErrDuplicate},
		{"duplicate effort", session(initialize, page("2", `{"model":"a","displayName":"a","supportedReasoningEfforts":[`+
			`{"reasoningEffort":"MARKER"},{"reasoningEffort":"MARKER"}]}`, "null")), domain.ErrDuplicate},
		{"empty list", session(initialize, page("2", "", "null")), domain.ErrNoDiscoveredModels},
		{"empty model", session(initialize, page("2", model(""), "null")), domain.ErrEmptyField},
		{"option-like model", session(initialize, page("2", model("--MARKER"), "null")), ErrModelReportMalformed},
		{"effort with a space", session(initialize, page("2", `{"model":"a","displayName":"a","supportedReasoningEfforts":[`+
			`{"reasoningEffort":"high MARKER"}]}`, "null")), ErrModelReportMalformed},
		{"display name with a newline", session(initialize, page("2",
			`{"model":"a","displayName":"a\nMARKER","supportedReasoningEfforts":[]}`, "null")), ErrModelReportMalformed},
		{"nonzero exit", nonzero, ErrModelReportRefused},
		{
			"token in a model", session(initialize, page("2", model("x"+codexModelListTestAccessToken[4:20]), "null")),
			ErrModelReportCarriesCredential,
		},
		{"token in the version", tokenInVersion, ErrModelReportCarriesCredential},
		{"escaped token in a display name", session(initialize, page("2",
			`{"model":"a","displayName":"`+jsonEscaped(codexModelListTestAccessToken)+`","supportedReasoningEfforts":[]}`,
			"null")), ErrModelReportCarriesCredential},
		{
			"id token in a model", session(initialize, page("2", model("x"+codexModelListTestIDToken[4:20]), "null")),
			ErrModelReportCarriesCredential,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCodexModelReport(tc.out, codexModelListCredential())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "MARKER") || strings.Contains(err.Error(), codexModelListTestAccessToken[4:20]) {
				t.Fatalf("err %q quotes the CLI's output", err)
			}
		})
	}
}

func TestDecodeCodexModelReportRefusesACredentialWithoutASecret(t *testing.T) {
	t.Parallel()
	credential := LineupProbeCredential{mode: CodexAuthSubscription, body: []byte(`{"tokens":{}}`)}
	if _, err := DecodeCodexModelReport(codexModelListOutput(codexModelListTranscript, 0), credential); err == nil {
		t.Fatal("decoded under a credential with no access token to scan for")
	}
}

// TestDiscoverCodexModelsRunsOneContainedCall drives discovery through the
// fake runtime: the container receives the access-only credential, runs the
// app-server, and leaves nothing behind.
func TestDiscoverCodexModelsRunsOneContainedCall(t *testing.T) {
	t.Parallel()
	rt := newLineupProbeFakeRuntime(t, lineupProbeOutputFiles("codex-cli 0.162.1\n", codexModelListTranscript, "", "0\n")...)
	credential := codexModelListCredential()
	var staged []byte
	rt.onCopyIntoContainer = func(_, hostDir, targetDir string) error {
		if targetDir == lineupProbeStageDir {
			body, err := os.ReadFile(filepath.Join(hostDir, lineupProbeCredName)) //nolint:gosec // G304: the stage directory the probe under test hands the fake runtime
			if err != nil {
				return err
			}
			staged = body
		}
		return nil
	}
	var spec ContainerSpec
	rt.onCreateContainer = func(s ContainerSpec) error { spec = s; return nil }

	report, err := DiscoverCodexModels(t.Context(), rt, lineupProbeTestReviewImage, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Models, wantCodexModels()) {
		t.Fatalf("models = %+v", report.Models)
	}
	if string(staged) != string(credential.body) {
		t.Fatal("the container received a different credential")
	}
	if spec.Image != lineupProbeTestReviewImage || !strings.Contains(spec.Command[2], "codex app-server --listen stdio://") {
		t.Fatalf("spec = %+v", spec)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.ctrs) != 0 || len(rt.nets) != 0 {
		t.Fatalf("left %d containers and %d networks behind", len(rt.ctrs), len(rt.nets))
	}
}

func TestDiscoverCodexModelsRefusesAnExportStillHoldingTheCredential(t *testing.T) {
	t.Parallel()
	for name, leftover := range map[string]tarFile{
		"staged credential": {lineupProbeStageDir + "/" + lineupProbeCredName, []byte("token")},
		"CODEX_HOME":        {CodexAuthFileTarget, []byte("token")},
	} {
		t.Run(name, func(t *testing.T) {
			files := append(lineupProbeOutputFiles("codex-cli 0.162.1\n", codexModelListTranscript, "", "0\n"), leftover)
			rt := newLineupProbeFakeRuntime(t, files...)
			_, err := DiscoverCodexModels(t.Context(), rt, lineupProbeTestReviewImage, codexModelListCredential(), nil)
			if err == nil || !strings.Contains(err.Error(), "still holds the credential") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
