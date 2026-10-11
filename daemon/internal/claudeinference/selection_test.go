package claudeinference

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

// recordingDriver is a driver over a stub CLI that writes its arguments to
// the returned path and answers as the model `reported`. The driver's
// configured model is "test-model".
func recordingDriver(t *testing.T, reported string) (*Driver, string) {
	t.Helper()
	answer := completion()
	answer["modelUsage"] = map[string]any{reported: map[string]any{"outputTokens": 12, "webSearchRequests": 0}}
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	return testDriver(t, `printf '%s' "$*" > '`+argsPath+`'
cat > /dev/null
printf '%s' '`+string(b)+`'
`), argsPath
}

// A launch that sends no effort passes the argument list the 2026-09-09
// audit covered, element for element. The list is written out here so a
// change to it is a change to this test.
func TestCommandArgsWithoutAnEffortAreTheAuditedList(t *testing.T) {
	want := []string{
		"-p", "--output-format", "json", "--model", "some-model", "--safe-mode", "--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--max-turns", "1", "--system-prompt", "the prompt",
	}
	if got := commandArgs("some-model", "", "the prompt"); !slices.Equal(got, want) {
		t.Fatalf("commandArgs = %q, want %q", got, want)
	}
	withEffort := slices.Insert(slices.Clone(want), 5, "--effort", "medium")
	if got := commandArgs("some-model", "medium", "the prompt"); !slices.Equal(got, withEffort) {
		t.Fatalf("commandArgs = %q, want %q", got, withEffort)
	}
}

func TestCompletePassesTheRequestsModelAndEffort(t *testing.T) {
	t.Run("an explicit selection is passed as written", func(t *testing.T) {
		d, argsPath := recordingDriver(t, "claude-explicit-1")
		r := classifierRequest()
		r.Model, r.NativeEffort = "claude-explicit-1", "medium"
		got, err := d.Complete(t.Context(), r, "token")
		if err != nil || got.Observed.ModelID != "claude-explicit-1" {
			t.Fatalf("Complete = %+v, %v", got, err)
		}
		args, err := os.ReadFile(argsPath) //nolint:gosec // G304: test-owned path
		if err != nil || !strings.HasPrefix(string(args), "-p --output-format json --model claude-explicit-1 --effort medium --safe-mode ") {
			t.Fatalf("launch arguments = %q, %v", args, err)
		}
	})
	t.Run("an empty selection launches with the configured model and no effort", func(t *testing.T) {
		d, argsPath := recordingDriver(t, "test-model")
		got, err := d.Complete(t.Context(), classifierRequest(), "token")
		if err != nil || got.Observed.ModelID != "test-model" {
			t.Fatalf("Complete = %+v, %v", got, err)
		}
		args, err := os.ReadFile(argsPath) //nolint:gosec // G304: test-owned path
		if err != nil || !strings.HasPrefix(string(args), "-p --output-format json --model test-model --safe-mode ") ||
			strings.Contains(string(args), "--effort") {
			t.Fatalf("launch arguments = %q, %v", args, err)
		}
	})
	// The exact-match check follows the model the launch passed: a request
	// with its own model is refused when the configured model answers, and
	// the configured model's launch is refused when another answers.
	t.Run("a completion from another model than the launch passed is refused", func(t *testing.T) {
		d, _ := recordingDriver(t, "test-model")
		r := classifierRequest()
		r.Model = "claude-explicit-1"
		if _, err := d.Complete(t.Context(), r, "token"); err == nil {
			t.Fatal("accepted the configured model's answer to a request that named another model")
		}
		d, _ = recordingDriver(t, "claude-explicit-1")
		if _, err := d.Complete(t.Context(), classifierRequest(), "token"); err == nil {
			t.Fatal("accepted another model's answer to the configured model's launch")
		}
	})
}

// A stored offer only has to name a non-empty model, so the driver refuses a
// value the CLI could read as anything but one option value, and it does so
// before it starts a process.
func TestCompleteStartsNoProcessForAnUnsafeSelection(t *testing.T) {
	for _, tc := range []struct{ name, model, effort string }{
		{"model with a space", "claude explicit", ""},
		{"model with a newline", "claude\nexplicit", ""},
		{"model with a control character", "claude\x00explicit", ""},
		{"model with a format character", "claude\u202eexplicit", ""},
		{"model that is an option", "--dangerously-skip-permissions", ""},
		{"model that is not UTF-8", "claude\xff", ""},
		{"effort with a tab", "", "me\tdium"},
		{"effort that is an option", "", "-high"},
		{"effort with a trailing space", "claude-explicit-1", "high "},
		{"model longer than an observed identifier", strings.Repeat("m", 257), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, argsPath := recordingDriver(t, "claude-explicit-1")
			r := classifierRequest()
			r.Model, r.NativeEffort = tc.model, tc.effort
			_, quiescent, err := d.CompleteAndConfirm(t.Context(), r, "token")
			if err == nil || !quiescent {
				t.Fatalf("CompleteAndConfirm = quiescent %v, %v; want a refusal with no process", quiescent, err)
			}
			if _, statErr := os.Stat(argsPath); !os.IsNotExist(statErr) {
				t.Fatalf("the CLI started for an unsafe selection: %v", statErr)
			}
		})
	}
}

