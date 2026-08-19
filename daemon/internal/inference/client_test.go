package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func testBudget(calls int64) inference.Budget {
	limits := inference.Limits{Calls: calls, ComputeUnits: calls * 10_000, AttentionItems: calls, Starvation: time.Hour}
	return inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: calls, MaxStarvationPerRoot: time.Hour,
	}
}

func testClient(t *testing.T, driver inference.Driver, calls int64) (*inference.Client, *advisory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 100, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	classifier := inference.ClassifierSite(testBudget(calls))
	diagnostic := inference.DiagnosticSite(testBudget(calls))
	classifier.AuditEvery = 1
	diagnostic.AuditEvery = 1
	statePath := filepath.Join(dir, "ledger.json")
	client, err := inference.New(inference.Config{
		StatePath: statePath,
		Binding:   inference.Binding{Provider: "fake", Model: "test", Credential: "token-value", Driver: driver},
		Sites:     []inference.Site{classifier, diagnostic}, Advisory: store,
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, store, statePath
}

func finding(severity string) domain.Finding {
	return domain.Finding{
		ID: "finding-1", RunID: "run-1", Source: "codex_local", Severity: domain.FindingSeverity(severity),
		Location: &domain.FindingLocation{Path: "main.go", StartLine: 1, EndLine: 1}, Message: "token-value should never leave", RawText: "P1 detail", CreatedAt: time.Unix(1, 0).UTC(),
	}
}

func TestClassifierAllowlistRedactionDigestAndProducer(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"low","confidence":"high","note":"not required"}`), ComputeUnits: 4,
	}})
	client, _, statePath := testClient(t, driver, 10)
	decision, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ReducesWork || decision.RequiresAttention || decision.Fallback {
		t.Fatalf("decision = %#v", decision)
	}
	if !strings.HasPrefix(decision.Classification.Note, "producer=fake/test; ") {
		t.Fatalf("producer label absent: %q", decision.Classification.Note)
	}
	requests := driver.Requests()
	if len(requests) != 1 || requests[0].Fields["message"] != "[REDACTED] should never leave" {
		t.Fatalf("request = %#v", requests)
	}
	if requests[0].InputDigest != contentaddr.Sum(mustJSON(t, requests[0].Fields)) {
		t.Fatal("input digest does not bind outbound fields")
	}
	ledgerBody, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ledgerBody), requests[0].InputDigest) ||
		!strings.Contains(string(ledgerBody), `"producer":"fake/test"`) {
		t.Fatalf("call record = %s", ledgerBody)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClassifierExtremeOutputsFallBackConservatively(t *testing.T) {
	cases := []string{
		`{"materiality":"none","confidence":"high","note":"bad"}`,
		`{"materiality":"low","materiality":"high","confidence":"high","note":"duplicate"}`,
		`{"materiality":"low","confidence":"high","note":"ok","fixed":true}`,
		`{"materiality":"low","confidence":"high","note":"ok"} {}`,
		`{"materiality":"low","confidence":"high","note":"` + strings.Repeat("x", 9<<10) + `"}`,
	}
	for _, output := range cases {
		t.Run(output, func(t *testing.T) {
			driver := fake.New()
			driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{Output: []byte(output)}})
			client, _, _ := testClient(t, driver, 10)
			decision, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("high"), 1)
			if err != nil {
				t.Fatal(err)
			}
			if !decision.Fallback || decision.ReducesWork || !decision.RequiresAttention ||
				decision.Classification.Materiality != "high" || decision.Classification.Confidence != "low" {
				t.Fatalf("fallback decision = %#v", decision)
			}
		})
	}
}

func TestCallRejectsAllowlistSensitivityAndInvalidUTF8BeforeDriver(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]inference.InputField)
	}{
		{"extra", func(fields map[string]inference.InputField) {
			fields["workspace"] = inference.InputField{Value: "/tmp/repo", Sensitivity: inference.SensitivityOperational}
		}},
		{"missing", func(fields map[string]inference.InputField) { delete(fields, "message") }},
		{"sensitivity", func(fields map[string]inference.InputField) {
			fields["message"] = inference.InputField{Value: "message", Sensitivity: inference.SensitivityPublic}
		}},
		{"invalid utf8", func(fields map[string]inference.InputField) {
			fields["message"] = inference.InputField{Value: string([]byte{0xff}), Sensitivity: inference.SensitivityRepository}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver := fake.New()
			client, _, _ := testClient(t, driver, 10)
			fields := map[string]inference.InputField{
				"finding_id": {Value: "finding-1", Sensitivity: inference.SensitivityOperational},
				"source":     {Value: "codex_local", Sensitivity: inference.SensitivityOperational},
				"severity":   {Value: "P2", Sensitivity: inference.SensitivityOperational},
				"location":   {Value: "main.go:1", Sensitivity: inference.SensitivityRepository},
				"message":    {Value: "message", Sensitivity: inference.SensitivityRepository},
				"raw_text":   {Value: "raw", Sensitivity: inference.SensitivityRepository},
			}
			tc.mutate(fields)
			result, err := client.Call(context.Background(), inference.ClassifierSiteID, "project-1", "run-1", fields)
			if err != nil || !result.Fallback || len(driver.Requests()) != 0 {
				t.Fatalf("Call = %#v, %v; requests = %#v", result, err, driver.Requests())
			}
		})
	}
}

func TestInferenceDownAndRepeatedCallsStayBounded(t *testing.T) {
	client, _, _ := testClient(t, nil, 2)
	for call := 0; call < 4; call++ {
		decision, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("critical"), call+1)
		if err != nil {
			t.Fatal(err)
		}
		if !decision.Fallback || !decision.RequiresAttention {
			t.Fatalf("call %d decision = %#v", call, decision)
		}
	}
}

func TestDiagnosticIsAdvisoryOnlyAndUnavailableSkipsClaim(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.DiagnosticSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"probable_cause":"quota","explanation":"provider rejected the request"}`), ComputeUnits: 2,
	}})
	client, store, _ := testClient(t, driver, 10)
	input := inference.DiagnosticInput{
		Project: "project-1", RootLineage: "run-1", RunID: "run-1",
		FailureClass: "failed", FailingStep: "implement", Reason: "exit 1",
	}
	if err := client.DiagnoseExecutionFailure(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Kind != "diagnostic_claim" || entries[1].Producer != "fake/test" {
		t.Fatalf("advisory entries = %#v", entries)
	}
	down, downStore, _ := testClient(t, nil, 10)
	if err := down.DiagnoseExecutionFailure(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if got, listErr := downStore.List(context.Background()); listErr != nil || len(got) != 0 {
		t.Fatalf("inference-down claims = %#v, %v", got, listErr)
	}
}

func TestBudgetPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10,
		advisory.WithClock(func() time.Time { return time.Unix(100, 0).UTC() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(driver inference.Driver) *inference.Client {
		site := inference.ClassifierSite(testBudget(1))
		client, err := inference.New(inference.Config{
			StatePath: filepath.Join(dir, "ledger.json"),
			Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
			Sites:     []inference.Site{site}, Advisory: store, Now: func() time.Time { return time.Unix(100, 0).UTC() },
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	firstDriver := fake.New()
	firstDriver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"medium","confidence":"high","note":"material"}`), ComputeUnits: 1,
	}})
	if decision, err := newClient(firstDriver).ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1); err != nil || decision.Fallback {
		t.Fatalf("first decision = %#v, %v", decision, err)
	}
	secondDriver := fake.New()
	secondDriver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"low","confidence":"high","note":"ignored"}`), ComputeUnits: 1,
	}})
	decision, err := newClient(secondDriver).ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Fallback || len(secondDriver.Requests()) != 0 {
		t.Fatalf("post-restart decision = %#v, requests = %#v", decision, secondDriver.Requests())
	}
}

