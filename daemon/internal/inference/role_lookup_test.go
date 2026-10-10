package inference_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

// roleHealth records what the client reports about each role.
type roleHealth struct {
	mu       sync.Mutex
	unbound  map[domain.RoleName]string
	resolved map[domain.RoleName]int
}

func newRoleHealth() *roleHealth {
	return &roleHealth{unbound: map[domain.RoleName]string{}, resolved: map[domain.RoleName]int{}}
}

func (h *roleHealth) RoleUnbound(_ context.Context, role domain.RoleName, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unbound[role] = reason
}

func (h *roleHealth) RoleResolved(_ context.Context, role domain.RoleName) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolved[role]++
}

func budgetedSites() []inference.Site {
	budget := testBudget(10)
	return []inference.Site{
		inference.DiagnosticSite(budget), inference.TaskNamerSite(budget),
		inference.PublicationAuthorExplainSite(budget), inference.PublicationAuthorProposeSite(budget),
		inference.ClassifierSite(budget), inference.AdjudicatorSite(budget),
		inference.DriftAuditorSite(budget), inference.DiscussionSite(budget),
	}
}

// lookupClient builds a client over the given role source and returns it with
// the path of its ledger.
func lookupClient(
	t *testing.T, roles inference.RoleSource, health inference.HealthReporter, sites []inference.Site,
) (*inference.Client, string) {
	t.Helper()
	dir := t.TempDir()
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 100, 16<<10, advisory.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "ledger.json")
	client, err := inference.New(inference.Config{
		StatePath: statePath, Roles: roles, Health: health, Sites: sites, Advisory: store, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, statePath
}

// siteFields fills a site's whole allowlist at each field's declared
// sensitivity, which is all Call asks of its input.
func siteFields(site inference.Site) map[string]inference.InputField {
	fields := map[string]inference.InputField{}
	for _, policy := range site.Fields {
		fields[policy.Name] = inference.InputField{Value: "data", Sensitivity: policy.Sensitivity}
	}
	return fields
}

// ledgerCalls reads the call records a ledger holds.
func ledgerCalls(t *testing.T, statePath string) []json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Calls []json.RawMessage `json:"calls"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	return state.Calls
}

type recordedIdentity struct {
	Admission struct {
		Role domain.RoleName `json:"role"`
	} `json:"admission"`
	CredentialSource string `json:"credential_source"`
	Observed         *struct {
		ModelID         *string `json:"model_id"`
		ServingOperator *string `json:"serving_operator"`
		OutputTokens    int64   `json:"output_tokens"`
	} `json:"observed"`
	Independence []struct {
		WritingRole domain.RoleName `json:"writing_role"`
		Lineage     string          `json:"lineage"`
	} `json:"independence"`
}

func onlyIdentity(t *testing.T, statePath string) recordedIdentity {
	t.Helper()
	calls := ledgerCalls(t, statePath)
	if len(calls) != 1 {
		t.Fatalf("ledger holds %d call records, want 1", len(calls))
	}
	var record struct {
		Identity *recordedIdentity `json:"identity"`
	}
	if err := json.Unmarshal(calls[0], &record); err != nil || record.Identity == nil {
		t.Fatalf("call record carries no identity: %s (%v)", calls[0], err)
	}
	return *record.Identity
}

const unboundReason = "judgment role has no admissible lineup line"

// A role the lineup cannot fill returns the site's declared fail-safe at
// every site, reaches no driver, spends no budget, and is reported unbound
// with the specific reason. The two causes are the two places a refusal comes
// from: the source finds no line, and the client's own admission refuses what
// the source returned.
func TestUnboundRoleReturnsEverySiteFailSafe(t *testing.T) {
	causes := []struct {
		name   string
		edit   func(domain.RoleName, *inference.RoleCall) error
		reason string
	}{
		{"no line", func(domain.RoleName, *inference.RoleCall) error {
			return errors.New("the lineup has no line for it")
		}, "the lineup has no line for it"},
		{"admission refused", func(_ domain.RoleName, call *inference.RoleCall) error {
			audit := *call.LaunchProof
			audit.HarnessBuild = "claude-code 9.9.9"
			call.LaunchProof = &audit
			return nil
		}, "audit covers harness build"},
	}
	for _, site := range budgetedSites() {
		for _, cause := range causes {
			t.Run(site.ID+"/"+cause.name, func(t *testing.T) {
				driver := fake.New()
				health := newRoleHealth()
				client, statePath := lookupClient(t, fake.Roles{
					Provider: "fake", Model: "test", Driver: driver, Edit: cause.edit,
				}, health, []inference.Site{site})
				result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
				if err != nil {
					t.Fatal(err)
				}
				if !result.Fallback || !bytes.Equal(result.Output, []byte(site.FailSafe)) ||
					result.Reason != unboundReason || result.Producer != "unavailable/unbound" || result.Identity != nil {
					t.Fatalf("result = %+v, want the %s fail-safe", result, site.ID)
				}
				if requests := driver.Requests(); len(requests) != 0 {
					t.Fatalf("an unbound role reached the driver: %+v", requests)
				}
				if calls := ledgerCalls(t, statePath); len(calls) != 0 {
					t.Fatalf("an unbound role reserved a call: %s", calls)
				}
				role, _ := domain.RoleForSite(site.ID)
				if reason := health.unbound[role]; !strings.Contains(reason, cause.reason) || health.resolved[role] != 0 {
					t.Fatalf("health for %s: unbound %q, resolved %d", role, reason, health.resolved[role])
				}
			})
		}
	}
}

// A role the configuration leaves off fails safe like inference being down,
// and is reported resolved so no item stands for it.
func TestRoleLeftOffFailsSafeWithoutAnItem(t *testing.T) {
	driver := fake.New()
	health := newRoleHealth()
	site := inference.PublicationAuthorProposeSite(testBudget(10))
	client, statePath := lookupClient(t, fake.Roles{
		Provider: "fake", Model: "test", Driver: driver,
		Edit: func(domain.RoleName, *inference.RoleCall) error { return inference.ErrRoleOff },
	}, health, []inference.Site{site})
	result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
	if err != nil || !result.Fallback || result.Reason != inference.ErrUnavailable.Error() ||
		!bytes.Equal(result.Output, []byte(site.FailSafe)) {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(driver.Requests()) != 0 || len(ledgerCalls(t, statePath)) != 0 {
		t.Fatal("a role left off reached the driver or the ledger")
	}
	if len(health.unbound) != 0 || health.resolved[domain.RolePublicationAuthor] != 1 {
		t.Fatalf("health = unbound %v, resolved %v", health.unbound, health.resolved)
	}
}

// The role source's answer is input. The client holds the prompt bytes to the
// digest the line names and runs the admission itself, so a source that hands
// back an inconsistent or unproved agent gets the fail-safe, never a call.
func TestClientAdmitsWhatTheSourceReturns(t *testing.T) {
	other := domain.Digest(contentaddr.Sum([]byte("another")))
	tests := []struct {
		name string
		edit func(*inference.RoleCall)
	}{
		{"prompt bytes differ from their digest", func(call *inference.RoleCall) { call.Prompt.Body = []byte("swapped") }},
		{"line names another prompt name", func(call *inference.RoleCall) { call.Line.PromptName = "another" }},
		{"line names another prompt digest", func(call *inference.RoleCall) { call.Line.PromptDigest = other }},
		{"line names another agent", func(call *inference.RoleCall) { call.Line.AgentDigest = other }},
		{"no launch proof", func(call *inference.RoleCall) { call.LaunchProof = nil }},
		{"launch proof for another adapter", func(call *inference.RoleCall) {
			audit := *call.LaunchProof
			audit.AdapterDigest = other
			call.LaunchProof = &audit
		}},
		{"offer edited after sealing", func(call *inference.RoleCall) { call.Offer.RouteModelID = "swapped" }},
		{"agent pins another offer", func(call *inference.RoleCall) { call.Agent.OfferDigest = other }},
		{"generation never persisted", func(call *inference.RoleCall) { call.Generation.Ordinal = 0 }},
		{"generation of another enrollment", func(call *inference.RoleCall) { call.Generation.EnrollmentID = "another" }},
		{"lineup revision malformed", func(call *inference.RoleCall) { call.LineupRevision = "sha256:short" }},
		{"unknown credential source", func(call *inference.RoleCall) { call.CredentialSource = "enrollment" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver := fake.New()
			health := newRoleHealth()
			site := inference.PublicationAuthorProposeSite(testBudget(10))
			client, statePath := lookupClient(t, fake.Roles{
				Provider: "fake", Model: "test", Driver: driver,
				Edit: func(_ domain.RoleName, call *inference.RoleCall) error { tc.edit(call); return nil },
			}, health, []inference.Site{site})
			result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
			if err != nil || !result.Fallback || result.Reason != unboundReason || result.Identity != nil {
				t.Fatalf("result = %+v, %v", result, err)
			}
			if len(driver.Requests()) != 0 || len(ledgerCalls(t, statePath)) != 0 {
				t.Fatal("a refused admission reached the driver or the ledger")
			}
			if health.unbound[domain.RolePublicationAuthor] == "" {
				t.Fatal("the refusal was not reported")
			}
		})
	}
}

// The golden pins what one judging call writes: the admission it ran under,
// its comparison keys, what it requested and what answered, and the
// independence record. Reopening the ledger proves the record validates.
func TestCallRecordGolden(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
		Observed: inference.Observed{ModelID: "observed-model", OutputTokens: 4},
	}})
	roles := fake.Roles{
		Provider: "fake", Model: "test", Credential: "token-value", Driver: driver, LineageGroup: "anthropic",
		WriterLineage: map[domain.RoleName]string{domain.RoleImplementer: "anthropic"},
	}
	site := inference.AdjudicatorSite(testBudget(10))
	client, statePath := lookupClient(t, roles, nil, []inference.Site{site})
	entries, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", adjudicatorInput())
	if err != nil || len(entries) != 1 {
		t.Fatalf("AdjudicateFindings = %+v, %v", entries, err)
	}
	calls := ledgerCalls(t, statePath)
	if len(calls) != 1 {
		t.Fatalf("ledger holds %d call records, want 1", len(calls))
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, calls[0], "", "  "); err != nil {
		t.Fatal(err)
	}
	indented.WriteByte('\n')
	golden.Assert(t, "call_record_v3", indented.Bytes())

	reopened, err := inference.New(inference.Config{
		StatePath: statePath, Roles: roles, Sites: []inference.Site{site},
		Advisory: advisory.Unavailable(errors.New("unused")),
		Now:      func() time.Time { return time.Unix(101, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reopened.Call(context.Background(), site.ID, "project-1", "run-2", siteFields(site))
	if err != nil || result.Identity == nil {
		t.Fatalf("a ledger holding the golden record refused a later call: %+v, %v", result, err)
	}
}

// rewriteLedger edits a ledger file as decoded JSON.
func rewriteLedger(t *testing.T, statePath string, edit func(state map[string]any)) {
	t.Helper()
	body, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	edit(state)
	if body, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A v2 ledger's records have no identity. They load, the file moves to v3,
// and the old records stay readable beside the stamped ones written after.
// An older-labelled file that already carries an identity is refused, because
// the binary that writes that version would erase the field; so is a version
// this binary does not know.
func TestLedgerV2LoadsAndOlderLabelsCarryingIdentityAreRefused(t *testing.T) {
	site := inference.ClassifierSite(testBudget(10))
	roles := fake.Roles{Provider: "fake", Model: "test", Driver: fake.New()}
	call := func(t *testing.T, statePath, root string) inference.CallResult {
		t.Helper()
		client, err := inference.New(inference.Config{
			StatePath: statePath, Roles: roles, Sites: []inference.Site{site},
			Advisory: advisory.Unavailable(errors.New("unused")),
			Now:      func() time.Time { return time.Unix(100, 0).UTC() },
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Call(context.Background(), site.ID, "project-1", root, siteFields(site))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	seed := func(t *testing.T) string {
		t.Helper()
		statePath := filepath.Join(t.TempDir(), "ledger.json")
		if result := call(t, statePath, "run-1"); result.Identity == nil {
			t.Fatalf("seed call reserved no record: %+v", result)
		}
		return statePath
	}

	t.Run("v2 records load", func(t *testing.T) {
		statePath := seed(t)
		rewriteLedger(t, statePath, func(state map[string]any) {
			state["version"] = "freeside.inference-budget/v2"
			for _, record := range state["calls"].([]any) {
				delete(record.(map[string]any), "identity")
			}
		})
		if result := call(t, statePath, "run-2"); result.Identity == nil {
			t.Fatalf("a v2 ledger refused a call: %+v", result)
		}
		body, err := os.ReadFile(statePath) //nolint:gosec // test-owned state path
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"version":"freeside.inference-budget/v3"`) {
			t.Fatalf("the v2 ledger was not migrated: %s", body)
		}
		calls := ledgerCalls(t, statePath)
		if len(calls) != 2 || bytes.Contains(calls[0], []byte(`"identity"`)) ||
			!bytes.Contains(calls[1], []byte(`"identity"`)) {
			t.Fatalf("records after migration = %s", calls)
		}
	})
	for _, version := range []string{"freeside.inference-budget/v1", "freeside.inference-budget/v2"} {
		t.Run(version+" carrying identity is refused", func(t *testing.T) {
			statePath := seed(t)
			rewriteLedger(t, statePath, func(state map[string]any) {
				state["version"] = version
				delete(state, "audit_debt")
			})
			if result := call(t, statePath, "run-2"); !result.Fallback || result.Identity != nil {
				t.Fatalf("an older-labelled ledger carrying an identity was used: %+v", result)
			}
		})
	}
	t.Run("unknown version is refused", func(t *testing.T) {
		statePath := seed(t)
		rewriteLedger(t, statePath, func(state map[string]any) { state["version"] = "freeside.inference-budget/v4" })
		if result := call(t, statePath, "run-2"); !result.Fallback || result.Identity != nil {
			t.Fatalf("a ledger of an unknown version was used: %+v", result)
		}
	})
	t.Run("half an identity is refused", func(t *testing.T) {
		statePath := seed(t)
		rewriteLedger(t, statePath, func(state map[string]any) {
			identity := state["calls"].([]any)[0].(map[string]any)["identity"].(map[string]any)
			delete(identity, "treatment_digest")
		})
		if result := call(t, statePath, "run-2"); !result.Fallback || result.Identity != nil {
			t.Fatalf("a record with a partial identity was accepted: %+v", result)
		}
	})
	// A stored record is history. One that today's registries would not
	// write (a site since retired, another set of writing roles) still loads:
	// refusing it would disable the ledger, and a disabled ledger cannot
	// prune the record that disabled it.
	t.Run("a record today's registries would not write still loads", func(t *testing.T) {
		statePath := seed(t)
		rewriteLedger(t, statePath, func(state map[string]any) {
			record := state["calls"].([]any)[0].(map[string]any)
			record["site"] = "retired_site"
			record["identity"].(map[string]any)["independence"] = []any{
				map[string]any{"writing_role": "specifier", "lineage": "unknown"},
			}
		})
		// An identity on the result means the ledger loaded and reserved.
		if result := call(t, statePath, "run-2"); result.Identity == nil {
			t.Fatalf("a ledger holding an older shape of record was refused: %+v", result)
		}
	})
	t.Run("an independence entry that is no role is refused", func(t *testing.T) {
		statePath := seed(t)
		rewriteLedger(t, statePath, func(state map[string]any) {
			record := state["calls"].([]any)[0].(map[string]any)
			record["identity"].(map[string]any)["independence"] = []any{
				map[string]any{"writing_role": "nobody", "lineage": "unknown"},
			}
		})
		if result := call(t, statePath, "run-2"); !result.Fallback || result.Identity != nil {
			t.Fatalf("a record with a malformed independence entry was accepted: %+v", result)
		}
	})
}