// rolesByName resolves each role through its own fake source, so two roles
// can sit on different offers and efforts under one client.
type rolesByName map[domain.RoleName]fake.Roles

func (r rolesByName) ResolveRole(ctx context.Context, role domain.RoleName) (inference.RoleCall, error) {
	return r[role].ResolveRole(ctx, role)
}

// Two sites on one client and one driver each launch with their own line's
// model and effort, and each call's record carries what it asked for. The
// driver's configured model is passed for neither.
func TestTwoSitesOnOneDriverLaunchWithTheirOwnSelection(t *testing.T) {
	answer := func(model, result string) string {
		m := completion()
		m["result"] = result
		m["modelUsage"] = map[string]any{model: map[string]any{"outputTokens": 12, "webSearchRequests": 0}}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const classifierModel, namerModel = "claude-site-a", "claude-site-b"
	argsPath := filepath.Join(t.TempDir(), "args")
	driver := testDriver(t, `printf '%s\n' "$*" >> '`+argsPath+`'
cat > /dev/null
case "$*" in
*"--model `+classifierModel+` "*) printf '%s' '`+answer(classifierModel, completion()["result"].(string))+`' ;;
*) printf '%s' '`+answer(namerModel, `{"name":"Pass the selection"}`)+`' ;;
esac
`)
	limits := inference.Limits{Calls: 10, ComputeUnits: 1_000_000, AttentionItems: 10, Starvation: time.Hour}
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
	classifier, namer := inference.ClassifierSite(budget), inference.TaskNamerSite(budget)
	source := func(model string, effort domain.EffortLevel) fake.Roles {
		return fake.Roles{Provider: "fake", Model: model, Effort: effort, Driver: driver, Credential: "synthetic-token"}
	}
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 100, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "ledger.json")
	client, err := inference.New(inference.Config{
		StatePath: statePath,
		Roles: rolesByName{
			domain.RoleFindingClassifier: source(classifierModel, domain.EffortHigh),
			domain.RoleTaskNamer:         source(namerModel, domain.EffortLow),
		},
		Sites: []inference.Site{classifier, namer}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range []inference.Site{classifier, namer} {
		fields := map[string]inference.InputField{}
		for _, policy := range site.Fields {
			fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
		}
		result, err := client.Call(t.Context(), site.ID, "project-1", "run-1", fields)
		if err != nil || result.Fallback || result.Identity == nil {
			t.Fatalf("%s = %+v, %v", site.ID, result, err)
		}
	}

	args, err := os.ReadFile(argsPath) //nolint:gosec // G304: test-owned path
	if err != nil {
		t.Fatal(err)
	}
	launches := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	want := []string{
		"-p --output-format json --model " + classifierModel + " --effort high --safe-mode ",
		"-p --output-format json --model " + namerModel + " --effort low --safe-mode ",
	}
	if len(launches) != len(want) || !strings.HasPrefix(launches[0], want[0]) || !strings.HasPrefix(launches[1], want[1]) {
		t.Fatalf("launches = %q, want prefixes %q", launches, want)
	}
	body, err := os.ReadFile(statePath) //nolint:gosec // G304: test-owned path
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Calls []struct {
			Identity struct {
				RequestedModelID string `json:"requested_model_id"`
				RequestedEffort  string `json:"requested_effort"`
				EffectiveEffort  string `json:"effective_effort"`
				Observed         struct {
					ModelID string `json:"model_id"`
				} `json:"observed"`
			} `json:"identity"`
		} `json:"calls"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	type recorded struct{ model, requested, effective, observed string }
	var got []recorded
	for _, call := range state.Calls {
		i := call.Identity
		got = append(got, recorded{i.RequestedModelID, i.RequestedEffort, i.EffectiveEffort, i.Observed.ModelID})
	}
	if wantRecords := []recorded{
		{classifierModel, "high", "high", classifierModel}, {namerModel, "low", "low", namerModel},
	}; !slices.Equal(got, wantRecords) {
		t.Fatalf("call records = %+v, want %+v", got, wantRecords)
	}
}
