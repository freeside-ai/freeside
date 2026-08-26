package inference_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

const acceptedAdjudicatorOutput = `{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"outside the accepted outcome","evidence":["main.go:1"],"cited_rules":["declared scope"],"assumptions":[],"alternatives":["revise the work unit"],"open_questions":["file follow-up work?"]}]}`

const engineAllowedAdjudicatorOutput = `{"entries":[{"finding_id":"finding-1","goal_relationship":"required","compatibility":null,"route":null,"confidence":"high","rationale":"the approved outcome requires the contained fix","evidence":["main.go:1"],"cited_rules":["declared scope"],"assumptions":[],"alternatives":[],"open_questions":[]}]}`

func adjudicatorInput() inference.FindingAdjudicationInput {
	classification := domain.Classification{
		FindingID: "finding-1", Version: 1,
		Materiality: "high", Confidence: "low", Note: "producer=fake/test; ambiguous",
	}
	return inference.FindingAdjudicationInput{
		RunID: "run-1", Round: 1,
		ApprovedSpecDigest:        "sha256:" + domain.Digest(strings.Repeat("a", 64)),
		ApprovedSpecification:     "approved token-value specification",
		InstructionSnapshotDigest: "sha256:" + domain.Digest(strings.Repeat("b", 64)),
		InstructionSnapshot:       "repository token-value instructions",
		ResolvedPolicyDigest:      "sha256:" + domain.Digest(strings.Repeat("c", 64)),
		DeclaredPaths:             []string{"daemon/**"},
		Findings: []inference.AdjudicationFinding{{
			Finding: finding("medium"), Classification: &classification,
			RemediationSurface: "main.go", Compatibility: domain.CompatibilityUnknown,
		}},
		PriorDispositions: []domain.ReviewDispositionRecord{},
		Dissent: &inference.AdjudicationDissent{
			Kind: "remediator_pushback", FindingIDs: []domain.FindingID{"finding-1"},
			Evidence: "token-value evidence",
		},
	}
}

func TestAdjudicatorContractDeclaresCeilingBoundedAllowedFreeLattice(t *testing.T) {
	classifier := inference.ClassifierSite(testBudget(10)).Annotation
	site := inference.AdjudicatorSite(testBudget(10))
	contract := site.Adjudication
	if site.Authority != inference.AuthorityAnnotate || site.Annotation != nil || contract == nil {
		t.Fatalf("site authority contract = %#v", site)
	}
	if slices.Contains(contract.ProposedCompatibilities, string(domain.CompatibilityAllowed)) ||
		slices.Contains(contract.Routes, string(domain.RouteRemediate)) {
		t.Fatalf("model lattice can widen or mint remediation: %#v", contract)
	}
	if !slices.ContainsFunc(contract.Rows, func(row inference.AdjudicationRow) bool {
		return row.GoalRelationship == string(domain.GoalRequired) && row.ProposedCompatibility == nil &&
			row.Route == "" && row.UsesEngineCompatibility
	}) {
		t.Fatalf("engine-composed required row is absent: %#v", contract.Rows)
	}
	if !slices.Equal(contract.ReducesWork,
		[]string{string(domain.RouteDefer), string(domain.RouteDecline)}) {
		t.Fatalf("work-reducing routes = %#v", contract.ReducesWork)
	}
	if !slices.Equal(contract.SeverityMappings, classifier.SeverityMappings) ||
		contract.UnknownSeverityFallback != classifier.UnknownSeverityFallback ||
		!slices.Equal(contract.NormalizedSeverityCeilings, classifier.NormalizedSeverityCeilings) ||
		!slices.Equal(contract.SecondAdjudicationRules, classifier.SecondAdjudicationRules) {
		t.Fatal("adjudicator severity ceilings diverge from classifier")
	}
}