// Two calls that differ only in the role prompt are the same treatment under
// a different prompt, and the driver is handed the prompt the line named.
func TestPromptOnlyDifferenceSharesTheTreatment(t *testing.T) {
	site := inference.PublicationAuthorProposeSite(testBudget(10))
	identityFor := func(prompt string) inference.CallIdentity {
		t.Helper()
		driver := fake.New()
		client, _ := lookupClient(t, fake.Roles{
			Provider: "fake", Model: "test", Driver: driver, AuthorPrompt: []byte(prompt),
		}, nil, []inference.Site{site})
		result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
		if err != nil || result.Identity == nil {
			t.Fatalf("result = %+v, %v", result, err)
		}
		requests := driver.Requests()
		if len(requests) != 1 || string(requests[0].RolePrompt) != prompt ||
			requests[0].RolePromptDigest != domain.Digest(contentaddr.Sum([]byte(prompt))) ||
			requests[0].RolePromptDigest != result.Identity.PromptDigest {
			t.Fatalf("driver request = %+v, identity = %+v", requests, result.Identity)
		}
		return *result.Identity
	}
	first, second := identityFor("Write for the reviewer."), identityFor("Write for the operator.")
	if first.PromptDigest == second.PromptDigest {
		t.Fatal("two prompts share a prompt digest")
	}
	if first.TreatmentDigest != second.TreatmentDigest || first.AgentDigest != second.AgentDigest ||
		first.SiteContractDigest != second.SiteContractDigest || first.Role != second.Role {
		t.Fatalf("a prompt-only difference moved another key: %+v vs %+v", first, second)
	}
}