func TestSecretNeverRendersItsValue(t *testing.T) {
	secret := inference.Secret("credential-value")
	renderings := []string{
		fmt.Sprint(secret), fmt.Sprintf("%s", secret), fmt.Sprintf("%q", secret),
		fmt.Sprintf("%v", secret), fmt.Sprintf("%+v", secret), fmt.Sprintf("%#v", secret),
		fmt.Sprintf("%x", secret),
	}
	body, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	renderings = append(renderings, string(body))
	for _, rendering := range renderings {
		if strings.Contains(rendering, secret.Reveal()) {
			t.Fatalf("secret leaked through %q", rendering)
		}
	}
}

func TestAttentionBudgetCountsStableItemIdentityOnce(t *testing.T) {
	client, _, _ := testClient(t, nil, 1)
	if err := client.ReserveAttention(inference.ClassifierSiteID, "project-1", "run-1", "item-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.ReserveAttention(inference.ClassifierSiteID, "project-1", "run-1", "item-1"); err != nil {
		t.Fatalf("idempotent attention replay: %v", err)
	}
	if err := client.ReserveAttention(inference.ClassifierSiteID, "project-1", "run-1", "item-2"); err == nil {
		t.Fatal("distinct attention exceeded no bound")
	}
}

func TestUnknownSeverityAndMalformedPersistedClassificationFailProtective(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"low","confidence":"low","note":"uncertain"}`), ComputeUnits: 1,
	}})
	client, _, _ := testClient(t, driver, 10)
	decision, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("P0"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.RequiresAttention {
		t.Fatalf("unknown severity decision = %#v", decision)
	}
	malformed := decision.Classification
	malformed.Confidence = "high"
	malformed.Materiality = "invented"
	if requires, err := client.EvaluateClassification(finding("critical"), malformed); err == nil || !requires {
		t.Fatalf("malformed persisted classification = requires %v, %v", requires, err)
	}
}

func TestClassifierContractExposesNativeSeverityMapping(t *testing.T) {
	contract := inference.ClassifierSite(testBudget(10)).Annotation
	if contract.UnknownSeverityFallback != "high" {
		t.Fatalf("unknown severity fallback = %q", contract.UnknownSeverityFallback)
	}
	found := false
	for _, mapping := range contract.SeverityMappings {
		if mapping.Source == "codex_local" && mapping.Native == "p1" && mapping.Normalized == "high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("P1 severity mapping absent: %#v", contract.SeverityMappings)
	}
}

func TestAuditSamplingUsesReservedCallOrdinal(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	scripts := make([]fake.Script, 0, 3)
	for call := 0; call < 3; call++ {
		scripts = append(scripts, fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"material"}`), ComputeUnits: 1,
		}})
	}
	driver.Script(inference.ClassifierSiteID, scripts...)
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 2
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{site}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 3; version++ {
		decision, callErr := client.ClassifyFinding(
			context.Background(), "project-1", "run-1", finding("medium"), version,
		)
		if callErr != nil || decision.Fallback {
			t.Fatalf("call %d = %#v, %v", version, decision, callErr)
		}
	}
	entries, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID == entries[1].ID {
		t.Fatalf("ordinal audit samples = %#v", entries)
	}
}

