package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
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

// TestAdjudicatorInstructionNamesInScopeRoute pins the rules that tell the
// model when required work takes the engine's route. Without them a run's
// model parked a missing decision note that its declared paths covered
// (#1550): it read the note as work beyond the remediation surface.
func TestAdjudicatorInstructionNamesInScopeRoute(t *testing.T) {
	instruction := inference.AdjudicatorSite(inference.Budget{}).Instruction
	for _, rule := range []string{
		// What an allowed input means, and that the remediator's reach is the
		// declared paths rather than the surface.
		"Compatibility allowed means remediation_surface is inside declared_paths",
		"may edit any path declared_paths matches, not only remediation_surface",
		// In-scope required work, including a repository-required file, takes
		// the engine's route.
		"For required work whose fix stays inside declared_paths",
		"adding a file the repository's instructions require",
		"use compatibility:null and route:null",
		// The park compatibilities are for a fix outside the declared paths.
		"Use work_unit_revision_required or separate_work_required only when the fix must write a path outside declared_paths",
		"name that path in rationale",
		// The engine route needs an allowed input.
		"Use compatibility:null with route:null only when that finding's input compatibility is allowed",
	} {
		if !strings.Contains(instruction, rule) {
			t.Errorf("adjudicator instruction lacks %q", rule)
		}
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
	var refusal *inference.AdjudicationUnavailable
	if !errors.As(err, &refusal) || refusal.Cause != inference.AdjudicationOutputRefused {
		t.Fatalf("output refusal cause = %v", err)
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
	if len(requests) != 1 || len(requests[0].Fields) != 15 {
		t.Fatalf("requests = %#v", requests)
	}
	fields := requests[0].Fields
	if fields["approved_spec"] != "approved [REDACTED] specification" ||
		fields["instruction_snapshot"] != "repository [REDACTED] instructions" ||
		!strings.Contains(fields["findings"], "[REDACTED] should never leave") ||
		!strings.Contains(fields["dissent"], "[REDACTED] evidence") ||
		fields["prior_adjudication"] != "null" || fields["diff_metrics"] != "null" ||
		fields["external_findings"] != "null" {
		t.Fatalf("redacted fields = %#v", fields)
	}
	if _, present := fields["implementer_reasoning"]; present {
		t.Fatal("implementer reasoning crossed the allowlist")
	}
	if requests[0].InputDigest != contentaddr.Sum(mustJSON(t, fields)) {
		t.Fatal("input digest does not bind adjudicator fields")
	}
}

func externalAdjudicationFinding() inference.ExternalAdjudicationFinding {
	return inference.ExternalAdjudicationFinding{
		FindingID: "external-1", ReviewerLogin: "maintainer", ThreadID: "review_comment/1",
		HeadSHA:    "head-1",
		Location:   &domain.FindingLocation{Path: "main.go", StartLine: 2, EndLine: 2},
		QuotedText: `"this leaks the handle"`, Notice: "a reviewer's words",
		RemediationSurface: "main.go", Compatibility: domain.CompatibilityAllowed,
	}
}

// TestAdjudicatorAllowlistCarriesExternalFindings pins that an external
// finding crosses the allowlist only as external_findings, exactly as
// supplied, and that the adjudicator's entry for it is accepted like one for a
// finding of Freeside's own.
func TestAdjudicatorAllowlistCarriesExternalFindings(t *testing.T) {
	const output = `{"entries":[` +
		`{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"outside the accepted outcome","evidence":["main.go:1"],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]},` +
		`{"finding_id":"external-1","goal_relationship":"required","compatibility":null,"route":null,"confidence":"high","rationale":"the approved outcome requires the contained fix","evidence":["main.go:2"],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(output), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	input := adjudicatorInput()
	input.ExternalFindings = []inference.ExternalAdjudicationFinding{externalAdjudicationFinding()}
	entries, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].FindingID != "external-1" ||
		entries[1].Producer != domain.AdjudicationProducerEngineModel ||
		entries[1].Route != domain.RouteRemediate {
		t.Fatalf("entries = %#v", entries)
	}
	requests := driver.Requests()
	if len(requests) != 1 ||
		requests[0].Fields["external_findings"] != string(mustJSON(t, input.ExternalFindings)) ||
		strings.Contains(requests[0].Fields["findings"], "external-1") {
		t.Fatalf("requests = %#v", requests)
	}
}

// TestAdjudicatorRefusesExternalFindingAmongFindings pins that a reviewer's
// raw words cannot ride in findings: a finding with external provenance, or
// one named in both lists, is refused before anything reaches the model.
func TestAdjudicatorRefusesExternalFindingAmongFindings(t *testing.T) {
	external := finding("medium")
	external.External = &domain.ExternalFindingProvenance{ReviewerLogin: "maintainer"}
	repeated := externalAdjudicationFinding()
	repeated.FindingID = "finding-1"
	for name, mutate := range map[string]func(*inference.FindingAdjudicationInput){
		"external provenance in findings": func(input *inference.FindingAdjudicationInput) {
			input.Findings[0].Finding = external
		},
		"one finding in both lists": func(input *inference.FindingAdjudicationInput) {
			input.ExternalFindings = []inference.ExternalAdjudicationFinding{repeated}
		},
	} {
		t.Run(name, func(t *testing.T) {
			driver := fake.New()
			driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
				Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
			}})
			client, _, _ := testClient(t, driver, 10)
			input := adjudicatorInput()
			mutate(&input)
			entries, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input)
			if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
				t.Fatalf("AdjudicateFindings = %#v, %v", entries, err)
			}
			if requests := driver.Requests(); len(requests) != 0 {
				t.Fatalf("refused input reached the model: %#v", requests)
			}
			var refusal *inference.AdjudicationUnavailable
			if !errors.As(err, &refusal) || refusal.Cause != inference.AdjudicationInputRefused {
				t.Fatalf("input refusal cause = %v", err)
			}
		})
	}
}

// TestAdjudicatorAllowlistCarriesDiffMetrics pins that the round's recorded
// diff metrics cross the allowlist as supplied; the nil case above is the gap.
func TestAdjudicatorAllowlistCarriesDiffMetrics(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	input := adjudicatorInput()
	input.DiffMetrics = &domain.ReviewRoundDiffMetrics{
		Cumulative: domain.DiffStats{
			FilesChanged: 4, Additions: 120, Deletions: 7, BaseSHA: "base", HeadSHA: "head-2",
		},
		Round: domain.DiffStats{
			FilesChanged: 1, Additions: 20, Deletions: 2, BaseSHA: "head-1", HeadSHA: "head-2",
		},
	}
	if _, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input); err != nil {
		t.Fatal(err)
	}
	requests := driver.Requests()
	if len(requests) != 1 || requests[0].Fields["diff_metrics"] != string(mustJSON(t, input.DiffMetrics)) {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestAdjudicatorAllowlistCarriesBoundConversationFeedback(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
	}})
	client, _, _ := testClient(t, driver, 10)
	input := adjudicatorInput()
	compatibility := domain.ProposedWorkUnitRevision
	prior, err := domain.NewModelAdjudicationEntry(
		"finding-1", domain.GoalRequired, &compatibility,
		domain.RouteParkRevision, domain.ConfidenceHigh, "original recommendation",
		nil, []string{"repository rule"}, []string{"scope is fixed"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.PriorEntries = []domain.FindingAdjudicationEntry{prior}
	input.Feedback = &inference.AdjudicationFeedback{
		InvocationID: "inv-feedback", ConversationID: "conv-feedback", ThroughSequence: 1,
		PrefixDigest:       "sha256:" + domain.Digest(strings.Repeat("d", 64)),
		ConversationPrefix: json.RawMessage(`{"version":"freeside.conversation.prefix/v1","messages":[{"body":"scope correction"}]}`),
		Attachments: []inference.AdjudicationAttachment{{
			Digest:  "sha256:" + domain.Digest(strings.Repeat("e", 64)),
			Content: "token-value attachment evidence",
		}},
	}
	if _, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input); err != nil {
		t.Fatal(err)
	}
	requests := driver.Requests()
	if len(requests) != 1 ||
		!strings.Contains(requests[0].Fields["conversation_feedback"], `"invocation_id":"inv-feedback"`) ||
		!strings.Contains(requests[0].Fields["conversation_feedback"], `"body":"scope correction"`) ||
		!strings.Contains(requests[0].Fields["prior_adjudication"], `"rationale":"original recommendation"`) ||
		!strings.Contains(requests[0].Fields["prior_adjudication"], `"cited_rules":["repository rule"]`) ||
		!strings.Contains(requests[0].Fields["conversation_feedback"], `"content":"[REDACTED] attachment evidence"`) {
		t.Fatalf("conversation feedback = %#v", requests)
	}
}

func TestAdjudicatorRedactsJSONEscapedCredential(t *testing.T) {
	driver := fake.New()
	driver.Script(inference.AdjudicatorSiteID, fake.Script{Response: inference.Response{
		Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 4,
	}})
	redactionValue := strings.Join([]string{"token", `"value<&`, "\n"}, "")
	client, _, _ := testClientWithCredential(t, driver, 10, redactionValue)
	input := adjudicatorInput()
	input.Feedback = &inference.AdjudicationFeedback{
		InvocationID: "inv-feedback", ConversationID: "conv-feedback", ThroughSequence: 1,
		PrefixDigest:       "sha256:" + domain.Digest(strings.Repeat("d", 64)),
		ConversationPrefix: json.RawMessage(`{"version":"freeside.conversation.prefix/v1","messages":[]}`),
		Attachments: []inference.AdjudicationAttachment{{
			Digest: "sha256:" + domain.Digest(strings.Repeat("e", 64)), Content: redactionValue,
		}},
	}
	if _, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", input); err != nil {
		t.Fatal(err)
	}
	field := driver.Requests()[0].Fields["conversation_feedback"]
	encoded, err := json.Marshal(redactionValue)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(encoded[1 : len(encoded)-1])
	if !strings.Contains(field, `"content":"[REDACTED]"`) ||
		strings.Contains(field, redactionValue) || strings.Contains(field, escaped) {
		t.Fatalf("escaped credential was not redacted: %q", field)
	}
}

func TestAdjudicatorExtremeOutputsReturnTypedUnavailable(t *testing.T) {
	oversized := `{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"` +
		strings.Repeat("x", domain.MaxFindingAdjudicationProposalBytes) +
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

// TestAdjudicatorLargestAcceptedOutputRoundTripsPersisted pins the two size
// bounds together (#961): the largest provider output the site accepts, plus an
// engine fast-path entry, builds an artifact that encodes and decodes within the
// persisted bound, and the measured growth stays inside the reserved headroom.
func TestAdjudicatorLargestAcceptedOutputRoundTripsPersisted(t *testing.T) {
	const (
		prefix = `{"entries":[{"finding_id":"finding-1","goal_relationship":"adjacent","compatibility":null,"route":"defer","confidence":"high","rationale":"`
		suffix = `","evidence":[],"cited_rules":[],"assumptions":[],"alternatives":[],"open_questions":[]}]}`
	)
	if got := inference.AdjudicatorSite(testBudget(10)).MaxOutputBytes; got != domain.MaxFindingAdjudicationProposalBytes {
		t.Fatalf("adjudicator MaxOutputBytes = %d, want proposal bound %d", got, domain.MaxFindingAdjudicationProposalBytes)
	}
	outputOfSize := func(size int) []byte {
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}
	adjudicate := func(output []byte) ([]domain.FindingAdjudicationEntry, *advisory.Store, error) {
		driver := fake.New()
		driver.Script(inference.AdjudicatorSiteID,
			fake.Script{Response: inference.Response{Output: output}})
		client, samples, _ := testClientWholeSamples(t, driver, 10)
		entries, err := client.AdjudicateFindings(context.Background(), "project-1", "run-1", adjudicatorInput())
		return entries, samples, err
	}

	output := outputOfSize(domain.MaxFindingAdjudicationProposalBytes)
	entries, samples, err := adjudicate(output)
	if err != nil || len(entries) != 1 {
		t.Fatalf("AdjudicateFindings at proposal bound = %d entries, %v", len(entries), err)
	}
	// The call is sampled, and the sample is the whole output (#2482).
	recorded, err := samples.List(context.Background())
	if err != nil || len(recorded) != 1 || recorded[0].Kind != "audit_sample" || recorded[0].Body != string(output) {
		t.Fatalf("audit samples at proposal bound = %d entries, %v", len(recorded), err)
	}
	allowed := domain.CompatibilityAllowed
	engine, err := domain.NewEngineAdjudicationEntry(
		"finding-2", domain.GoalRequired, &allowed, domain.RouteRemediate,
		"in declared scope", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine entry: %v", err)
	}
	input := adjudicatorInput()
	artifact, err := domain.NewFindingAdjudication(
		input.RunID, input.Round,
		input.ApprovedSpecDigest, input.InstructionSnapshotDigest, input.ResolvedPolicyDigest,
		append(entries, engine), "", time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	body, err := artifact.Encode()
	if err != nil {
		t.Fatalf("encode artifact from largest accepted output: %v", err)
	}
	if _, err := domain.DecodeFindingAdjudication(body); err != nil {
		t.Fatalf("decode artifact from largest accepted output: %v", err)
	}
	if growth := len(body) - len(output); growth >= domain.FindingAdjudicationEnvelopeHeadroomBytes {
		t.Fatalf("output-to-artifact growth = %d bytes, headroom %d", growth, domain.FindingAdjudicationEnvelopeHeadroomBytes)
	}

	entries, _, err = adjudicate(outputOfSize(domain.MaxFindingAdjudicationProposalBytes + 1))
	if !errors.Is(err, inference.ErrAdjudicationNotAvailable) || entries != nil {
		t.Fatalf("AdjudicateFindings one byte over = %#v, %v", entries, err)
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
	var refusal *inference.AdjudicationUnavailable
	if !errors.As(err, &refusal) || refusal.Cause != inference.AdjudicationJudgmentFallback ||
		refusal.Fallback != inference.FallbackDriverAbsent {
		t.Fatalf("judgment fallback cause = %v", err)
	}
	if site := inference.AdjudicatorSite(testBudget(10)); site.FailSafe != `{"entries":[]}` ||
		site.Timeout != 120*time.Second {
		t.Fatalf("fallback site = %#v", site)
	}
}