// Two calls that differ only in the site's instruction differ in the
// site-contract digest and in no other key.
func TestSiteInstructionDifferenceMovesOnlyTheSiteContractDigest(t *testing.T) {
	identityFor := func(site inference.Site) inference.CallIdentity {
		t.Helper()
		client, _ := lookupClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: fake.New()},
			nil, []inference.Site{site})
		result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
		if err != nil || result.Identity == nil {
			t.Fatalf("result = %+v, %v", result, err)
		}
		return *result.Identity
	}
	site := inference.ClassifierSite(testBudget(10))
	reworded := site
	reworded.Instruction += " Answer briefly."
	first, second := identityFor(site), identityFor(reworded)
	if first.SiteContractDigest == second.SiteContractDigest {
		t.Fatal("an instruction change did not move the site-contract digest")
	}
	if first.SiteContractDigest != site.ContractDigest() || second.SiteContractDigest != reworded.ContractDigest() {
		t.Fatal("the recorded digest is not the site's contract digest")
	}
	if first.TreatmentDigest != second.TreatmentDigest || first.AgentDigest != second.AgentDigest ||
		first.PromptDigest != second.PromptDigest || first.Role != second.Role {
		t.Fatalf("an instruction-only difference moved another key: %+v vs %+v", first, second)
	}
}