func TestStateDirectoryLossAfterInitializationFailsInferenceClosed(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "ledger.json")
	newClient := func(driver inference.Driver) *inference.Client {
		client, err := inference.New(inference.Config{
			StatePath: statePath, Binding: inference.Binding{Provider: "fake", Model: "test", Driver: driver},
			Sites: []inference.Site{inference.ClassifierSite(testBudget(10))}, Advisory: store, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	_ = newClient(nil)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"low","confidence":"high","note":"must not run"}`), ComputeUnits: 1,
	}})
	decision, err := newClient(driver).ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Fallback || len(driver.Requests()) != 0 {
		t.Fatalf("missing-ledger decision = %#v, requests = %#v", decision, driver.Requests())
	}
}

type stuckDriver struct{ release <-chan struct{} }

func (d stuckDriver) Complete(context.Context, inference.Request, inference.Secret) (inference.Response, error) {
	<-d.release
	return inference.Response{}, context.Canceled
}

func TestClientEnforcesHardTimeoutWhenDriverIgnoresContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	dir := t.TempDir()
	now := func() time.Time { return time.Now().UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	site := inference.ClassifierSite(testBudget(10))
	site.Timeout = 10 * time.Millisecond
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "stuck", Model: "test", Driver: stuckDriver{release: release}},
		Sites:     []inference.Site{site}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	decision, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("high"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Fallback || time.Since(started) > time.Second {
		t.Fatalf("hard-timeout decision = %#v after %s", decision, time.Since(started))
	}
	second, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("high"), 2)
	if err != nil || !second.Fallback {
		t.Fatalf("orphan-bounded decision = %#v, %v", second, err)
	}
}

type failingAdvisory struct {
	store *advisory.Store
	fail  bool
}

func (w *failingAdvisory) Append(ctx context.Context, entry advisory.Entry) error {
	if w.fail {
		w.fail = false
		return errors.New("audit unavailable")
	}
	return w.store.Append(ctx, entry)
}
func (w *failingAdvisory) Prune(ctx context.Context) error { return w.store.Prune(ctx) }

func TestFailedRequiredAuditBlocksLaterProviderCalls(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	writer := &failingAdvisory{store: store, fail: true}
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID,
		fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"first"}`), ComputeUnits: 1,
		}},
		fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"second"}`), ComputeUnits: 1,
		}},
	)
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 1
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{site}, Advisory: writer,
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1); err == nil {
		t.Fatal("required audit failure was accepted")
	}
	second, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 2)
	if err != nil || second.Fallback || len(driver.Requests()) != 2 {
		t.Fatalf("post-audit call = %#v, %v; requests = %d", second, err, len(driver.Requests()))
	}
	entries, err := store.List(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("transferred audit samples = %#v, %v", entries, err)
	}
}

func TestPendingAuditTransfersAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 10
	newClient := func(driver inference.Driver) *inference.Client {
		client, newErr := inference.New(inference.Config{
			StatePath: filepath.Join(dir, "ledger.json"),
			Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
			Sites:     []inference.Site{site}, Advisory: store, Now: now,
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return client
	}
	failed := fake.New()
	failed.Script(inference.ClassifierSiteID, fake.Script{Err: errors.New("provider failed")})
	if decision, callErr := newClient(failed).ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 1,
	); callErr != nil || !decision.Fallback {
		t.Fatalf("failed sampled call = %#v, %v", decision, callErr)
	}
	recovered := fake.New()
	recovered.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"medium","confidence":"high","note":"recovered"}`), ComputeUnits: 1,
	}})
	if decision, callErr := newClient(recovered).ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 2,
	); callErr != nil || decision.Fallback {
		t.Fatalf("recovered sampled call = %#v, %v", decision, callErr)
	}
	entries, err := store.List(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovered audit samples = %#v, %v", entries, err)
	}
}