func TestAdjudicatorComposesModelGoalWithEngineAllowedCompatibility(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(engineAllowedAdjudicatorOutput), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	input := adjudicatorInput()
	input.Findings[0].Compatibility = domain.CompatibilityAllowed
	entries, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Producer != domain.AdjudicationProducerEngineModel ||
		entries[0].Compatibility == nil || *entries[0].Compatibility != domain.CompatibilityAllowed ||
		entries[0].Route != domain.RouteRemediate || entries[0].Confidence == nil ||
		*entries[0].Confidence != domain.ConfidenceHigh {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestAdjudicatorRejectsEngineCompositionWithoutAllowedInput(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(engineAllowedAdjudicatorOutput), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	entries, err := client.AdjudicateFindings(
		context.Background(), "project-1", "run-1", adjudicatorInput())
	if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
		t.Fatalf("AdjudicateFindings = %#v, %v", entries, err)
	}
}

func TestAdjudicatorAllowlistConvertsValidatedProposal(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	entries, err := client.AdjudicateFindings(
		context.Background(), "project-1", "run-1", adjudicatorInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Producer != domain.AdjudicationProducerModel ||
		entries[0].Route != domain.RouteDefer || entries[0].Confidence == nil ||
		*entries[0].Confidence != domain.ConfidenceHigh {
		t.Fatalf("entries = %#v", entries)
	}
	requests := driver.Requests()
	if len(requests) != 1 || len(requests[0].Fields) != 11 {
		t.Fatalf("requests = %#v", requests)
	}
	fields := requests[0].Fields
	if fields["approved_spec"] != "approved [REDACTED] specification" ||
		fields["instruction_snapshot"] != "repository [REDACTED] instructions" ||
		!strings.Contains(fields["findings"], "[REDACTED] should never leave") ||
		!strings.Contains(fields["dissent"], "[REDACTED] evidence") {
		t.Fatalf("redacted fields = %#v", fields)
	}
	if _, present := fields["implementer_reasoning"]; present {
		t.Fatal("implementer reasoning crossed the allowlist")
	}
	if requests[0].InputDigest != contentaddr.Sum(mustJSON(t, fields)) {
		t.Fatal("input digest does not bind adjudicator fields")
	}
}

func TestAdjudicatorExtremeOutputsReturnTypedUnavailable(t *testing.T) {
	oversized := `{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"` +
		strings.Repeat("x", domain.MaxFindingAdjudicationBytes) +
		`","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`
	cases := map[string][]byte{
		"minted allowed":            []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"required","compatibility":"allowed","route":"remediate","confidence":"high","rationale":"widen","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"minted remediate":          []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"required","compatibility":null,"route":"remediate","confidence":"high","rationale":"widen","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"unknown goal":              []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"optional","compatibility":null,"route":"defer","confidence":"high","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"unknown route":             []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"ignore","confidence":"high","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"unknown confidence":        []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"certain","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"empty rationale":           []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":" ","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"omitted compatibility":     []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","route":"defer","confidence":"high","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"omitted route":             []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"confidence":"high","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`),
		"omitted explanation field": []byte(`{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"bad","evidence":[],"cited_rules":[],"assumptions":[],"open_questions":[]}]}`),
		"duplicate key":             []byte(`{"entries":[],"entries":[]}`),
		"unknown field":             []byte(`{"entries":[],"workspace":"/tmp/repo"}`),
		"oversized":                 []byte(oversized),
		"missing entries":           []byte(`{}`),
		"invalid utf8":              append([]byte(`{"entries":[]}`), 0xff),
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			driver := fake.New()
			driver.Script(inference.AdjudicatorSiteID,
				fake.Script{Response: inference.Response{Output: output}})
			client, _, _ := testClient(t, driver, 10)
			entries, err := client.AdjudicateFindings(
				context.Background(), "project-1", "run-1", adjudicatorInput())
			if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
				t.Fatalf("AdjudicateFindings = %#v, %v", entries, err)
			}
		})
	}
}

func TestClassifierAndAdjudicatorShareRootBudget(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.ClassifierSiteID, fake.Script{Response: inference.Response{
		Output:       []byte(`{"materiality":"high","confidence":"high","note":"required"}`),
		ComputeUnits: 1,
	}})
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1,
	}})
	client, _, _ := testClient(t, driver, 1)
	if _, err := client.ClassifyFinding(
		context.Background(), "project-1", "run-1", finding("medium"), 1); err != nil {
		t.Fatal(err)
	}
	entries, err := client.AdjudicateFindings(
		context.Background(), "project-1", "run-1", adjudicatorInput())
	if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
		t.Fatalf("second site call = %#v, %v", entries, err)
	}
	if got := len(driver.Requests()); got != 1 {
		t.Fatalf("provider calls = %d, want one cumulative-root-bounded call", got)
	}
}

func TestAdjudicatorRepeatedCallsStopAtCumulativeBound(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID,
		fake.Script{Response: inference.Response{Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1}},
		fake.Script{Response: inference.Response{Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1}},
		fake.Script{Response: inference.Response{Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1}},
	)
	client, _, _ := testClient(t, driver, 2)
	for call := 1; call <= 3; call++ {
		entries, err := client.AdjudicateFindings(
			context.Background(), "project-1", "run-1", adjudicatorInput())
		if call <= 2 && (err != nil || len(entries) != 1) {
			t.Fatalf("call %d = %#v, %v", call, entries, err)
		}
		if call == 3 && (!errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil) {
			t.Fatalf("bounded call = %#v, %v", entries, err)
		}
	}
	if got := len(driver.Requests()); got != 2 {
		t.Fatalf("provider calls = %d, want two bounded calls", got)
	}
}

func TestAdjudicatorDeterministicFallbackIsSchemaValidEmptySet(t *testing.T) {
	client, _, _ := testClient(t, nil, 10)
	entries, err := client.AdjudicateFindings(
		context.Background(), "project-1", "run-1", adjudicatorInput())
	if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
		t.Fatalf("inference-down fallback = %#v, %v", entries, err)
	}
	if site := inference.AdjudicatorSite(testBudget(10)); site.FailSafe != `{"entries":[]}` ||
		site.Timeout != 30*time.Second {
		t.Fatalf("fallback site = %#v", site)
	}
}