// A call that judges written work records, per writing role, whether its
// lineage matched; it runs whatever the answer. A call that judges none
// records nothing.
func TestIndependenceIsRecordedAndNeverGates(t *testing.T) {
	record := func(site inference.Site, writers map[domain.RoleName]string) recordedIdentity {
		t.Helper()
		driver := fake.New()
		client, statePath := lookupClient(t, fake.Roles{
			Provider: "fake", Model: "test", Driver: driver, LineageGroup: "anthropic", WriterLineage: writers,
		}, nil, []inference.Site{site})
		if _, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site)); err != nil {
			t.Fatal(err)
		}
		if len(driver.Requests()) != 1 {
			t.Fatal("the call did not run")
		}
		return onlyIdentity(t, statePath)
	}
	lineages := func(identity recordedIdentity) string {
		var out []string
		for _, entry := range identity.Independence {
			out = append(out, string(entry.WritingRole)+"="+entry.Lineage)
		}
		return strings.Join(out, ",")
	}
	for _, site := range []inference.Site{
		inference.AdjudicatorSite(testBudget(10)), inference.DriftAuditorSite(testBudget(10)),
	} {
		same := record(site, map[domain.RoleName]string{
			domain.RoleImplementer: "anthropic", domain.RoleRemediator: "openai",
		})
		if got := lineages(same); got != "implementer=matched,remediator=differed" {
			t.Fatalf("%s independence = %q", site.ID, got)
		}
		if same.CredentialSource != "interim_flag" {
			t.Fatalf("%s credential_source = %q", site.ID, same.CredentialSource)
		}
		// A writing role the lineup does not name, or whose offer has no
		// lineage group, is unknown.
		if got := lineages(record(site, map[domain.RoleName]string{domain.RoleRemediator: ""})); got != "implementer=unknown,remediator=unknown" {
			t.Fatalf("%s independence with no writer lineage = %q", site.ID, got)
		}
	}
	if identity := record(inference.ClassifierSite(testBudget(10)), map[domain.RoleName]string{
		domain.RoleImplementer: "anthropic",
	}); identity.Independence != nil {
		t.Fatalf("a call that judges no written work recorded independence: %+v", identity.Independence)
	}
}