func TestMaintainPrunesExpiredCallMetadataAndPreservesAuditDebt(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(100, 0).UTC()
	now := func() time.Time { return current }
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	writer := &failingAdvisory{store: store, fail: true}
	statePath := filepath.Join(dir, "ledger.json")
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 1
	site.Retention = time.Minute
	failed := fake.New()
	failed.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"medium","confidence":"high","note":"first"}`), ComputeUnits: 1,
	}})
	client, err := inference.New(inference.Config{
		StatePath: statePath,
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: failed},
		Sites:     []inference.Site{site}, Advisory: writer, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, callErr := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 1,
	); callErr == nil {
		t.Fatal("required audit failure was accepted")
	}
	before, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), `"project":"project-1"`) {
		t.Fatalf("call metadata was not recorded: %s", before)
	}

	current = current.Add(2 * time.Minute)
	if err := client.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	maintained, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(maintained), `"project":"project-1"`) ||
		!strings.Contains(string(maintained), `"audit_debt":{"finding_classifier":true}`) {
		t.Fatalf("maintained ledger = %s", maintained)
	}

	recovered := fake.New()
	recovered.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"medium","confidence":"high","note":"recovered"}`), ComputeUnits: 1,
	}})
	client, err = inference.New(inference.Config{
		StatePath: statePath,
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: recovered},
		Sites:     []inference.Site{site}, Advisory: writer, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 2,
	)
	if err != nil || decision.Fallback {
		t.Fatalf("audit-debt recovery = %#v, %v", decision, err)
	}
	entries, err := store.List(context.Background())
	if err != nil || len(entries) != 1 || entries[0].Kind != "audit_sample" {
		t.Fatalf("recovered audit sample = %#v, %v", entries, err)
	}
	current = current.Add(2 * time.Minute)
	if err := client.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(final), `"project":"project-1"`) ||
		strings.Contains(string(final), `"audit_debt"`) {
		t.Fatalf("completed call survived maintenance: %s", final)
	}
}

func TestMaintainRetainsAnActiveExpiredCallUntilAuditCompletes(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(100, 0).UTC()
	now := func() time.Time { return current }
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{
		Wait: release,
		Response: inference.Response{
			Output:       []byte(`{"materiality":"medium","confidence":"high","note":"completed"}`),
			ComputeUnits: 1,
		},
	})
	statePath := filepath.Join(dir, "ledger.json")
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 1
	site.Retention = time.Minute
	site.Timeout = time.Hour
	client, err := inference.New(inference.Config{
		StatePath: statePath,
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{site}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan inference.ClassificationDecision, 1)
	errResult := make(chan error, 1)
	go func() {
		decision, callErr := client.ClassifyFinding(
			context.Background(), "project-1", "run-1", finding("medium"), 1,
		)
		result <- decision
		errResult <- callErr
	}()
	for deadline := time.Now().Add(time.Second); len(driver.Requests()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("driver call did not start")
		}
		time.Sleep(time.Millisecond)
	}
	current = current.Add(2 * time.Minute)
	if err := client.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), `"project":"project-1"`) {
		t.Fatalf("active call was retention-pruned: %s", active)
	}
	close(release)
	released = true
	if decision, callErr := <-result, <-errResult; callErr != nil || decision.Fallback {
		t.Fatalf("active call completion = %#v, %v", decision, callErr)
	}
	if err := client.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(completed), `"project":"project-1"`) {
		t.Fatalf("completed expired call survived maintenance: %s", completed)
	}
}