// What a driver says answered is recorded as it says it, within bounds. A
// driver that reports an identifier no record should hold is out of contract.
func TestObservedFactsAreBoundedAndRecorded(t *testing.T) {
	site := inference.ClassifierSite(testBudget(10))
	output := []byte(`{"materiality":"low","confidence":"high","note":"not required"}`)
	run := func(observed inference.Observed) (inference.CallResult, recordedIdentity) {
		t.Helper()
		driver := fake.New()
		driver.Script(site.ID, fake.Script{Response: inference.Response{
			Output: output, ComputeUnits: 4, Observed: observed,
		}})
		client, statePath := lookupClient(t, fake.Roles{Provider: "fake", Model: "test", Driver: driver},
			nil, []inference.Site{site})
		result, err := client.Call(context.Background(), site.ID, "project-1", "run-1", siteFields(site))
		if err != nil {
			t.Fatal(err)
		}
		return result, onlyIdentity(t, statePath)
	}

	result, identity := run(inference.Observed{ModelID: "observed-model", ServingOperator: "bedrock", OutputTokens: 4})
	if result.Fallback || identity.Observed == nil || identity.Observed.ModelID == nil ||
		*identity.Observed.ModelID != "observed-model" || identity.Observed.ServingOperator == nil ||
		*identity.Observed.ServingOperator != "bedrock" || identity.Observed.OutputTokens != 4 {
		t.Fatalf("result = %+v, observed = %+v", result, identity.Observed)
	}
	// A fact the driver does not have is null, not an empty string.
	result, identity = run(inference.Observed{OutputTokens: 4})
	if result.Fallback || identity.Observed == nil || identity.Observed.ModelID != nil ||
		identity.Observed.ServingOperator != nil {
		t.Fatalf("result = %+v, observed = %+v", result, identity.Observed)
	}
	for name, observed := range map[string]inference.Observed{
		"oversized model id":     {ModelID: strings.Repeat("m", 257)},
		"control character":      {ModelID: "model\nid"},
		"format character":       {ServingOperator: "bed\u202erock"},
		"invalid utf-8":          {ServingOperator: string([]byte{0xff})},
		"negative output tokens": {ModelID: "observed-model", OutputTokens: -1},
	} {
		result, identity := run(observed)
		if !result.Fallback || result.Reason != "driver response exceeded contract" || identity.Observed != nil {
			t.Fatalf("%s: result = %+v, observed = %+v", name, result, identity.Observed)
		}
	}
}

// The standing check names each role's result without making a call.
func TestCheckRolesReportsWithoutCalling(t *testing.T) {
	driver := fake.New()
	health := newRoleHealth()
	client, statePath := lookupClient(t, fake.Roles{
		Provider: "fake", Model: "test", Driver: driver,
		Edit: func(role domain.RoleName, _ *inference.RoleCall) error {
			if role == domain.RoleDriftAuditor {
				return errors.New("the lineup has no line for it")
			}
			if role == domain.RolePublicationAuthor {
				return inference.ErrRoleOff
			}
			return nil
		},
	}, health, budgetedSites())
	results := map[domain.RoleName]inference.RoleCheck{}
	for _, check := range client.CheckRoles(context.Background()) {
		results[check.Role] = check
	}
	if len(results) != 7 {
		t.Fatalf("checked %d roles, want the seven with a site: %+v", len(results), results)
	}
	for role, check := range results {
		switch role {
		case domain.RoleDriftAuditor:
			if check.Err == nil || check.Off || health.unbound[role] == "" {
				t.Fatalf("drift auditor check = %+v, health %q", check, health.unbound[role])
			}
		case domain.RolePublicationAuthor:
			if check.Err != nil || !check.Off || health.resolved[role] != 1 {
				t.Fatalf("publication author check = %+v", check)
			}
		default:
			if check.Err != nil || check.Off || health.resolved[role] != 1 {
				t.Fatalf("%s check = %+v", role, check)
			}
		}
	}
	if len(driver.Requests()) != 0 || len(ledgerCalls(t, statePath)) != 0 {
		t.Fatal("the standing check made or reserved a call")
	}
	var down *inference.Client
	if down, _ = lookupClient(t, nil, health, budgetedSites()); len(down.CheckRoles(context.Background())) != 0 {
		t.Fatal("a client with no role source checked a role")
	}
}