func TestReserveAtAnotherSiteRetainsAnActiveExpiredCall(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(100, 0).UTC()
	now := func() time.Time { return current }
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{
		Wait: release,
		Response: inference.Response{
			Output:       []byte(`{"materiality":"medium","confidence":"high","note":"completed"}`),
			ComputeUnits: 1,
		},
	})
	driver.Script(inference.DiagnosticSiteID, fake.Script{Response: inference.Response{
		Output:       []byte(`{"probable_cause":"quota","explanation":"provider rejected the request"}`),
		ComputeUnits: 1,
	}})
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: 2 * time.Hour}
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: 2 * time.Hour,
	}
	classifier := inference.ClassifierSite(budget)
	diagnostic := inference.DiagnosticSite(budget)
	classifier.AuditEvery, diagnostic.AuditEvery = 1, 1
	classifier.Retention, diagnostic.Retention = time.Minute, time.Minute
	classifier.Timeout, diagnostic.Timeout = time.Hour, time.Hour
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{classifier, diagnostic}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan inference.ClassificationDecision, 1)
	errResult := make(chan error, 1)
	go func() {
		decision, callErr := client.ClassifyFinding(
			context.Background(), "project-1", "run-1", finding("medium"), 1,
		)
		result <- decision
		errResult <- callErr
	}()
	for deadline := time.Now().Add(time.Second); len(driver.Requests()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("classifier call did not start")
		}
		time.Sleep(time.Millisecond)
	}
	current = current.Add(2 * time.Minute)
	if err := client.DiagnoseExecutionFailure(context.Background(), inference.DiagnosticInput{
		Project: "project-2", RootLineage: "run-2", RunID: "run-2",
		FailureClass: "failed", FailingStep: "implement", Reason: "exit 1",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	released = true
	if decision, callErr := <-result, <-errResult; callErr != nil || decision.Fallback {
		t.Fatalf("classifier completion after cross-site reserve = %#v, %v", decision, callErr)
	}
	entries, err := store.List(context.Background())
	auditSamples := 0
	for _, entry := range entries {
		if entry.Kind == "audit_sample" {
			auditSamples++
		}
	}
	if err != nil || auditSamples != 2 {
		t.Fatalf("cross-site audit samples = %#v, %v", entries, err)
	}
}

func TestMaintainPrunesTimedOutOrphanCallMetadata(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(100, 0).UTC()
	now := func() time.Time { return current }
	store, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	statePath := filepath.Join(dir, "ledger.json")
	site := inference.ClassifierSite(testBudget(10))
	site.AuditEvery = 1
	site.Retention = time.Minute
	site.Timeout = 10 * time.Millisecond
	client, err := inference.New(inference.Config{
		StatePath: statePath,
		Binding: inference.Binding{
			Provider: "stuck", Model: "test", Driver: stuckDriver{release: release},
		},
		Sites: []inference.Site{site}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("high"), 1,
	)
	if err != nil || !decision.Fallback {
		t.Fatalf("timed-out orphan = %#v, %v", decision, err)
	}
	current = current.Add(2 * time.Minute)
	if err := client.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"project":"project-1"`) {
		t.Fatalf("timed-out orphan retained call metadata: %s", body)
	}
}

func TestLedgerV1MigratesBeforeAuditDebtCanBeWritten(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "ledger.json")
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	newClient := func() *inference.Client {
		client, newErr := inference.New(inference.Config{
			StatePath: statePath,
			Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: fake.New()},
			Sites:     []inference.Site{inference.ClassifierSite(testBudget(10))}, Advisory: store,
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		return client
	}
	_ = newClient()
	body, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	state["version"] = "freeside.inference-budget/v1"
	delete(state, "audit_debt")
	legacy, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = newClient()
	migrated, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), `"version":"freeside.inference-budget/v2"`) {
		t.Fatalf("legacy ledger was not migrated before use: %s", migrated)
	}
}

func TestExhaustedReplacementRetainsPendingAuditDebt(t *testing.T) {
	dir := t.TempDir()
	current := time.Unix(100, 0).UTC()
	now := func() time.Time { return current }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	driver.Script(inference.DiagnosticSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"probable_cause":"quota","explanation":"provider rejected the request"}`), ComputeUnits: 1,
	}})
	driver.Script(inference.ClassifierSiteID,
		fake.Script{Err: errors.New("provider failed")},
		fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"recovered"}`), ComputeUnits: 1,
		}},
	)
	siteLimits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	globalLimits := siteLimits
	globalLimits.Calls = 2
	budget := inference.Budget{
		Window: time.Hour, Site: siteLimits, Project: siteLimits, Global: globalLimits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	}
	classifier := inference.ClassifierSite(budget)
	diagnostic := inference.DiagnosticSite(budget)
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{classifier, diagnostic}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DiagnoseExecutionFailure(context.Background(), inference.DiagnosticInput{
		Project: "other-project", RootLineage: "other-root", RunID: "other-root",
		FailureClass: "failed", FailingStep: "implement", Reason: "exit 1",
	}); err != nil {
		t.Fatal(err)
	}
	current = current.Add(30 * time.Minute)
	if decision, callErr := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 1,
	); callErr != nil || !decision.Fallback {
		t.Fatalf("failed sampled call = %#v, %v", decision, callErr)
	}
	current = current.Add(time.Minute)
	if decision, callErr := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 2,
	); callErr != nil || !decision.Fallback || len(driver.Requests()) != 2 {
		t.Fatalf("exhausted replacement = %#v, %v; requests = %d", decision, callErr, len(driver.Requests()))
	}
	current = current.Add(30 * time.Minute)
	if decision, callErr := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 3,
	); callErr != nil || decision.Fallback {
		t.Fatalf("recovered replacement = %#v, %v", decision, callErr)
	}
	entries, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auditSamples := 0
	for _, entry := range entries {
		if entry.Kind == "audit_sample" {
			auditSamples++
		}
	}
	if auditSamples != 2 {
		t.Fatalf("audit samples = %d, entries = %#v", auditSamples, entries)
	}
}

func TestAmbiguousLedgerWriteDisablesLaterInference(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "inference")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) }) //nolint:gosec // test-owned directory needs traversal
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output: []byte(`{"materiality":"medium","confidence":"high","note":"must not run"}`), ComputeUnits: 1,
	}})
	client, err := inference.New(inference.Config{
		StatePath:  filepath.Join(stateDir, "ledger.json"),
		AnchorPath: filepath.Join(dir, "ledger.anchor"),
		Binding:    inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:      []inference.Site{inference.ClassifierSite(testBudget(10))}, Advisory: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil { //nolint:gosec // deliberate write-denial fixture
		t.Fatal(err)
	}
	first, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1)
	if err != nil || !first.Fallback {
		t.Fatalf("ambiguous-write decision = %#v, %v", first, err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil { //nolint:gosec // restore test-owned directory traversal
		t.Fatal(err)
	}
	second, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 2)
	if err != nil || !second.Fallback || len(driver.Requests()) != 0 {
		t.Fatalf("disabled-ledger decision = %#v, %v; requests = %d", second, err, len(driver.Requests()))
	}
}

func TestComputeAndStarvationAreReservedBeforeDriverCall(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Now().UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID,
		fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"first"}`), ComputeUnits: 1,
		}},
		fake.Script{Response: inference.Response{
			Output: []byte(`{"materiality":"medium","confidence":"high","note":"second"}`), ComputeUnits: 1,
		}},
	)
	limits := inference.Limits{Calls: 2, ComputeUnits: 10_000, AttentionItems: 2, Starvation: 30 * time.Second}
	site := inference.ClassifierSite(inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 2, MaxStarvationPerRoot: 30 * time.Second,
	})
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "test", Driver: driver},
		Sites:     []inference.Site{site}, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 1)
	if err != nil || first.Fallback {
		t.Fatalf("first reserved call = %#v, %v", first, err)
	}
	second, err := client.ClassifyFinding(context.Background(), "project-1", "run-1", finding("medium"), 2)
	if err != nil || !second.Fallback || len(driver.Requests()) != 1 {
		t.Fatalf("second reserved call = %#v, %v; requests = %d", second, err, len(driver.Requests()))
	}
}
